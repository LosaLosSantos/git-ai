package mvcc

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/feder-cr/open_scion/internal/gitaly/storage"
)

// ErrCacheFactoryClosed is returned by NewCache when the CacheFactory has
// already been closed. Its ObjectStorages have been torn down at that point, so
// no new cache can be handed out.
var ErrCacheFactoryClosed = errors.New("cache factory is closed")

// CacheFactory creates the per-run LocalCaches used by every MVCC consumer (the
// mvcc gRPC middleware for RPCs, the housekeeping manager for background
// optimizations) so that all of them share a single ObjectStorage per storage.
//
// Pooling the ObjectStorage per storage path is a stop-gap: ideally there would
// be one shared ObjectStorage for the whole Gitaly process, but Gitaly has the
// concept of `Storages`, which are different locations on disk where a
// repository can be stored. Once the `Storages` concept becomes obsolete, the
// pool can be replaced by a singleton created at process start-up.
type CacheFactory struct {
	// runtimeDir is the runtime directory of Gitaly, where the caches stage
	// the per-run manifest pointer files of mutator runs.
	runtimeDir string
	// locator resolves a storage name into the storage's path on disk.
	locator storage.Locator

	// cacheMetrics is shared by every LocalCache the factory creates. Embedding it
	// promotes its Describe and Collect methods so the factory itself implements
	// prometheus.Collector.
	*cacheMetrics

	mu       sync.Mutex
	storages map[string]ObjectStorage
	closed   bool
}

// NewCacheFactory returns a CacheFactory staging per-run state in runtimeDir and
// resolving storage paths via locator.
func NewCacheFactory(runtimeDir string, locator storage.Locator) *CacheFactory {
	return &CacheFactory{
		runtimeDir:   runtimeDir,
		locator:      locator,
		cacheMetrics: newCacheMetrics(),
		storages:     make(map[string]ObjectStorage),
	}
}

// NewCache returns a new LocalCache for the repository identified by
// repositoryID on the storage named storageName. Caches created for the same
// storage share one ObjectStorage instance. It returns ErrCacheFactoryClosed
// once Close has run.
func (f *CacheFactory) NewCache(ctx context.Context, storageName, repositoryID string, readOnly bool) (*LocalCache, error) {
	storagePath, err := f.locator.GetStorageByName(ctx, storageName)
	if err != nil {
		return nil, fmt.Errorf("get storage path: %w", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil, ErrCacheFactoryClosed
	}

	objectStorage, ok := f.storages[storagePath]
	if !ok {
		objectStorage = NewLocalStorage(storagePath)
		f.storages[storagePath] = objectStorage
	}

	cache := NewLocalCache(objectStorage, f.runtimeDir, storagePath, repositoryID, readOnly)
	cache.metrics = f.cacheMetrics
	return cache, nil
}

// Close tears down every ObjectStorage the factory has created. All storages
// are torn down even if some of them fail; the returned error then joins one
// error per storage path that could not be cleaned up.
//
// The factory is closed for good afterwards: subsequent NewCache calls fail
// with ErrCacheFactoryClosed rather than handing out a cache backed by an
// already torn down ObjectStorage. Close is idempotent and does not tear down
// the same ObjectStorage twice.
func (f *CacheFactory) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil
	}
	f.closed = true

	var errs []error
	for storagePath, objectStorage := range f.storages {
		if err := objectStorage.TearDown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("cache-factory: teardown of storage %q failed: %w", storagePath, err))
		}
	}
	clear(f.storages)

	return errors.Join(errs...)
}
