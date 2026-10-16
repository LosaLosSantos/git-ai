package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

// TestSDKBackendLandReportsBatchAndAttempts checks that a land on a real server
// reports its batch and attempts to the CLI: a land that is alone in its batch
// reports position 1 of 1 and one attempt.
func TestSDKBackendLandReportsBatchAndAttempts(t *testing.T) {
	t.Parallel()

	env := startServer(t)
	env.seedMain(t, map[string]string{"a.txt": "one\n"})
	b := env.backend(t)

	oid, err := b.CommitFiles(env.ctx, workspace.CommitOptions{
		Branch:   "feature",
		StartRev: "main",
		Message:  "add b",
		Author:   testAuthor(),
		Actions:  []workspace.Action{{Kind: workspace.ActionPut, Path: "b.txt", Content: []byte("two\n")}},
	})
	require.NoError(t, err)

	res, err := b.Land(env.ctx, workspace.LandOptions{
		Branch:    "main",
		Commit:    oid,
		Strategy:  workspace.StrategyRebase,
		Committer: testAuthor(),
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), res.BatchSize)
	require.Equal(t, int32(1), res.BatchPosition)
	require.Equal(t, int32(1), res.Attempts)
	require.Empty(t, batchNote(res.BatchSize, res.BatchPosition, res.Attempts), "a change alone in its batch prints no batch note")
}

// batchingBackend reports every land as position 3 of a batch of 17, as a server
// with batched landing would for a change that shared its batch.
type batchingBackend struct {
	*workspace.MemoryBackend
}

func (b *batchingBackend) Land(ctx context.Context, opts workspace.LandOptions) (workspace.LandResult, error) {
	res, err := b.MemoryBackend.Land(ctx, opts)
	if err == nil {
		res.BatchSize, res.BatchPosition, res.Attempts = 17, 3, 1
	}
	return res, err
}

// TestRunCommitLandPrintsBatch checks that graft commit --land prints the batch
// the change landed in, as graft land does.
func TestRunCommitLandPrintsBatch(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("base"))
	backend := &batchingBackend{MemoryBackend: mem}
	previous := backendFactory
	backendFactory = func(context.Context, target) (workspace.Backend, error) { return backend, nil }
	t.Cleanup(func() { backendFactory = previous })

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("local"), 0o644))

	code, stdout, stderr := runCLI(t, "commit", "-C", dir, "-m", "local", "--land", "main")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "and landed it on main (batch 3/17, 1 attempt)")
}

func TestBatchNote(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		size     int32
		position int32
		attempts int32
		want     string
	}{
		{name: "not batched", size: 0, position: 0, attempts: 1, want: ""},
		{name: "batch of one", size: 1, position: 1, attempts: 1, want: ""},
		{name: "batch of one with rebuilds", size: 1, position: 1, attempts: 3, want: ""},
		{name: "first of a batch", size: 12, position: 1, attempts: 1, want: " (batch 1/12, 1 attempt)"},
		{name: "last of a batch", size: 17, position: 3, attempts: 1, want: " (batch 3/17, 1 attempt)"},
		{name: "rebuilt batch", size: 12, position: 12, attempts: 2, want: " (batch 12/12, 2 attempts)"},
		{name: "attempts not reported", size: 12, position: 4, attempts: 0, want: " (batch 4/12)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, batchNote(tc.size, tc.position, tc.attempts))
		})
	}
}
