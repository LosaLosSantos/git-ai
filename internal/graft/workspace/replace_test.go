package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommitReplacesDirectoryWithFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"dir/f.txt", "dir/sub/g.txt", "top.txt"}, []string{"f", "g", "top"})
	ws := cloneTestWorkspace(t, b)

	require.NoError(t, os.RemoveAll(filepath.Join(ws.Root(), "dir")))
	writeWorkspaceFile(t, ws.Root(), "dir", "now a file")

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Equal(t, []Change{
		{Path: "dir", Kind: ChangeAdded},
		{Path: "dir/f.txt", Kind: ChangeDeleted},
		{Path: "dir/sub/g.txt", Kind: ChangeDeleted},
	}, changes)

	res, err := ws.Commit(ctx, CommitRequest{Message: "dir becomes a file", Author: Signature{Name: "dev"}, LandBranch: "main"})
	require.NoError(t, err)
	require.True(t, res.Landed)
	require.Equal(t, 3, res.Changes)

	require.Equal(t, "now a file", readRemote(t, b, "main", "dir"))
	_, err = b.ReadFile(ctx, "main", "dir/f.txt")
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, "top", readRemote(t, b, "main", "top.txt"))

	changes, err = ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
	require.Equal(t, "now a file", readWorkspaceFile(t, ws.Root(), "dir"))
}

func TestCommitReplacesFileWithDirectory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"a", "top.txt"}, []string{"file", "top"})
	ws := cloneTestWorkspace(t, b)

	require.NoError(t, os.Remove(filepath.Join(ws.Root(), "a")))
	writeWorkspaceFile(t, ws.Root(), "a/b.txt", "nested")

	_, err := ws.Commit(ctx, CommitRequest{Message: "a becomes a directory", Author: Signature{Name: "dev"}, LandBranch: "main"})
	require.NoError(t, err)
	require.Equal(t, "nested", readRemote(t, b, "main", "a/b.txt"))

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
}

func TestCommitRefusesFileReplacingDirectoryWithFilesOutsideSparseSet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"dir/a/x.txt", "dir/b/y.txt"}, []string{"x", "y"})
	ws := cloneTestWorkspace(t, b, "dir/a")

	require.NoError(t, os.RemoveAll(filepath.Join(ws.Root(), "dir")))
	writeWorkspaceFile(t, ws.Root(), "dir", "now a file")

	_, err := ws.Commit(ctx, CommitRequest{Message: "m", Author: Signature{Name: "dev"}, LandBranch: "main"})
	require.ErrorIs(t, err, ErrAlreadyExists)
	require.ErrorContains(t, err, "dir is a directory on the server that holds files outside the sparse set (dir/b/y.txt)")
	require.Nil(t, ws.Summary().Pending)
	require.Equal(t, "y", readRemote(t, b, "main", "dir/b/y.txt"))
}

func TestMemoryBackendAppliesActionsInOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"dir/f.txt"}, []string{"f"})

	_, err := b.ReadFile(ctx, "main", "dir")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotFound, "a directory is not a missing path")

	commit := func(actions ...Action) error {
		_, err := b.CommitFiles(ctx, CommitOptions{Branch: "tmp", StartRev: "main", Message: "m", Actions: actions})
		return err
	}
	require.ErrorContains(t, commit(
		Action{Kind: ActionPut, Path: "dir", Content: []byte("file")},
		Action{Kind: ActionDelete, Path: "dir/f.txt"},
	), "a directory with this name exists")
	require.ErrorContains(t, commit(Action{Kind: ActionPut, Path: "dir/f.txt/x", Content: []byte("x")}), `"dir/f.txt" is a file`)
	require.NoError(t, commit(
		Action{Kind: ActionDelete, Path: "dir/f.txt"},
		Action{Kind: ActionPut, Path: "dir", Content: []byte("file")},
	))
}

// failingObjectsBackend fails every GetObjects call, like an interrupted
// transfer.
type failingObjectsBackend struct {
	Backend
}

func (failingObjectsBackend) GetObjects(context.Context, []string) (map[string][]byte, error) {
	return nil, errors.New("interrupted")
}

func TestFailedCloneLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"a/x.txt", "top.txt"}, []string{"x", "top"})

	t.Run("directory created by the clone", func(t *testing.T) {
		t.Parallel()

		dir := filepath.Join(t.TempDir(), "ws")
		_, err := Clone(ctx, dir, failingObjectsBackend{Backend: b}, CloneOptions{Branch: "main"})
		require.ErrorContains(t, err, "interrupted")
		require.NoDirExists(t, dir)

		ws, err := Clone(ctx, dir, b, CloneOptions{Branch: "main"})
		require.NoError(t, err, "the clone can be run again")
		require.Equal(t, "x", readWorkspaceFile(t, ws.Root(), "a/x.txt"))
	})

	t.Run("existing empty directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		_, err := Clone(ctx, dir, failingObjectsBackend{Backend: b}, CloneOptions{Branch: "main"})
		require.ErrorContains(t, err, "interrupted")
		require.DirExists(t, dir, "a directory that existed before is kept")
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Empty(t, entries)

		_, err = Open(dir, b)
		require.ErrorIs(t, err, ErrNotWorkspace)

		ws, err := Clone(ctx, dir, b, CloneOptions{Branch: "main"})
		require.NoError(t, err, "the clone can be run again")
		require.Equal(t, "top", readWorkspaceFile(t, ws.Root(), "top.txt"))
	})
}

func TestWorkspaceWithoutBaseAsksForUpdate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"a.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)

	// A clone interrupted by a crash, before this was cleaned up, left a state
	// without a base commit and without files.
	require.NoError(t, os.Remove(filepath.Join(ws.Root(), "a.txt")))
	ws.state.Base = ""
	ws.state.Files = map[string]FileState{}
	require.NoError(t, ws.state.save(ws.Root()))
	ws, err := Open(ws.Root(), b)
	require.NoError(t, err)
	writeWorkspaceFile(t, ws.Root(), "new.txt", "new")

	_, err = ws.Status()
	require.ErrorContains(t, err, "run graft update")
	_, err = ws.Diff()
	require.ErrorContains(t, err, "run graft update")
	_, err = ws.Commit(ctx, CommitRequest{Message: "m", Author: Signature{Name: "dev"}})
	require.ErrorContains(t, err, "run graft update")
	require.ErrorContains(t, ws.Sparse(ctx, []string{"a.txt"}), "run graft update")
	require.ErrorContains(t, ws.Revert(ctx, nil), "run graft update")

	_, err = ws.Update(ctx)
	require.NoError(t, err)
	require.Equal(t, "one", readWorkspaceFile(t, ws.Root(), "a.txt"))
	changes, err := ws.Status()
	require.NoError(t, err)
	require.Equal(t, []Change{{Path: "new.txt", Kind: ChangeAdded}}, changes)
}
