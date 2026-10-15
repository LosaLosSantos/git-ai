package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCommitRecordsModeChangeOfTrackedFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the executable bit is not tracked on Windows")
	}

	ctx := context.Background()
	b := seedBackend(t, []string{"run.sh"}, []string{"echo hi\n"})
	ws := cloneTestWorkspace(t, b)
	script := filepath.Join(ws.Root(), "run.sh")
	require.NoError(t, os.Chmod(script, 0o755))

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Equal(t, []Change{{Path: "run.sh", Kind: ChangeModified}}, changes)

	_, err = ws.Commit(ctx, CommitRequest{Message: "make executable", Author: Signature{Name: "dev"}, LandBranch: "main"})
	require.NoError(t, err)

	entries, err := b.ListTree(ctx, "main", "", false)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, modeExec, entries[0].Mode, "the server records the mode change")

	info, err := os.Stat(script)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "the land does not revert the local mode")

	changes, err = ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
	unlock, err := lockWithin(ws.Root(), 0)
	require.NoError(t, err, "a finished command releases the lock")
	unlock()
}

func TestCommitRecordsContentAndModeChangeTogether(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the executable bit is not tracked on Windows")
	}

	ctx := context.Background()
	b := seedBackend(t, []string{"run.sh"}, []string{"echo hi\n"})
	ws := cloneTestWorkspace(t, b)
	script := filepath.Join(ws.Root(), "run.sh")
	writeWorkspaceFile(t, ws.Root(), "run.sh", "echo bye\n")
	require.NoError(t, os.Chmod(script, 0o755))

	_, err := ws.Commit(ctx, CommitRequest{Message: "edit and chmod", Author: Signature{Name: "dev"}, LandBranch: "main"})
	require.NoError(t, err)

	require.Equal(t, "echo bye\n", readRemote(t, b, "main", "run.sh"))
	entries, err := b.ListTree(ctx, "main", "", false)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, modeExec, entries[0].Mode)
}

func TestOverwriteKeepsModeOfReplacedFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"run.sh"}, []string{"old"})
	// As on the server, Executable is ignored when an existing file is overwritten.
	_, err := b.CommitFiles(ctx, CommitOptions{
		Branch:   "main",
		StartRev: "main",
		Message:  "overwrite",
		Author:   Signature{Name: "dev"},
		Actions:  []Action{{Kind: ActionPut, Path: "run.sh", Content: []byte("new"), Executable: true, Overwrite: true}},
	})
	require.NoError(t, err)

	entries, err := b.ListTree(ctx, "main", "", false)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, modeRegular, entries[0].Mode)
}

func TestLandOntoOtherBranchMakesItTheWorkspaceBranch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"a.txt"}, []string{"base"})
	_, err := b.CommitFiles(ctx, CommitOptions{
		Branch:   "feature",
		StartRev: "main",
		Message:  "feature",
		Author:   Signature{Name: "dev"},
		Actions:  []Action{{Kind: ActionPut, Path: "f.txt", Content: []byte("feature only")}},
	})
	require.NoError(t, err)
	ws := cloneTestWorkspace(t, b)

	writeWorkspaceFile(t, ws.Root(), "a.txt", "edit")
	res, err := ws.Commit(ctx, CommitRequest{Message: "edit", Author: Signature{Name: "dev"}, LandBranch: "feature"})
	require.NoError(t, err)
	require.True(t, res.Landed)

	// The base moved to the landed commit, so the workspace must name that branch.
	// Otherwise the next default land would target main with feature's history.
	require.Equal(t, "feature", ws.Summary().Branch)
	require.Equal(t, "feature only", readWorkspaceFile(t, ws.Root(), "f.txt"))
	require.Equal(t, "edit", readRemote(t, b, "feature", "a.txt"))
	_, err = b.ReadFile(ctx, "main", "f.txt")
	require.ErrorIs(t, err, ErrNotFound, "landing onto feature does not touch main")
}

func TestAbortDropsPendingCommitAndRevertRecoversWorkspace(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"a.txt"}, []string{"base"})
	ws := cloneTestWorkspace(t, b)

	b.Put("main", "a.txt", []byte("remote"))
	writeWorkspaceFile(t, ws.Root(), "a.txt", "local")
	_, err := ws.Commit(ctx, CommitRequest{Message: "local", Author: Signature{Name: "dev"}, LandBranch: "main"})
	require.ErrorIs(t, err, ErrMergeConflict)
	require.NotNil(t, ws.Summary().Pending)

	require.NoError(t, ws.Abort(ctx))
	require.Nil(t, ws.Summary().Pending)
	changes, err := ws.Status()
	require.NoError(t, err)
	require.Equal(t, []Change{{Path: "a.txt", Kind: ChangeModified}}, changes, "the local edit is kept as a change")
	require.Equal(t, "local", readWorkspaceFile(t, ws.Root(), "a.txt"))

	require.NoError(t, ws.Revert(ctx, []string{"a.txt"}))
	require.Equal(t, "base", readWorkspaceFile(t, ws.Root(), "a.txt"))

	_, err = ws.Update(ctx)
	require.NoError(t, err)
	require.Equal(t, "remote", readWorkspaceFile(t, ws.Root(), "a.txt"))

	require.Error(t, ws.Abort(ctx), "nothing is pending any more")
}

