package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The handles below stand for separate graft processes: each one loads the
// state when it is opened, before any command holds the lock.

func TestStatusWaitingForLockKeepsPendingCommit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"a.txt", "b.txt"}, []string{"one", "two"})
	ws := cloneTestWorkspace(t, b)
	root := ws.Root()
	// b.txt is old enough that a status re-hashes it and records it as verified,
	// which is how a stale state would be saved wholesale.
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(root, "b.txt"), old, old))

	statusProc, err := Open(root, b)
	require.NoError(t, err)
	commitProc, err := Open(root, b)
	require.NoError(t, err)

	writeWorkspaceFile(t, root, "a.txt", "edited")
	_, err = commitProc.Commit(ctx, CommitRequest{Message: "edit", Author: Signature{Name: "dev"}})
	require.NoError(t, err)

	_, err = statusProc.Status()
	require.NoError(t, err)

	reopened, err := Open(root, b)
	require.NoError(t, err)
	require.NotNil(t, reopened.Summary().Pending, "a status must not erase the pending commit saved by another command")
}

func TestCommitWaitingForLockIsRefusedWhilePending(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"a.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)
	root := ws.Root()

	first, err := Open(root, b)
	require.NoError(t, err)
	second, err := Open(root, b)
	require.NoError(t, err)

	writeWorkspaceFile(t, root, "a.txt", "two")
	_, err = first.Commit(ctx, CommitRequest{Message: "first", Author: Signature{Name: "dev"}})
	require.NoError(t, err)

	_, err = second.Commit(ctx, CommitRequest{Message: "second", Author: Signature{Name: "dev"}})
	require.ErrorContains(t, err, "pending", "a second commit must not start while the first one is pending")

	reopened, err := Open(root, b)
	require.NoError(t, err)
	require.NotNil(t, reopened.Summary().Pending)
	require.Len(t, b.branches, 2, "main and the temporary branch of the first commit only")
}

func TestRepositoryFileWithTemporaryPrefixIsTracked(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{".graft-tmp-notes", "a.txt"}, []string{"server notes", "one"})
	ws := cloneTestWorkspace(t, b)

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes, "a clean clone has no local changes")

	writeWorkspaceFile(t, ws.Root(), "a.txt", "two")
	writeWorkspaceFile(t, ws.Root(), ".graft-tmp-mine", "local notes")
	_, err = ws.Commit(ctx, CommitRequest{Message: "edit", Author: Signature{Name: "dev"}, LandBranch: "main"})
	require.NoError(t, err)

	require.Equal(t, "server notes", readRemote(t, b, "main", ".graft-tmp-notes"), "the server file is kept")
	require.Equal(t, "local notes", readRemote(t, b, "main", ".graft-tmp-mine"), "a local file with that prefix is committed")
	require.Equal(t, "two", readRemote(t, b, "main", "a.txt"))
}
