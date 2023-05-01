package cache

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/moby/buildkit/cache"
	cacheconfig "github.com/moby/buildkit/cache/config"
	remotecache "github.com/moby/buildkit/cache/remotecache/v1"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/mounts"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/compression"
	"github.com/moby/buildkit/worker"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

type manager struct {
	ManagerConfig
	cacheClient   Service
	httpClient    *http.Client
	layerProvider content.Provider
	runtimeConfig Config
	localCache    solver.CacheManager

	mu                 sync.RWMutex
	inner              solver.CacheManager
	startCloseCh       chan struct{} // closed when shutdown should start
	doneCh             chan struct{} // closed when shutdown is complete
	stopCacheMountSync func(context.Context) error
}

type ManagerConfig struct {
	KeyStore     solver.CacheKeyStorage
	ResultStore  solver.CacheResultStorage
	Worker       worker.Worker
	MountManager *mounts.MountManager
	ServiceURL   string
	EngineID     string
}

func NewManager(ctx context.Context, managerConfig ManagerConfig) (Manager, error) {
	localCache := solver.NewCacheManager(ctx, "local", managerConfig.KeyStore, managerConfig.ResultStore)
	m := &manager{
		ManagerConfig: managerConfig,
		localCache:    localCache,
		startCloseCh:  make(chan struct{}),
		doneCh:        make(chan struct{}),
		httpClient:    &http.Client{},
	}

	if managerConfig.ServiceURL == "" {
		return defaultCacheManager{m.localCache}, nil
	}
	bklog.G(ctx).Debugf("using cache service at %s", managerConfig.ServiceURL)

	serviceClient, err := newClient(managerConfig.ServiceURL)
	if err != nil {
		return nil, err
	}
	m.cacheClient = serviceClient
	m.layerProvider = &layerProvider{
		httpClient:  m.httpClient,
		cacheClient: m.cacheClient,
	}

	config, err := m.cacheClient.GetConfig(ctx, GetConfigRequest{
		EngineID: m.EngineID,
	})
	if err != nil {
		return nil, err
	}
	if config.ImportPeriod == 0 || config.ExportPeriod == 0 || config.ExportTimeout == 0 {
		return nil, fmt.Errorf("invalid cache config: import/export periods must be non-zero")
	}
	m.runtimeConfig = *config

	// do an initial synchronous import at start
	// TODO: make this non-fatal (but ensure no inconsistent state in failure case)
	if err := m.Import(ctx); err != nil {
		return nil, err
	}
	// loop for periodic async imports
	importParentCtx, cancelImport := context.WithCancel(context.Background())
	go func() {
		<-m.startCloseCh
		cancelImport()
	}()
	go func() {
		for {
			select {
			case <-time.After(config.ImportPeriod):
			case <-m.startCloseCh:
				return
			}
			importContext, cancel := context.WithTimeout(importParentCtx, time.Minute)
			if err := m.Import(importContext); err != nil {
				bklog.G(ctx).WithError(err).Error("failed to import cache")
			}
			cancel()
		}
	}()

	// loop for periodic async exports
	go func() {
		defer close(m.doneCh)
		var shutdown bool
		for {
			select {
			case <-time.After(config.ExportPeriod):
			case <-m.startCloseCh:
				shutdown = true
				// always run a final export before shutdown
			}
			exportCtx, cancel := context.WithTimeout(context.Background(), config.ExportTimeout)
			defer cancel()
			if err := m.Export(exportCtx); err != nil {
				bklog.G(ctx).WithError(err).Error("failed to export cache")
			}
			if shutdown {
				return
			}
		}
	}()

	return m, nil
}

