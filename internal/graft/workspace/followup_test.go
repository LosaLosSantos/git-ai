package workspace

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRevertSelectsDirectoriesAndRejectsUnknownPaths(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"a/x.txt", "a/y.txt", "ab.txt", "top.txt"}, []string{"x", "y", "ab", "top"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "edit x")
	writeWorkspaceFile(t, ws.Root(), "a/new.txt", "new")
	writeWorkspaceFile(t, ws.Root(), "ab.txt", "edit ab")
	writeWorkspaceFile(t, ws.Root(), "top.txt", "edit top")

	for _, unknown := range []string{"nope.txt", "a/nope", "zzz/"} {
		err := ws.Revert(ctx, []string{"top.txt", unknown})
		require.ErrorContains(t, err, "does not match any file", unknown)
	}
	require.Equal(t, "edit top", readWorkspaceFile(t, ws.Root(), "top.txt"), "a refused revert reverts nothing")

	// A directory, with or without a trailing slash, selects the changes under it
	// and nothing that merely shares its name as a prefix.
	require.NoError(t, ws.Revert(ctx, []string{"a/"}))
	require.Equal(t, "x", readWorkspaceFile(t, ws.Root(), "a/x.txt"))
	require.NoFileExists(t, filepath.Join(ws.Root(), "a", "new.txt"))
	changes, err := ws.Status()
	require.NoError(t, err)
	require.Equal(t, []Change{{Path: "ab.txt", Kind: ChangeModified}, {Path: "top.txt", Kind: ChangeModified}}, changes)

	// A tracked file without changes is accepted and left alone.
	require.NoError(t, ws.Revert(ctx, []string{"a/y.txt", "a"}))
	require.NoError(t, ws.Revert(ctx, []string{"ab.txt"}))
	changes, err = ws.Status()
	require.NoError(t, err)
	require.Equal(t, []Change{{Path: "top.txt", Kind: ChangeModified}}, changes)
}

// landedButNotRefreshed returns a workspace whose pending commit of a.txt has
// landed on main while the refresh failed, because b.txt was changed both on
// main and locally.
func landedButNotRefreshed(t *testing.T) (*MemoryBackend, *Workspace) {
	t.Helper()

	ctx := context.Background()
	author := Signature{Name: "dev"}
	b := seedBackend(t, []string{"a.txt", "b.txt"}, []string{"A0", "B0"})
	ws := cloneTestWorkspace(t, b)

	writeWorkspaceFile(t, ws.Root(), "a.txt", "A1")
	_, err := ws.Commit(ctx, CommitRequest{Message: "a=A1", Author: author})
	require.NoError(t, err)

	b.Put("main", "b.txt", []byte("B-remote"))
	writeWorkspaceFile(t, ws.Root(), "b.txt", "B-local")

	_, err = ws.Land(ctx, "", "", author)
	require.ErrorContains(t, err, "refreshing the workspace failed")
	require.Equal(t, "A1", readRemote(t, b, "main", "a.txt"), "the land reached the branch")
	require.NotNil(t, ws.Summary().Pending)
	require.NotEmpty(t, ws.Summary().Pending.Landed)
	return b, ws
}

func TestAbortRefusesCommitThatAlreadyLanded(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	author := Signature{Name: "dev"}
	b, ws := landedButNotRefreshed(t)
	landed := ws.Summary().Pending.Landed

	require.ErrorContains(t, ws.Abort(ctx), "already landed")
	require.NotNil(t, ws.Summary().Pending, "a refused abort keeps the commit pending")

	// The state survives a reopen.
	ws, err := Open(ws.Root(), b)
	require.NoError(t, err)
	require.ErrorContains(t, ws.Abort(ctx), "already landed")

	// Landing onto another branch is refused: the commit is already on main.
	_, err = ws.Land(ctx, "other", "", author)
	require.ErrorContains(t, err, "already landed")

	// The local edit of the landed file cannot be reverted to the old base, but
	// the conflicting file that the commit did not change can.
	writeWorkspaceFile(t, ws.Root(), "a.txt", "A2")
	require.ErrorContains(t, ws.Revert(ctx, []string{"a.txt"}), "part of the landed commit")
	writeWorkspaceFile(t, ws.Root(), "a.txt", "A1")
	require.NoError(t, ws.Revert(ctx, []string{"b.txt"}))

	res, err := ws.Land(ctx, "", "", author)
	require.NoError(t, err)
	require.True(t, res.AlreadyLanded)
	require.Equal(t, landed, res.Commit)
	require.Nil(t, ws.Summary().Pending)
	require.Equal(t, landed, ws.Summary().Base)

	tip, err := b.ResolveRevision(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, landed, tip, "the retry does not land the commit a second time")
	require.Equal(t, "B-remote", readWorkspaceFile(t, ws.Root(), "b.txt"))
	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)

	_, err = ws.Update(ctx)
	require.NoError(t, err, "nothing blocks an update after the refresh")
}

