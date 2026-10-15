package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConcurrentLandsOfDisjointChangesAllSucceed(t *testing.T) {
	t.Parallel()

	const agents = 12

	b := seedBackend(t, []string{"base.txt"}, []string{"base"})

	// Every agent starts from the same tip, so each land has to rebase onto the
	// changes that landed before it.
	workspaces := make([]*Workspace, agents)
	for i := range workspaces {
		workspaces[i] = cloneTestWorkspace(t, b)
	}

	var wg sync.WaitGroup
	errs := make(chan error, agents)
	for i, ws := range workspaces {
		wg.Add(1)
		go func(i int, ws *Workspace) {
			defer wg.Done()

			name := fmt.Sprintf("agent-%02d.txt", i)
			if err := os.WriteFile(filepath.Join(ws.Root(), name), []byte(name), 0o644); err != nil {
				errs <- err
				return
			}
			_, err := ws.Commit(context.Background(), CommitRequest{
				Message:    "add " + name,
				Author:     Signature{Name: fmt.Sprintf("agent %d", i), Email: "agent@example.com"},
				LandBranch: "main",
			})
			errs <- err
		}(i, ws)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	for i := range agents {
		name := fmt.Sprintf("agent-%02d.txt", i)
		require.Equal(t, name, readRemote(t, b, "main", name), "change of agent %d is on main", i)
	}
	require.Equal(t, "base", readRemote(t, b, "main", "base.txt"))

	for i, ws := range workspaces {
		changes, err := ws.Status()
		require.NoError(t, err)
		require.Empty(t, changes, "workspace %d is clean", i)
		require.Nil(t, ws.Summary().Pending, "workspace %d has nothing pending", i)
	}
}

func TestCommitRefusesToOverwriteUntrackedRemoteFile(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt", "b/y.txt"}, []string{"ax", "by"})
	ws := cloneTestWorkspace(t, b, "a")

	// The file exists on the server outside the sparse set, so the workspace does
	// not track it. Creating it locally must not silently replace it.
	b.Put("main", "b/y.txt", []byte("remote y"))
	writeWorkspaceFile(t, ws.Root(), "b/y.txt", "local y")

	res, err := ws.Commit(context.Background(), CommitRequest{
		Message:    "clobber",
		Author:     Signature{Name: "dev"},
		LandBranch: "main",
	})
	require.ErrorIs(t, err, ErrAlreadyExists)
	require.False(t, res.Landed)
	require.Equal(t, "remote y", readRemote(t, b, "main", "b/y.txt"))
}

func TestFastForwardOnlyDivergenceKeepsCommitPending(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"base"})
	ws := cloneTestWorkspace(t, b)

	writeWorkspaceFile(t, ws.Root(), "a.txt", "mine")
	res, err := ws.Commit(context.Background(), CommitRequest{Message: "mine", Author: Signature{Name: "dev"}})
	require.NoError(t, err)

	b.Put("main", "other.txt", []byte("other"))

	_, err = ws.Land(context.Background(), "", StrategyFFOnly, Signature{Name: "dev"})
	require.ErrorIs(t, err, ErrNotFastForward)
	require.NotNil(t, ws.Summary().Pending, "a refused land leaves the commit pending")
	require.Equal(t, res.Commit, ws.Summary().Pending.Commit)
	require.Equal(t, "base", readRemote(t, b, "main", "a.txt"))

	// The default strategy rebases the pending commit onto the diverged tip.
	landed, err := ws.Land(context.Background(), "", StrategyRebase, Signature{Name: "dev"})
	require.NoError(t, err)
	require.True(t, landed.Rewritten)
	require.Equal(t, "mine", readRemote(t, b, "main", "a.txt"))
	require.Equal(t, "other", readRemote(t, b, "main", "other.txt"))
}

func TestCommitLeavesNoTemporaryFiles(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a.txt", "two")

	_, err := ws.Commit(context.Background(), CommitRequest{
		Message:    "two",
		Author:     Signature{Name: "dev"},
		LandBranch: "main",
	})
	require.NoError(t, err)

	leftovers, err := filepath.Glob(filepath.Join(ws.Root(), metaDirName, ".graft-tmp-*"))
	require.NoError(t, err)
	require.Empty(t, leftovers)
	require.FileExists(t, filepath.Join(ws.Root(), metaDirName, stateFileName))
}

func TestStrategyNamesAreRecognized(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a.txt", "two")
	_, err := ws.Commit(context.Background(), CommitRequest{Message: "two", Author: Signature{Name: "dev"}})
	require.NoError(t, err)

	_, err = ws.Land(context.Background(), "", "squash", Signature{Name: "dev"})
	require.Error(t, err)
	require.NotNil(t, ws.Summary().Pending, "an unknown strategy does not consume the pending commit")
}
