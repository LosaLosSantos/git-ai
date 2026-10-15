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
