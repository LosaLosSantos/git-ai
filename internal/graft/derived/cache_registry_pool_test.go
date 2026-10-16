package derived

import (
	"context"
	"errors"
	"testing"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// jsonKey is a valid store key, as the store only accepts keys of the derived form.
var jsonKey = Key("fake-json", fakeOID(7), "path")

func TestCachedJSON_ComputesOnceAndReadsStore(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	store := NewStore(t.TempDir())
	logger := testhelper.NewLogger(t)

	var runs int
	compute := func(context.Context) ([]int, error) {
		runs++
		return []int{1, 2, 3}, nil
	}

	got, err := cachedJSON(ctx, store, logger, "fake-json", jsonKey, compute)
	require.NoError(t, err)
	require.Equal(t, []int{1, 2, 3}, got)

	got, err = cachedJSON(ctx, store, logger, "fake-json", jsonKey, compute)
	require.NoError(t, err)
	require.Equal(t, []int{1, 2, 3}, got)
	require.Equal(t, 1, runs, "the second call is answered from the store")
}

func TestCachedJSON_RecomputesCorruptEntry(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	store := NewStore(t.TempDir())
	logger := testhelper.NewLogger(t)
	require.NoError(t, store.Put("fake-json", jsonKey, []byte("not json")))

	got, err := cachedJSON(ctx, store, logger, "fake-json", jsonKey, func(context.Context) (string, error) {
		return "fresh", nil
	})
	require.NoError(t, err)
	require.Equal(t, "fresh", got)

	again, err := cachedJSON(ctx, store, logger, "fake-json", jsonKey, func(context.Context) (string, error) {
		return "unused", nil
	})
	require.NoError(t, err)
	require.Equal(t, "fresh", again, "the corrupt entry was replaced")
}

func TestCachedJSON_ComputeErrorStoresNothing(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	store := NewStore(t.TempDir())
	computeErr := errors.New("compute failed")

	_, err := cachedJSON(ctx, store, testhelper.NewLogger(t), "fake-json", jsonKey, func(context.Context) (int, error) {
		return 0, computeErr
	})
	require.ErrorIs(t, err, computeErr)

	_, found, err := store.Get("fake-json", jsonKey)
	require.NoError(t, err)
	require.False(t, found)
}

func TestNewRegistry_RejectsInvalidAndDuplicateNames(t *testing.T) {
	t.Parallel()

	_, err := NewRegistry("Bad_Name")
	require.Error(t, err)

	_, err = NewRegistry("lastmod", "lastmod")
	require.Error(t, err)
}

func TestRegistry_SelectUsesRegistrationOrder(t *testing.T) {
	t.Parallel()

	registry, err := NewRegistry("alpha", "beta", "gamma")
	require.NoError(t, err)

	all, err := registry.Select(nil)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "beta", "gamma"}, all)

	selected, err := registry.Select([]string{"gamma", "alpha"})
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "gamma"}, selected)

	_, err = registry.Select([]string{"delta"})
	require.Error(t, err, "unknown kinds are never silently ignored")
}

func TestDefaultRegistry_ListsEveryKind(t *testing.T) {
	t.Parallel()

	require.Equal(t,
		[]string{kindLastMod, kindLastModFirstParent, kindBlameOrigin, kindBlame},
		DefaultRegistry().Names(),
	)
}