func (m *manager) Export(ctx context.Context) error {
	var cacheKeys []CacheKey
	var links []Link

	err := m.KeyStore.Walk(func(id string) error {
		cacheKey := CacheKey{ID: id}
		err := m.KeyStore.WalkBacklinks(id, func(linkedID string, linkInfo solver.CacheInfoLink) error {
			link := Link{
				ID:       id,
				LinkedID: linkedID,
				Input:    int(linkInfo.Input),
				Digest:   linkInfo.Digest,
				Selector: linkInfo.Selector,
			}
			links = append(links, link)
			return nil
		})
		if err != nil {
			return err
		}
		err = m.KeyStore.WalkResults(id, func(cacheResult solver.CacheResult) error {
			res, err := m.ResultStore.Load(ctx, cacheResult)
			if err != nil {
				// the ref may be lazy or pruned, just skip it
				bklog.G(ctx).Debugf("skipping cache result %s for %s: %v", cacheResult.ID, id, err)
				return nil
			}
			defer res.Release(context.Background()) // TODO: hold on until later export?
			workerRef, ok := res.Sys().(*worker.WorkerRef)
			if !ok {
				bklog.G(ctx).Debugf("skipping cache result %s for %s: not an immutable ref", cacheResult.ID, id)
				return nil
			}
			cacheRef := workerRef.ImmutableRef
			cacheKey.Results = append(cacheKey.Results, Result{
				ID:          cacheRef.ID(),
				CreatedAt:   cacheResult.CreatedAt,
				Description: cacheRef.GetDescription(),
			})
			return nil
		})
		if err != nil {
			return err
		}
		cacheKeys = append(cacheKeys, cacheKey)
		return nil
	})
	if err != nil {
		return err
	}

	updateCacheRecordsResp, err := m.cacheClient.UpdateCacheRecords(ctx, UpdateCacheRecordsRequest{
		CacheKeys: cacheKeys,
		Links:     links,
	})
	if err != nil {
		return err
	}
	recordsToExport := updateCacheRecordsResp.ExportRecords
	if len(recordsToExport) == 0 {
		return nil
	}

	updatedRecords := make([]RecordLayers, 0, len(recordsToExport))
	for _, record := range recordsToExport {
		if err := func() error {
			cacheRef, err := m.Worker.CacheManager().Get(ctx, record.CacheRefID, nil, cache.NoUpdateLastUsed)
			if err != nil {
				// the ref may be lazy or pruned, just skip it
				bklog.G(ctx).Debugf("skipping cache ref for export %s: %v", record.CacheRefID, err)
				return nil
			}
			defer cacheRef.Release(context.Background())
			remotes, err := cacheRef.GetRemotes(ctx, true, cacheconfig.RefConfig{
				Compression: compression.Config{
					Type: compression.Zstd,
				},
			}, false, nil)
			if err != nil {
				return err
			}
			if len(remotes) == 0 {
				bklog.G(ctx).Errorf("skipping cache ref for export %s: no remotes", record.CacheRefID)
				return nil
			}
			if len(remotes) > 1 {
				bklog.G(ctx).Debugf("multiple remotes for cache ref %s, using the first one", record.CacheRefID)
			}
			remote := remotes[0]
			for _, layer := range remote.Descriptors {
				if err := m.pushLayer(ctx, layer, remote.Provider); err != nil {
					return err
				}
			}
			updatedRecords = append(updatedRecords, RecordLayers{
				RecordDigest: record.Digest,
				Layers:       remote.Descriptors,
			})
			return nil
		}(); err != nil {
			return err
		}
	}

	if err := m.cacheClient.UpdateCacheLayers(ctx, UpdateCacheLayersRequest{
		UpdatedRecords: updatedRecords,
	}); err != nil {
		return err
	}

	return nil
}

func (m *manager) pushLayer(ctx context.Context, layerDesc ocispecs.Descriptor, provider content.Provider) error {
	getURLResp, err := m.cacheClient.GetLayerUploadURL(ctx, GetLayerUploadURLRequest{Digest: layerDesc.Digest})
	if err != nil {
		return err
	}

	readerAt, err := provider.ReaderAt(ctx, layerDesc)
	if err != nil {
		return err
	}
	defer readerAt.Close()
	reader := content.NewReader(readerAt)

	req, err := http.NewRequest("PUT", getURLResp.URL, reader)
	if err != nil {
		return err
	}
	defer req.Body.Close()
	req.ContentLength = readerAt.Size()

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}
	return nil
}

