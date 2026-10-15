package derived

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/stretchr/testify/require"
)

func TestKey(t *testing.T) {
	t.Parallel()

	commit := git.ObjectID("0123456789abcdef0123456789abcdef01234567")

	key := Key("lastmod", commit, "dir/file")
	require.Len(t, key, 64)
	require.Equal(t, key, Key("lastmod", commit, "dir/file"), "keys must be deterministic")

	require.NotEqual(t, key, Key("blame", commit, "dir/file"), "kind is part of the key")
	require.NotEqual(t, key, Key("lastmod", commit, "dir/other"), "path is part of the key")
	require.NotEqual(t, key, Key("lastmod", "fedcba9876543210fedcba9876543210fedcba98", "dir/file"), "commit is part of the key")

	// Fields are separated, so concatenations cannot collide.
	require.NotEqual(t, Key("lastmod", "ab", "c"), Key("lastmod", "a", "bc"))
}

func TestStore_PutGet(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	store := NewStore(repoPath)
	key := Key("lastmod", "0123456789abcdef0123456789abcdef01234567", "a")

	_, found, err := store.Get("lastmod", key)
	require.NoError(t, err)
	require.False(t, found)

	require.NoError(t, store.Put("lastmod", key, []byte("value")))

	value, found, err := store.Get("lastmod", key)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "value", string(value))

	// The layout is <repo>/graft/derived/<kind>/<first two hex>/<key>.
	require.FileExists(t, filepath.Join(repoPath, "graft", "derived", "lastmod", key[:2], key))
}

func TestStore_EmptyValueIsStored(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	key := Key("lastmod", "0123456789abcdef0123456789abcdef01234567", "missing")

	require.NoError(t, store.Put("lastmod", key, nil))

	value, found, err := store.Get("lastmod", key)
	require.NoError(t, err)
	require.True(t, found, "an empty value is a valid answer and must be remembered")
	require.Empty(t, value)
}

func TestStore_PutReplacesEntry(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	key := Key("blame", "0123456789abcdef0123456789abcdef01234567", "a")

	require.NoError(t, store.Put("blame", key, []byte("garbage")))
	require.NoError(t, store.Put("blame", key, []byte("valid")))

	value, found, err := store.Get("blame", key)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "valid", string(value), "a corrupt entry must be replaced by a later write")
}

func TestStore_NoTemporaryFilesLeft(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	store := NewStore(repoPath)
	key := Key("lastmod", "0123456789abcdef0123456789abcdef01234567", "a")
	require.NoError(t, store.Put("lastmod", key, []byte("value")))

	entries, err := os.ReadDir(filepath.Join(repoPath, "graft", "derived", "lastmod", key[:2]))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, key, entries[0].Name())
}

func TestStore_Validation(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	validKey := Key("lastmod", "0123456789abcdef0123456789abcdef01234567", "a")

	for _, tc := range []struct {
		desc string
		kind string
		key  string
	}{
		{desc: "empty kind", kind: "", key: validKey},
		{desc: "kind with path separator", kind: "../x", key: validKey},
		{desc: "kind with upper case", kind: "LastMod", key: validKey},
		{desc: "kind with digits", kind: "lastmod2", key: validKey},
		{desc: "short key", kind: "lastmod", key: "abc"},
		{desc: "key with path separator", kind: "lastmod", key: "../" + validKey[3:]},
		{desc: "key with upper case hex", kind: "lastmod", key: "ABCDEF" + validKey[6:]},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			_, _, err := store.Get(tc.kind, tc.key)
			require.Error(t, err)
			require.Error(t, store.Put(tc.kind, tc.key, []byte("x")))
		})
	}
}

func TestStore_NilStoreIsEmpty(t *testing.T) {
	t.Parallel()

	var store *Store
	key := Key("lastmod", "0123456789abcdef0123456789abcdef01234567", "a")

	_, found, err := store.Get("lastmod", key)
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, store.Put("lastmod", key, []byte("discarded")))

	_, found, err = store.Get("lastmod", key)
	require.NoError(t, err)
	require.False(t, found, "a nil store discards writes")
}

func TestStore_UnreadableDirectory(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	// A regular file where the store directory should be makes every write fail.
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "graft"), []byte("not a directory"), 0o644))

	store := NewStore(repoPath)
	key := Key("lastmod", "0123456789abcdef0123456789abcdef01234567", "a")
	require.Error(t, store.Put("lastmod", key, []byte("value")))
}

func TestStore_ConcurrentWritersAndReaders(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	key := Key("blame", "0123456789abcdef0123456789abcdef01234567", "a")
	value := []byte("the value")

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := store.Put("blame", key, value); err != nil {
				errs <- fmt.Errorf("put: %w", err)
			}
		}()
		go func() {
			defer wg.Done()
			got, found, err := store.Get("blame", key)
			if err != nil {
				errs <- fmt.Errorf("get: %w", err)
				return
			}
			// A reader sees either nothing or the complete value, never a partial write.
			if found && string(got) != string(value) {
				errs <- fmt.Errorf("partial value %q", got)
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	got, found, err := store.Get("blame", key)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, value, got)
}
