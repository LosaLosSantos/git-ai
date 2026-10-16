package main

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/stretchr/testify/require"
)

// failedWatchBackend returns a watch in the state a backend leaves it in when it
// fails while the consumer is busy: the error is buffered and both channels are
// closed.
type failedWatchBackend struct {
	workspace.Backend
	err error
}

func (b failedWatchBackend) WatchChangesets(context.Context, uint64) (<-chan workspace.Changeset, <-chan error) {
	changes := make(chan workspace.Changeset)
	errs := make(chan error, 1)
	errs <- b.err
	close(errs)
	close(changes)
	return changes, errs
}

func TestRunWatchReportsFailureEvenWhenChangesetsCloseFirst(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("one"))
	useMemoryBackend(t, mem)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	failed := failedWatchBackend{Backend: mem, err: errors.New("repository not found")}
	backendFactory = func(context.Context, target) (workspace.Backend, error) { return failed, nil }

	// select picks a ready case at random, so a dropped error shows up within a
	// few runs.
	for i := 0; i < 50; i++ {
		code, _, stderr := runCLI(t, "watch", "-C", dir)
		require.Equal(t, exitError, code, "run %d", i)
		require.Contains(t, stderr, "repository not found")
	}
}

// TestCLICommitPassesPolicyForAgentNameThatIsNotASlug checks that an agent named
// build_bot can commit and land under a policy written for
// refs/heads/graft/build_bot/**: the temporary branch keeps the name as given.
func TestCLICommitPassesPolicyForAgentNameThatIsNotASlug(t *testing.T) {
	env := startPolicyServerFor(t, "build_bot", "bb-secret")

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-token", "bb-secret", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	writeLocal(t, dir, "a.txt", "two\n")
	code, stdout, stderr := runCLI(t, "commit", "-C", dir, "-token", "bb-secret", "-user", "build_bot", "-m", "change")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "graft/build_bot/")
	code, _, stderr = runCLI(t, "land", "-C", dir, "-token", "bb-secret", "-user", "build_bot")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, _ = runCLI(t, "cat", "-C", dir, "-token", "bb-secret", "main:a.txt")
	require.Equal(t, exitOK, code)
	require.Equal(t, "two\n", stdout)
}

// TestCLIClonesAndLandsWithColonInPath checks against a real server that a
// repository with a colon in a path can be cloned, and that a land refreshes
// the workspace. Only Windows refuses such names.
func TestCLIClonesAndLandsWithColonInPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a colon is not a valid file name character on Windows")
	}
	env := startServer(t)
	env.seedMain(t, map[string]string{"ok.txt": "one\n", "docs/a:b.txt": "colon\n"})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)
	require.NotContains(t, stderr, "warning")
	require.Equal(t, "colon\n", readLocalFile(t, dir, "docs/a:b.txt"))

	writeLocal(t, dir, "ok.txt", "two\n")
	code, stdout, stderr := runCLI(t, "commit", "-C", dir, "-m", "change", "-land", "main")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "landed it on main")

	code, stdout, _ = runCLI(t, "cat", "-C", dir, "main:docs/a:b.txt")
	require.Equal(t, exitOK, code)
	require.Equal(t, "colon\n", stdout)
}

func TestRunWarnsAboutPathsThatAreNotMaterialized(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "sub/code.go", []byte("package sub"))
	mem.Put("main", "sub/.graft/state.json", []byte(`{"version":1,"address":"tcp://attacker.example:9999","repo":"evil.git"}`))
	useMemoryBackend(t, mem)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stderr, `warning: not materialized: reserved path "sub/.graft/state.json"`)
	require.NoFileExists(t, filepath.Join(dir, "sub", ".graft", "state.json"))

	root, err := workspace.Find(filepath.Join(dir, "sub"))
	require.NoError(t, err)
	require.Equal(t, dir, root)

	code, _, stderr = runCLI(t, "update", "-C", dir)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stderr, "sub/.graft/state.json")
}
