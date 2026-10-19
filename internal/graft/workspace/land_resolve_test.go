package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// recordingLandBackend records the land requests of a MemoryBackend. When a land asks for
// conflict resolution, it reports a resolution of resolvedPath, as a server would.
type recordingLandBackend struct {
	*MemoryBackend
	lands []LandOptions
}

const resolvedPath = "resolved.txt"

func (r *recordingLandBackend) Land(ctx context.Context, opts LandOptions) (LandResult, error) {
	r.lands = append(r.lands, opts)

	res, err := r.MemoryBackend.Land(ctx, opts)
	if err != nil {
		return LandResult{}, err
	}
	if opts.ResolveConflicts {
		res.ResolvedPaths = []string{resolvedPath}
		res.Resolver = "graft-resolve/test"
	}
	return res, nil
}

// writeLocalFile creates a local change of the workspace that the next commit records.
func writeLocalFile(t *testing.T, ws *Workspace, name string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(ws.Root(), name), []byte(name), 0o644))
}

func TestCommitLandWithResolveConflictsPassesOptionThrough(t *testing.T) {
	t.Parallel()

	b := &recordingLandBackend{MemoryBackend: seedBackend(t, []string{"base.txt"}, []string{"base"})}
	ws := cloneTestWorkspace(t, b)
	writeLocalFile(t, ws, "change.txt")

	res, err := ws.Commit(context.Background(), CommitRequest{
		Message:    "change",
		Author:     Signature{Name: "Ada", Email: "ada@example.com"},
		LandBranch: "main",
		Resolve:    true,
	})
	require.NoError(t, err)
	require.True(t, res.Landed)
	require.Equal(t, []string{resolvedPath}, res.ResolvedPaths)

	require.Len(t, b.lands, 1)
	require.True(t, b.lands[0].ResolveConflicts)
	require.Equal(t, "change.txt", readRemote(t, b, "main", "change.txt"))
}

func TestLandWithoutResolveConflictsDoesNotResolve(t *testing.T) {
	t.Parallel()

	b := &recordingLandBackend{MemoryBackend: seedBackend(t, []string{"base.txt"}, []string{"base"})}
	ws := cloneTestWorkspace(t, b)
	writeLocalFile(t, ws, "change.txt")

	_, err := ws.Commit(context.Background(), CommitRequest{
		Message: "change",
		Author:  Signature{Name: "Ada", Email: "ada@example.com"},
	})
	require.NoError(t, err)

	res, err := ws.Land(context.Background(), "", "", Signature{Name: "Ada", Email: "ada@example.com"})
	require.NoError(t, err)
	require.Empty(t, res.ResolvedPaths)
	require.Empty(t, res.Resolver)

	require.Len(t, b.lands, 1)
	require.False(t, b.lands[0].ResolveConflicts)
}

func TestLandResolvingPassesOptionThrough(t *testing.T) {
	t.Parallel()

	b := &recordingLandBackend{MemoryBackend: seedBackend(t, []string{"base.txt"}, []string{"base"})}
	ws := cloneTestWorkspace(t, b)
	writeLocalFile(t, ws, "change.txt")

	_, err := ws.Commit(context.Background(), CommitRequest{
		Message: "change",
		Author:  Signature{Name: "Ada", Email: "ada@example.com"},
	})
	require.NoError(t, err)

	res, err := ws.LandResolving(context.Background(), "", "", Signature{Name: "Ada", Email: "ada@example.com"}, true)
	require.NoError(t, err)
	require.Equal(t, []string{resolvedPath}, res.ResolvedPaths)
	require.Equal(t, "graft-resolve/test", res.Resolver)

	require.Len(t, b.lands, 1)
	require.True(t, b.lands[0].ResolveConflicts)
}

// TestLandResolutionLandsTheResolvedFiles drives LandResolution over the memory backend: the
// pending commit conflicts with the tip on a.txt, and a resolution of a.txt made against that tip
// lands it; a resolution against another tip, or one that names a path without a conflict, fails
// as a merge conflict and keeps the commit pending.
func TestLandResolutionLandsTheResolvedFiles(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt", "b.txt"}, []string{"base", "b"})
	ws := cloneTestWorkspace(t, b)
	require.NoError(t, os.WriteFile(filepath.Join(ws.Root(), "a.txt"), []byte("local"), 0o644))
	_, err := ws.Commit(context.Background(), CommitRequest{Message: "local a", Author: Signature{Name: "Ada", Email: "ada@example.com"}})
	require.NoError(t, err)
	pending := ws.state.Pending.Commit

	// Another writer changes a.txt on main.
	tip := b.Put("main", "a.txt", []byte("remote"))
	committer := Signature{Name: "Ada", Email: "ada@example.com"}

	_, err = ws.LandResolution(context.Background(), "", committer, &Resolution{Tip: "stale", Commit: pending,
		Files: []ResolvedFile{{Path: "a.txt", Mode: "100644", Content: []byte("both")}}})
	require.ErrorIs(t, err, ErrMergeConflict, "a resolution against another tip")

	_, err = ws.LandResolution(context.Background(), "", committer, &Resolution{Tip: tip, Commit: pending,
		Files: []ResolvedFile{{Path: "a.txt", Mode: "100644", Content: []byte("both")}, {Path: "b.txt", Mode: "100644", Content: []byte("x")}}})
	require.ErrorIs(t, err, ErrMergeConflict, "a resolution of a path without a conflict")
	require.NotNil(t, ws.state.Pending, "a failed land keeps the commit pending")

	_, err = ws.LandResolution(context.Background(), "", committer, &Resolution{Tip: tip, Commit: "other", Files: nil})
	require.ErrorContains(t, err, "not the pending commit")

	res, err := ws.LandResolution(context.Background(), "", committer, &Resolution{Tip: tip, Commit: pending,
		Files: []ResolvedFile{{Path: "a.txt", Mode: "100644", Content: []byte("both")}}})
	require.NoError(t, err)
	require.True(t, res.Rewritten)
	require.Equal(t, "both", readRemote(t, b, "main", "a.txt"))
	require.Equal(t, "b", readRemote(t, b, "main", "b.txt"))
	require.Nil(t, ws.state.Pending)
}
