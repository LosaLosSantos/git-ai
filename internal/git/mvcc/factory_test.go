package mvcc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// stubLocator resolves storage names to paths and fails for unknown names. Only
// GetStorageByName is used by the CacheFactory, so the remaining methods of the
// embedded interface are never called.
type stubLocator struct {
	storage.Locator
	paths map[string]string
}

func (l stubLocator) GetStorageByName(_ context.Context, storageName string) (string, error) {
	path, ok := l.paths[storageName]
	if !ok {
		return "", fmt.Errorf("no such storage: %q", storageName)
	}

	return path, nil
}

func TestCacheFactory(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	factory := NewCacheFactory("/runtime", stubLocator{paths: map[string]string{
		"storage-a": "/storage/a",
		"storage-b": "/storage/b",
	}})

	a1, err := factory.NewCache(ctx, "storage-a", "repo-1", false)
	require.NoError(t, err)
	a2, err := factory.NewCache(ctx, "storage-a", "repo-2", true)
	require.NoError(t, err)
	b, err := factory.NewCache(ctx, "storage-b", "repo-1", false)
	require.NoError(t, err)

	require.NotSame(t, a1, a2, "each call must return a fresh per-run cache")
	require.Same(t, a1.rs, a2.rs, "caches on the same storage must share one ObjectStorage")
	require.NotSame(t, a1.rs, b.rs, "caches on different storages must not share an ObjectStorage")

	require.Equal(t, "/runtime", a1.runtimeDir)
	require.Equal(t, "/storage/a", a1.storagePath)
	require.Equal(t, "repo-2", a2.repositoryID)
	require.False(t, a1.readOnly)
	require.True(t, a2.readOnly)

	_, err = factory.NewCache(ctx, "unknown", "repo-1", false)
	require.Error(t, err)
}

// stubObjectStorage records the TearDown calls it received and returns
// tearDownErr. Only TearDown is used by CacheFactory.Close, so the remaining
// methods of the embedded interface are never called.
type stubObjectStorage struct {
	ObjectStorage
	tearDownErr   error
	tearDownCalls int
}

func (s *stubObjectStorage) TearDown(context.Context) error {
	s.tearDownCalls++
	return s.tearDownErr
}

func TestCacheFactory_Close(t *testing.T) {
	t.Parallel()

	t.Run("no storages", func(t *testing.T) {
		t.Parallel()

		factory := NewCacheFactory("/runtime", stubLocator{})

		require.NoError(t, factory.Close(testhelper.Context(t)))
	})

	t.Run("all storages tear down successfully", func(t *testing.T) {
		t.Parallel()

		factory := NewCacheFactory("/runtime", stubLocator{})
		a := &stubObjectStorage{}
		b := &stubObjectStorage{}
		factory.storages["/storage/a"] = a
		factory.storages["/storage/b"] = b

		require.NoError(t, factory.Close(testhelper.Context(t)))
		require.Equal(t, 1, a.tearDownCalls)
		require.Equal(t, 1, b.tearDownCalls)
	})

	t.Run("single storage fails to tear down", func(t *testing.T) {
		t.Parallel()

		tearDownErr := errors.New("drain failed")
		factory := NewCacheFactory("/runtime", stubLocator{})
		factory.storages["/storage/a"] = &stubObjectStorage{tearDownErr: tearDownErr}

		err := factory.Close(testhelper.Context(t))
		require.EqualError(t, err, `cache-factory: teardown of storage "/storage/a" failed: drain failed`)
		require.ErrorIs(t, err, tearDownErr, "the error of the storage must stay inspectable")
	})

	t.Run("only failing storages are reported", func(t *testing.T) {
		t.Parallel()

		factory := NewCacheFactory("/runtime", stubLocator{})
		failing := &stubObjectStorage{tearDownErr: errors.New("drain failed")}
		succeeding := &stubObjectStorage{}
		factory.storages["/storage/a"] = failing
		factory.storages["/storage/b"] = succeeding

		require.EqualError(t, factory.Close(testhelper.Context(t)),
			`cache-factory: teardown of storage "/storage/a" failed: drain failed`)
		require.Equal(t, 1, failing.tearDownCalls)
		require.Equal(t, 1, succeeding.tearDownCalls,
			"a failing storage must not stop the remaining storages from being torn down")
	})

	t.Run("multiple storages fail to tear down", func(t *testing.T) {
		t.Parallel()

		tearDownErrA := errors.New("drain a failed")
		tearDownErrB := errors.New("drain b failed")
		factory := NewCacheFactory("/runtime", stubLocator{})
		factory.storages["/storage/a"] = &stubObjectStorage{tearDownErr: tearDownErrA}
		factory.storages["/storage/b"] = &stubObjectStorage{tearDownErr: tearDownErrB}

		err := factory.Close(testhelper.Context(t))
		require.ErrorIs(t, err, tearDownErrA)
		require.ErrorIs(t, err, tearDownErrB)

		// The storages are held in a map, so the order in which they are
		// reported is not deterministic. Assert on the unordered set of
		// joined errors instead.
		require.ElementsMatch(t,
			[]string{
				`cache-factory: teardown of storage "/storage/a" failed: drain a failed`,
				`cache-factory: teardown of storage "/storage/b" failed: drain b failed`,
			},
			strings.Split(err.Error(), "\n"),
		)
	})

	t.Run("closing twice tears the storages down once", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		factory := NewCacheFactory("/runtime", stubLocator{})
		a := &stubObjectStorage{}
		factory.storages["/storage/a"] = a

		require.NoError(t, factory.Close(ctx))
		require.NoError(t, factory.Close(ctx))
		require.Equal(t, 1, a.tearDownCalls)
	})

	t.Run("new caches are rejected after close", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		factory := NewCacheFactory("/runtime", stubLocator{paths: map[string]string{
			"storage-a": "/storage/a",
		}})

		cache, err := factory.NewCache(ctx, "storage-a", "repo-1", false)
		require.NoError(t, err)
		require.NotNil(t, cache)

		require.NoError(t, factory.Close(ctx))

		// Without this, the factory would hand out a cache backed by an
		// already torn down ObjectStorage.
		_, err = factory.NewCache(ctx, "storage-a", "repo-1", false)
		require.ErrorIs(t, err, ErrCacheFactoryClosed)
		require.Empty(t, factory.storages, "torn down storages must not be retained")
	})

	t.Run("storages failing to tear down are dropped as well", func(t *testing.T) {
		t.Parallel()

		factory := NewCacheFactory("/runtime", stubLocator{})
		factory.storages["/storage/a"] = &stubObjectStorage{tearDownErr: errors.New("drain failed")}

		require.Error(t, factory.Close(testhelper.Context(t)))
		require.Empty(t, factory.storages)
	})
}