func TestLandedButNotRefreshedGuidanceDoesNotSuggestAbort(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, ws := landedButNotRefreshed(t)

	_, err := ws.Update(ctx)
	require.ErrorContains(t, err, "landed as")
	require.NotContains(t, err.Error(), "--abort")
	_, err = ws.Commit(ctx, CommitRequest{Message: "more", Author: Signature{Name: "dev"}})
	require.ErrorContains(t, err, "landed as")
	require.NotContains(t, err.Error(), "--abort")

	// Sparse finishes the refresh with the new sparse set; here the local edit
	// of b.txt, which the new set leaves out, still blocks it.
	err = ws.Sparse(ctx, []string{"a.txt"})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{"b.txt"}, conflict.Paths)
	require.ErrorContains(t, err, "landed as")
	require.NotContains(t, err.Error(), "--abort")
	require.NotNil(t, ws.Summary().Pending)
}

func TestAbortStillDropsCommitThatDidNotLand(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"a.txt"}, []string{"A0"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a.txt", "A1")
	_, err := ws.Commit(ctx, CommitRequest{Message: "a", Author: Signature{Name: "dev"}})
	require.NoError(t, err)
	require.Empty(t, ws.Summary().Pending.Landed)

	_, err = ws.Update(ctx)
	require.ErrorContains(t, err, "--abort", "an unlanded commit can be dropped")
	require.NoError(t, ws.Abort(ctx))
	require.Equal(t, "A0", readRemote(t, b, "main", "a.txt"))
	require.Equal(t, "A1", readWorkspaceFile(t, ws.Root(), "a.txt"))
}

func TestLockFileLeftByAnInterruptedCommandDoesNotBlock(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"base"})
	ws := cloneTestWorkspace(t, b)
	// A command that was killed leaves the lock file, but not the lock.
	require.NoError(t, os.WriteFile(lockPath(ws.Root()), []byte("stale"), 0o600))

	_, err := ws.Status()
	require.NoError(t, err)
	writeWorkspaceFile(t, ws.Root(), "a.txt", "edit")
	_, err = ws.Commit(context.Background(), CommitRequest{Message: "edit", Author: Signature{Name: "dev"}, LandBranch: "main"})
	require.NoError(t, err)
}

func TestConcurrentReadOnlyCommandsWaitForEachOther(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"base"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a.txt", "edit")

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each command opens the workspace, as separate processes do.
			other, err := Open(ws.Root(), b)
			if err == nil {
				_, err = other.Status()
			}
			if err == nil {
				_, err = other.Diff()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

// lockHelperEnv makes the test binary act as a graft process that takes the lock
// of the workspace named by the variable and then waits to be killed.
const lockHelperEnv = "GRAFT_WORKSPACE_LOCK_HELPER"

func TestLockHelperProcess(t *testing.T) {
	root := os.Getenv(lockHelperEnv)
	if root == "" {
		t.Skip("only runs as a helper process")
	}
	if _, err := lockWithin(root, 0); err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("locked\n")
	time.Sleep(time.Minute)
	os.Exit(0)
}

func TestLockOfKilledProcessIsReleased(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"base"})
	ws := cloneTestWorkspace(t, b)

	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+ws.Root())
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "locked", strings.TrimSpace(line))

	_, err = lockWithin(ws.Root(), 0)
	require.ErrorIs(t, err, ErrLocked, "another process holds the lock")

	// Killing the holder, as an interrupt or a crash does, releases the lock even
	// though the lock file stays.
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	require.FileExists(t, lockPath(ws.Root()))
	_, err = ws.Status()
	require.NoError(t, err)
}
