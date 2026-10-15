package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/open_scion/internal/graft/workspace"
)

// TestRunRecoversFromRefusedLand checks the way out of a conflicting land: the
// pending commit is dropped, the server version is restored and the workspace
// accepts updates and commits again.
func TestRunRecoversFromRefusedLand(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("base"))
	useMemoryBackend(t, mem)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	// Another agent lands a change to the same file, and the local edit conflicts.
	mem.Put("main", "a.txt", []byte("remote"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("local"), 0o644))
	code, _, _ = runCLI(t, "commit", "-C", dir, "-m", "local", "--land", "main")
	require.NotEqual(t, exitOK, code, "the land conflicts")

	code, _, stderr = runCLI(t, "land", "--abort", "-C", dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, _ := runCLI(t, "status", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "M a.txt", "the edit is kept")

	code, _, stderr = runCLI(t, "revert", "-C", dir, "a.txt")
	require.Equal(t, exitOK, code, stderr)

	code, _, stderr = runCLI(t, "update", "-C", dir)
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, "remote", readFileForTest(t, filepath.Join(dir, "a.txt")))
}

func TestRunLandAbortWithoutPendingCommitFails(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("base"))
	useMemoryBackend(t, mem)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	code, _, _ = runCLI(t, "land", "--abort", "-C", dir)
	require.NotEqual(t, exitOK, code)
}

func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}