func (m *manager) Import(ctx context.Context) error {
	cacheConfig, err := m.cacheClient.ImportCache(ctx)
	if err != nil {
		return err
	}

	descProvider := remotecache.DescriptorProvider{}
	for _, layer := range cacheConfig.Layers {
		providerPair, err := m.descriptorProviderPair(layer)
		if err != nil {
			return err
		}
		descProvider[layer.Blob] = *providerPair
	}

	chain := remotecache.NewCacheChains()
	if err := remotecache.ParseConfig(*cacheConfig, descProvider, chain); err != nil {
		return err
	}

	keyStore, resultStore, err := remotecache.NewCacheKeyStorage(chain, m.Worker)
	if err != nil {
		return err
	}
	importedCache := solver.NewCacheManager(ctx, m.ID(), keyStore, resultStore)
	newInner := solver.NewCombinedCacheManager([]solver.CacheManager{importedCache}, m.localCache)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.inner = newInner
	return nil
}

// Close will block until the final export has finished or ctx is canceled.
func (m *manager) Close(ctx context.Context) (rerr error) {
	close(m.startCloseCh)
	if m.stopCacheMountSync != nil {
		rerr = m.stopCacheMountSync(ctx)
	}
	select {
	case <-m.doneCh:
	case <-ctx.Done():
	}
	return rerr
}

func (m *manager) ID() string {
	return "enginecache"
}

func (m *manager) Query(inp []solver.CacheKeyWithSelector, inputIndex solver.Index, dgst digest.Digest, outputIndex solver.Index) ([]*solver.CacheKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.inner.Query(inp, inputIndex, dgst, outputIndex)
}

func (m *manager) Records(ctx context.Context, ck *solver.CacheKey) ([]*solver.CacheRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.inner.Records(ctx, ck)
}

func (m *manager) Load(ctx context.Context, rec *solver.CacheRecord) (solver.Result, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.inner.Load(ctx, rec)
}

func (m *manager) Save(key *solver.CacheKey, s solver.Result, createdAt time.Time) (*solver.ExportableCacheKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inner.Save(key, s, createdAt)
}

func (m *manager) descriptorProviderPair(layerMetadata remotecache.CacheLayer) (*remotecache.DescriptorProviderPair, error) {
	if layerMetadata.Annotations == nil {
		return nil, fmt.Errorf("missing annotations for layer %s", layerMetadata.Blob)
	}

	annotations := map[string]string{}
	if layerMetadata.Annotations.DiffID == "" {
		return nil, fmt.Errorf("missing diffID for layer %s", layerMetadata.Blob)
	}
	annotations["containerd.io/uncompressed"] = layerMetadata.Annotations.DiffID.String()
	if !layerMetadata.Annotations.CreatedAt.IsZero() {
		createdAt, err := layerMetadata.Annotations.CreatedAt.MarshalText()
		if err != nil {
			return nil, err
		}
		annotations["buildkit/createdat"] = string(createdAt)
	}
	desc := ocispecs.Descriptor{
		MediaType:   layerMetadata.Annotations.MediaType,
		Digest:      layerMetadata.Blob,
		Size:        layerMetadata.Annotations.Size,
		Annotations: annotations,
	}
	return &remotecache.DescriptorProviderPair{
		Provider:   m.layerProvider,
		Descriptor: desc,
	}, nil
}

type Manager interface {
	solver.CacheManager
	StartCacheMountSynchronization(context.Context) error
	Close(context.Context) error
}

type defaultCacheManager struct {
	solver.CacheManager
}

var _ Manager = defaultCacheManager{}

func (defaultCacheManager) StartCacheMountSynchronization(ctx context.Context) error {
	return nil
}

func (defaultCacheManager) Close(context.Context) error {
	return nil
}
