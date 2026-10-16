package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

const (
	testAgentName  = "Agent One"
	testAgentEmail = "agent@example.com"
)

func testAuthor() workspace.Signature {
	return workspace.Signature{Name: testAgentName, Email: testAgentEmail}
}

func cloneForTest(t *testing.T, env serverEnv, b *sdkBackend, sparse ...string) (string, *workspace.Workspace) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "ws")
	ws, err := workspace.Clone(env.ctx, dir, b, workspace.CloneOptions{
		Address: env.addr,
		Storage: env.storage,
		Repo:    env.relPath,
		Branch:  "main",
		Sparse:  sparse,
	})
	require.NoError(t, err)
	return dir, ws
}

func writeLocal(t *testing.T, root, rel, content string) {
	t.Helper()

	full := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

func readRemoteFile(t *testing.T, env serverEnv, b *sdkBackend, rev, path string) string {
	t.Helper()

	content, err := b.ReadFile(env.ctx, rev, path)
	require.NoError(t, err)
	return string(content)
}

func TestSDKBackendCloneCommitLandRoundTrip(t *testing.T) {
	t.Parallel()

	env := startServer(t)
	env.seedMain(t, map[string]string{"a/x.txt": "ax\n", "b/y.txt": "by\n", "top.txt": "top\n"})
	b := env.backend(t)

	dir, ws := cloneForTest(t, env, b, "a")
	require.Equal(t, "ax\n", readLocalFile(t, dir, "a/x.txt"))
	require.NoDirExists(t, filepath.Join(dir, "b"))
	require.NoFileExists(t, filepath.Join(dir, "top.txt"))

	writeLocal(t, dir, "a/x.txt", "changed\n")
	writeLocal(t, dir, "a/new.txt", "new\n")

	res, err := ws.Commit(env.ctx, workspace.CommitRequest{
		Message:    "change a",
		Author:     testAuthor(),
		LandBranch: "main",
	})
	require.NoError(t, err)
	require.True(t, res.Landed)
	require.Equal(t, 2, res.Changes)

	require.Equal(t, "changed\n", readRemoteFile(t, env, b, "main", "a/x.txt"))
	require.Equal(t, "new\n", readRemoteFile(t, env, b, "main", "a/new.txt"))
	require.Equal(t, "by\n", readRemoteFile(t, env, b, "main", "b/y.txt"), "server-side content outside the sparse set is untouched")

	_, err = b.ResolveRevision(env.ctx, res.Branch)
	require.ErrorIs(t, err, workspace.ErrNotFound, "the temporary branch is deleted after landing")

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
	require.Equal(t, ws.Summary().Base, landedHead(t, env, b))

	lines, err := b.Blame(env.ctx, "main", "a/x.txt")
	require.NoError(t, err)
	require.Len(t, lines, 1)
	require.Equal(t, "changed\n", string(lines[0].Content)+"\n")
	require.Equal(t, ws.Summary().Base, lines[0].CommitID)

	commits, err := b.PathHistory(env.ctx, "main", "a/x.txt", 10)
	require.NoError(t, err)
	require.Len(t, commits, 2)
	require.Equal(t, "change a", commits[0].Subject)
	require.Equal(t, testAgentName, commits[0].AuthorName)

	changesets, err := b.Changesets(env.ctx, 0, 0)
	require.NoError(t, err)
	require.NotEmpty(t, changesets)
	for i, c := range changesets {
		require.Equal(t, uint64(i+1), c.Sequence, "sequence numbers are dense")
		require.False(t, c.RecordedAt.IsZero())
	}

	var mainUpdate *workspace.Changeset
	for i := range changesets {
		if changesets[i].Reference == "refs/heads/main" {
			mainUpdate = &changesets[i]
		}
	}
	require.NotNil(t, mainUpdate, "the land is recorded for main")
	require.Equal(t, ws.Summary().Base, mainUpdate.NewOID)

	after, err := b.Changesets(env.ctx, mainUpdate.Sequence-1, 1)
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.Equal(t, mainUpdate.Sequence, after[0].Sequence)
}

// landedHead returns the current commit of main.
func landedHead(t *testing.T, env serverEnv, b *sdkBackend) string {
	t.Helper()

	oid, err := b.ResolveRevision(env.ctx, "main")
	require.NoError(t, err)
	return oid
}

func readLocalFile(t *testing.T, root, rel string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	require.NoError(t, err)
	return string(data)
}

func TestSDKBackendReadsAndErrorMapping(t *testing.T) {
	t.Parallel()

	env := startServer(t)
	env.seedMain(t, map[string]string{"a.txt": "one\n", "dir/b.txt": "two\n"})
	b := env.backend(t)

	_, err := b.ResolveRevision(env.ctx, "no-such-branch")
	require.ErrorIs(t, err, workspace.ErrNotFound)

	_, err = b.ReadFile(env.ctx, "main", "missing.txt")
	require.ErrorIs(t, err, workspace.ErrNotFound)

	_, err = b.ListTree(env.ctx, "main", "missing-dir", false)
	require.ErrorIs(t, err, workspace.ErrNotFound)

	entries, err := b.ListTree(env.ctx, "main", "", true)
	require.NoError(t, err)
	paths := map[string]workspace.Entry{}
	for _, e := range entries {
		paths[e.Path] = e
	}
	require.Equal(t, workspace.ObjectBlob, paths["a.txt"].Type)
	require.Equal(t, int64(len("one\n")), paths["a.txt"].Size)
	require.Equal(t, workspace.ObjectBlob, paths["dir/b.txt"].Type)

	objects, err := b.GetObjects(env.ctx, []string{paths["a.txt"].OID, missingOID})
	require.NoError(t, err)
	require.Equal(t, "one\n", string(objects[paths["a.txt"].OID]))
	require.NotContains(t, objects, missingOID, "missing objects are left out")

	_, err = b.CommitFiles(env.ctx, workspace.CommitOptions{
		Branch:   "clash",
		StartRev: "main",
		Message:  "clash",
		Author:   testAuthor(),
		Actions:  []workspace.Action{{Kind: workspace.ActionPut, Path: "a.txt", Content: []byte("dup")}},
	})
	require.ErrorIs(t, err, workspace.ErrAlreadyExists)

	_, err = b.CommitFiles(env.ctx, workspace.CommitOptions{
		Branch:   "unknown-action",
		StartRev: "main",
		Message:  "bad",
		Author:   testAuthor(),
		Actions:  []workspace.Action{{Kind: "rename", Path: "a.txt"}},
	})
	require.ErrorContains(t, err, "unknown action")

	_, err = b.Land(env.ctx, workspace.LandOptions{Branch: "main", Commit: missingOID, Strategy: workspace.StrategyRebase})
	require.Error(t, err)

	head := landedHead(t, env, b)
	_, err = b.Land(env.ctx, workspace.LandOptions{Branch: "main", Commit: head, Strategy: "squash"})
	require.ErrorContains(t, err, "unknown strategy")

	require.NoError(t, b.DeleteBranch(env.ctx, "never-existed"), "deleting a missing branch is not an error")
}

func TestSDKBackendMergeConflictAndFastForwardOnly(t *testing.T) {
	t.Parallel()

	env := startServer(t)
	env.seedMain(t, map[string]string{"a.txt": "base\n"})
	b := env.backend(t)

	dirA, wsA := cloneForTest(t, env, b)
	dirB, wsB := cloneForTest(t, env, b)

	writeLocal(t, dirA, "a.txt", "from A\n")
	_, err := wsA.Commit(env.ctx, workspace.CommitRequest{Message: "A", Author: testAuthor(), LandBranch: "main"})
	require.NoError(t, err)

	writeLocal(t, dirB, "a.txt", "from B\n")
	res, err := wsB.Commit(env.ctx, workspace.CommitRequest{Message: "B", Author: testAuthor()})
	require.NoError(t, err)
	require.False(t, res.Landed)

	_, err = wsB.Land(env.ctx, "", workspace.StrategyFFOnly, testAuthor())
	require.ErrorIs(t, err, workspace.ErrNotFastForward)
	require.NotNil(t, wsB.Summary().Pending)

	_, err = wsB.Land(env.ctx, "", workspace.StrategyRebase, testAuthor())
	require.ErrorIs(t, err, workspace.ErrMergeConflict)
	require.NotNil(t, wsB.Summary().Pending, "a conflicting commit stays pending")
	require.Equal(t, "from A\n", readRemoteFile(t, env, b, "main", "a.txt"))
}

func TestSDKBackendMergeStrategyCreatesMergeCommit(t *testing.T) {
	t.Parallel()

	env := startServer(t)
	env.seedMain(t, map[string]string{"a.txt": "base\n"})
	b := env.backend(t)

	dirA, wsA := cloneForTest(t, env, b)
	dirB, wsB := cloneForTest(t, env, b)

	writeLocal(t, dirA, "a.txt", "from A\n")
	_, err := wsA.Commit(env.ctx, workspace.CommitRequest{Message: "A", Author: testAuthor(), LandBranch: "main"})
	require.NoError(t, err)

	writeLocal(t, dirB, "b.txt", "from B\n")
	_, err = wsB.Commit(env.ctx, workspace.CommitRequest{
		Message:    "B",
		Author:     testAuthor(),
		LandBranch: "main",
		Strategy:   workspace.StrategyMerge,
	})
	require.NoError(t, err)

	require.Equal(t, "from A\n", readRemoteFile(t, env, b, "main", "a.txt"))
	require.Equal(t, "from B\n", readRemoteFile(t, env, b, "main", "b.txt"))

	commits, err := b.PathHistory(env.ctx, "main", "b.txt", 10)
	require.NoError(t, err)
	require.NotEmpty(t, commits)
}

func TestSDKBackendConcurrentAgentsLandDisjointChanges(t *testing.T) {
	t.Parallel()

	const agents = 10

	env := startServer(t)
	env.seedMain(t, map[string]string{"base.txt": "base\n"})
	b := env.backend(t)

	dirs := make([]string, agents)
	workspaces := make([]*workspace.Workspace, agents)
	for i := range agents {
		dirs[i], workspaces[i] = cloneForTest(t, env, b)
	}

	var wg sync.WaitGroup
	errs := make(chan error, agents)
	for i := range agents {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			name := fmt.Sprintf("agent-%02d.txt", i)
			if err := os.WriteFile(filepath.Join(dirs[i], name), []byte(name+"\n"), 0o644); err != nil {
				errs <- err
				return
			}
			_, err := workspaces[i].Commit(env.ctx, workspace.CommitRequest{
				Message:    "add " + name,
				Author:     testAuthor(),
				LandBranch: "main",
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	for i := range agents {
		name := fmt.Sprintf("agent-%02d.txt", i)
		require.Equal(t, name+"\n", readRemoteFile(t, env, b, "main", name))
	}
	require.Equal(t, "base\n", readRemoteFile(t, env, b, "main", "base.txt"))

	changesets, err := b.Changesets(env.ctx, 0, 0)
	require.NoError(t, err)
	// Concurrent lands on one branch are batched, so one changeset may record
	// several changes. Each change is a member of its batch, or else the
	// changeset itself.
	landed := 0
	for _, c := range changesets {
		if c.Reference != "refs/heads/main" {
			continue
		}
		if len(c.Members) > 0 {
			landed += len(c.Members)
		} else {
			landed++
		}
	}
	require.GreaterOrEqual(t, landed, agents, "every agent landed on main")
}

func TestSDKBackendWatchChangesetsDeliversBacklogAndLiveUpdates(t *testing.T) {
	t.Parallel()

	env := startServer(t)
	env.seedMain(t, map[string]string{"a.txt": "one\n"})
	b := env.backend(t)

	dir, ws := cloneForTest(t, env, b)
	writeLocal(t, dir, "a.txt", "two\n")
	_, err := ws.Commit(env.ctx, workspace.CommitRequest{Message: "two", Author: testAuthor(), LandBranch: "main"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	changes, errs := b.WatchChangesets(ctx, 0)

	// The backlog holds the earlier land on main, delivered in sequence order.
	backlog := receiveChangeset(t, changes, "refs/heads/main")
	require.Equal(t, ws.Summary().Base, backlog.NewOID)

	// A later land arrives live.
	writeLocal(t, dir, "a.txt", "three\n")
	_, err = ws.Commit(env.ctx, workspace.CommitRequest{Message: "three", Author: testAuthor(), LandBranch: "main"})
	require.NoError(t, err)

	live := receiveChangeset(t, changes, "refs/heads/main")
	require.Greater(t, live.Sequence, backlog.Sequence)
	require.Equal(t, ws.Summary().Base, live.NewOID)

	cancel()
	for range changes {
	}
	for err := range errs {
		require.NoError(t, err)
	}
}

// receiveChangeset waits for the next changeset of a watch that updates ref. It
// fails the test if a changeset arrives out of sequence order.
func receiveChangeset(t *testing.T, changes <-chan workspace.Changeset, ref string) workspace.Changeset {
	t.Helper()

	var last uint64
	for {
		select {
		case c, ok := <-changes:
			require.True(t, ok, "the watch ended early")
			require.Greater(t, c.Sequence, last, "sequences are delivered in order")
			last = c.Sequence
			if c.Reference == ref {
				return c
			}
		case <-time.After(10 * time.Second):
			require.FailNow(t, "no changeset received", "waiting for %s", ref)
		}
	}
}

func TestSDKBackendWatchReportsFailure(t *testing.T) {
	t.Parallel()

	env := startServer(t)
	b := env.backend(t)
	b.repo = b.client.Repo("does-not-exist")

	changes, errs := b.WatchChangesets(env.ctx, 0)
	for range changes {
	}
	err, ok := <-errs
	require.True(t, ok, "the failure is reported before the channel closes")
	require.Error(t, err)
}
