package verify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadStateMissingFileIsInitial(t *testing.T) {
	st, err := loadState(filepath.Join(t.TempDir(), "state.json"))
	require.NoError(t, err)
	require.Equal(t, State{Version: stateVersion, Cursor: 0}, st)
}

func TestSaveAndLoadState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")

	require.NoError(t, saveState(path, State{Cursor: 41}))
	st, err := loadState(path)
	require.NoError(t, err)
	require.Equal(t, uint64(41), st.Cursor)

	// A later save replaces the earlier state.
	require.NoError(t, saveState(path, State{Cursor: 42}))
	st, err = loadState(path)
	require.NoError(t, err)
	require.Equal(t, uint64(42), st.Cursor)
}

func TestSaveStateLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	for i := uint64(1); i <= 5; i++ {
		require.NoError(t, saveState(path, State{Cursor: i}))
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "state.json", entries[0].Name())
}

func TestLoadStateRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	_, err := loadState(path)
	require.ErrorContains(t, err, "parse state file")
}

func TestLoadStateRejectsOtherVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version": 99, "cursor": 3}`), 0o600))

	_, err := loadState(path)
	require.ErrorContains(t, err, "has version 99")
}

func TestSaveStateFailsWhenDirectoryIsAFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	err := saveState(filepath.Join(blocker, "state.json"), State{Cursor: 1})
	require.Error(t, err)
}
