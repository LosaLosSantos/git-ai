package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

// TestRunResolveNeedsLand checks that --resolve, which asks the server to resolve conflicts while
// landing, is a usage error on commit without --land instead of being silently ignored.
func TestRunResolveNeedsLand(t *testing.T) {
	useMemoryBackend(t, workspace.NewMemoryBackend())

	code, _, stderr := runCLI(t, "commit", "-m", "msg", "--resolve")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "--resolve requires --land BRANCH")
}

// resolvingBackend records the land requests of the CLI. The memory backend has no conflict
// resolver, so a land that asks for resolution is landed without it, and the result names the file
// the resolver is taken to have resolved. This checks the pass-through of the option and the note
// the CLI prints, while the resolution itself is covered by the server tests.
type resolvingBackend struct {
	*workspace.MemoryBackend
	lands []workspace.LandOptions
}

func (b *resolvingBackend) Land(ctx context.Context, opts workspace.LandOptions) (workspace.LandResult, error) {
	b.lands = append(b.lands, opts)

	resolve := opts.ResolveConflicts
	opts.ResolveConflicts = false
	res, err := b.MemoryBackend.Land(ctx, opts)
	if err == nil && resolve {
		res.ResolvedPaths = []string{"a.txt"}
		res.Resolver = "graft-resolve/fake"
	}
	return res, err
}

// useResolvingBackend routes every command to backend for the duration of the test.
func useResolvingBackend(t *testing.T, backend *resolvingBackend) {
	t.Helper()
	previous := backendFactory
	backendFactory = func(context.Context, target) (workspace.Backend, error) { return backend, nil }
	t.Cleanup(func() { backendFactory = previous })
}

// cloneWithRemoteChange clones the memory repository, then another agent lands a change to another
// file, so the local edit of a.txt lands onto a moved branch without a conflict.
func cloneWithRemoteChange(t *testing.T, mem *workspace.MemoryBackend) string {
	t.Helper()

	mem.Put("main", "a.txt", []byte("base"))
	backend := &resolvingBackend{MemoryBackend: mem}
	useResolvingBackend(t, backend)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	mem.Put("main", "other.txt", []byte("remote"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("local"), 0o644))
	return dir
}

// TestRunCommitLandResolvePassesThrough checks that graft commit --land B --resolve asks for the
// resolution and prints the files the server resolved.
func TestRunCommitLandResolvePassesThrough(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	dir := cloneWithRemoteChange(t, mem)
	backend := &resolvingBackend{MemoryBackend: mem}
	useResolvingBackend(t, backend)

	code, stdout, stderr := runCLI(t, "commit", "-C", dir, "-m", "local", "--land", "main", "--resolve")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "landed it on main (resolved 1 conflicting files: a.txt)")

	require.NotEmpty(t, backend.lands)
	last := backend.lands[len(backend.lands)-1]
	require.True(t, last.ResolveConflicts)
	require.Equal(t, "main", last.Branch)
}

// TestRunLandResolvePassesThrough checks that graft land --resolve asks for the resolution of the
// pending commit and prints the files the server resolved.
func TestRunLandResolvePassesThrough(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	dir := cloneWithRemoteChange(t, mem)
	backend := &resolvingBackend{MemoryBackend: mem}
	useResolvingBackend(t, backend)

	code, _, stderr := runCLI(t, "commit", "-C", dir, "-m", "local")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "land", "-C", dir, "--resolve", "main")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "landed at ")
	require.Contains(t, stdout, "(resolved 1 conflicting files: a.txt)")

	require.NotEmpty(t, backend.lands)
	last := backend.lands[len(backend.lands)-1]
	require.True(t, last.ResolveConflicts)
	require.Equal(t, "main", last.Branch)
}