func TestRevertRestoresBaseAndRemovesUntrackedFiles(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt", "d/x.txt"}, []string{"base", "x"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a.txt", "edit")
	require.NoError(t, os.Remove(filepath.Join(ws.Root(), "d", "x.txt")))
	writeWorkspaceFile(t, ws.Root(), "new/untracked.txt", "mine")

	require.NoError(t, ws.Revert(context.Background(), []string{"a.txt"}))
	require.Equal(t, "base", readWorkspaceFile(t, ws.Root(), "a.txt"))
	changes, err := ws.Status()
	require.NoError(t, err)
	require.Equal(t, []Change{{Path: "d/x.txt", Kind: ChangeDeleted}, {Path: "new/untracked.txt", Kind: ChangeAdded}}, changes)

	require.NoError(t, ws.Revert(context.Background(), nil))
	require.Equal(t, "x", readWorkspaceFile(t, ws.Root(), "d/x.txt"))
	require.NoDirExists(t, filepath.Join(ws.Root(), "new"))
	changes, err = ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
}

func TestUpdateRefusesUntrackedFileBlockingDirectoryBecomingFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"d/x.txt"}, []string{"x"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "d/untracked.txt", "mine")

	_, err := b.CommitFiles(ctx, CommitOptions{
		Branch:   "main",
		StartRev: "main",
		Message:  "replace the directory with a file",
		Author:   Signature{Name: "other"},
		Actions: []Action{
			{Kind: ActionDelete, Path: "d/x.txt"},
			{Kind: ActionPut, Path: "d", Content: []byte("now a file")},
		},
	})
	require.NoError(t, err)
	base := ws.Summary().Base

	_, err = ws.Update(ctx)
	var conflict *ConflictError
	require.True(t, errors.As(err, &conflict), "got %v", err)
	require.Equal(t, []string{"d/untracked.txt"}, conflict.Paths)
	require.Equal(t, "x", readWorkspaceFile(t, ws.Root(), "d/x.txt"), "nothing is removed on conflict")
	require.Equal(t, "mine", readWorkspaceFile(t, ws.Root(), "d/untracked.txt"))
	require.Equal(t, base, ws.Summary().Base)

	require.NoError(t, os.Remove(filepath.Join(ws.Root(), "d", "untracked.txt")))
	_, err = ws.Update(ctx)
	require.NoError(t, err)
	require.Equal(t, "now a file", readWorkspaceFile(t, ws.Root(), "d"))
}

func TestCloneMaterializesFileSparsePrefix(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"top.txt", "sub/a.txt"}, []string{"top", "a"})
	ws := cloneTestWorkspace(t, b, "top.txt")
	require.Equal(t, "top", readWorkspaceFile(t, ws.Root(), "top.txt"))
	require.NoDirExists(t, filepath.Join(ws.Root(), "sub"))
	require.Equal(t, []string{"top.txt"}, ws.Summary().Sparse)
}

func TestWindowsNameProblem(t *testing.T) {
	t.Parallel()

	for _, name := range []string{".graft.", ".graft ", "x.", "GRAFT~1", "x~2.txt", "CON", "nul.txt", "com1", "LPT9.log", "a:b", "a\\b", "C:", "a?b", "a*b", "a<b", "a|b", "tab\tname"} {
		require.True(t, windowsNameProblem(name), name)
	}
	for _, name := range []string{"graft", "file.txt", "a~b", "COM10", "console.txt", "dir"} {
		require.False(t, windowsNameProblem(name), name)
	}
}

// TestLockRejectsConcurrentWorkspaceCommands changes lockWait, so it does not
// run in parallel with the other tests.
func TestLockRejectsConcurrentWorkspaceCommands(t *testing.T) {
	restore := lockWait
	lockWait = 100 * time.Millisecond
	t.Cleanup(func() { lockWait = restore })

	ctx := context.Background()
	b := seedBackend(t, []string{"a.txt"}, []string{"base"})
	ws := cloneTestWorkspace(t, b)

	// Another graft command holds the lock.
	unlock, err := lockWithin(ws.Root(), 0)
	require.NoError(t, err)
	_, err = ws.Status()
	require.ErrorIs(t, err, ErrLocked)
	writeWorkspaceFile(t, ws.Root(), "a.txt", "edit")
	_, err = ws.Commit(ctx, CommitRequest{Message: "edit", Author: Signature{Name: "dev"}})
	require.ErrorIs(t, err, ErrLocked)
	require.Equal(t, "edit", readWorkspaceFile(t, ws.Root(), "a.txt"), "a refused command writes nothing")
	require.Nil(t, ws.Summary().Pending)

	unlock()
	_, err = ws.Commit(ctx, CommitRequest{Message: "edit", Author: Signature{Name: "dev"}})
	require.NoError(t, err)
}

func TestSymbolicLinkIsReportedAndNotCommitted(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need extra privileges on Windows")
	}

	b := seedBackend(t, []string{"a.txt"}, []string{"base"})
	ws := cloneTestWorkspace(t, b)
	require.NoError(t, os.Symlink("a.txt", filepath.Join(ws.Root(), "link")))

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Equal(t, []Change{{Path: "link", Kind: ChangeAdded}}, changes, "a link is not silently ignored")

	_, err = ws.Commit(context.Background(), CommitRequest{Message: "link", Author: Signature{Name: "dev"}})
	require.ErrorContains(t, err, "symbolic link")
}

func TestRelativeRootRemovesEmptyDirectories(t *testing.T) {
	// t.Chdir cannot be used by parallel tests.
	ctx := context.Background()
	b := seedBackend(t, []string{"a.txt", "d/x.txt"}, []string{"a", "x"})
	root := filepath.Join(t.TempDir(), "ws")
	_, err := Clone(ctx, root, b, CloneOptions{Address: "memory", Storage: "default", Repo: "repo", Branch: "main"})
	require.NoError(t, err)

	t.Chdir(root)
	ws, err := Open(".", b)
	require.NoError(t, err)
	require.NoError(t, ws.Sparse(ctx, []string{"a.txt"}))
	require.NoDirExists(t, filepath.Join(root, "d"), "the emptied directory is removed from a relative root")
}
