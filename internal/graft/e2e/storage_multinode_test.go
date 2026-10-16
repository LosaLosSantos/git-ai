package e2e

import (
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// TestStorageWriteOnOneNodeReadOnAnother writes on node A and reads on node B,
// then the other way round. B has never seen the repository before its first
// read: it hydrates its local copy from the database and the bucket. Both
// nodes serve the same changelog from the database, and both local copies
// equal the database afterwards.
func TestStorageWriteOnOneNodeReadOnAnother(t *testing.T) {
	t.Parallel()

	forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		graft, tokens := workerGraft(2)
		cluster := newStorageCluster(t, dialect, corestore.ModeAuthoritative, unbatched(graft))
		a := cluster.addNode(t)
		b := cluster.addNode(t)

		rel := a.createRepository(t).GetRelativePath()
		onA := a.agent(t, rel, tokens[workerName(0)])
		onB := b.agent(t, rel, tokens[workerName(1)])

		first, err := commitFile(a.ctx, onA, workerName(0), "main", "", "README.md", "written on node-a\n")
		require.NoError(t, err)
		require.NoDirExists(t, b.repoPath(rel), "node-b has no copy before its first read")

		tip, err := onB.ResolveRevision(b.ctx, "main")
		require.NoError(t, err)
		require.Equal(t, first, tip, "node-b serves the write of node-a")
		content, err := onB.ReadFile(b.ctx, "main", "README.md")
		require.NoError(t, err)
		require.Equal(t, "written on node-a\n", string(content))
		require.Equal(t, corestore.Target(first), b.localRefs(t, rel)["refs/heads/main"], "node-b hydrated its local copy")

		// And the other way round: node-a catches up with a write of node-b.
		second, err := commitFile(b.ctx, onB, workerName(1), "main", first, "b.txt", "written on node-b\n")
		require.NoError(t, err)
		require.Equal(t, corestore.Target(first), a.localRefs(t, rel)["refs/heads/main"], "node-a is behind until it serves a request")

		tip, err = onA.ResolveRevision(a.ctx, "main")
		require.NoError(t, err)
		require.Equal(t, second, tip, "node-a serves the write of node-b")
		content, err = onA.ReadFile(a.ctx, "main", "b.txt")
		require.NoError(t, err)
		require.Equal(t, "written on node-b\n", string(content))

		// Both nodes serve the changelog from the database: the same dense log,
		// with the actor of each write.
		logA := allChangesets(a.ctx, t, onA)
		logB := allChangesets(b.ctx, t, onB)
		requireDense(t, logA)
		require.Len(t, logA, 2)
		require.Equal(t, len(logA), len(logB))
		for i := range logA {
			require.Equal(t, logA[i].GetNewOid(), logB[i].GetNewOid(), "entry %d", i)
			require.Equal(t, "refs/heads/main", string(logA[i].GetReference()))
		}
		require.Equal(t, workerName(0), logA[0].GetActor())
		require.Equal(t, first, logA[0].GetNewOid())
		require.Equal(t, workerName(1), logA[1].GetActor())
		require.Equal(t, second, logA[1].GetNewOid())

		a.requireConsistent(t, rel)
		b.requireConsistent(t, rel)
	})
}

// TestStorageConcurrentLandsAcrossNodes lands changes of several agents on one
// branch at the same time, spread over two (unbatched) and three (batched)
// nodes. The nodes race in the database: a land that loses the
// compare-and-swap is retried after its node caught up. No update is lost,
// history is linear, and every landed request is in the database changelog
// exactly once, with its agent.
func TestStorageConcurrentLandsAcrossNodes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc    string
		nodes   int
		batched bool
	}{
		{desc: "two nodes, unbatched", nodes: 2},
		{desc: "three nodes, batched", nodes: 3, batched: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
				runConcurrentLandsAcrossNodes(t, dialect, tc.nodes, tc.batched)
			})
		})
	}
}

func runConcurrentLandsAcrossNodes(t *testing.T, dialect sqlstore.Dialect, nodes int, batched bool) {
	t.Helper()

	workers := 4 * nodes
	graft, tokens := workerGraft(workers)
	if !batched {
		graft = unbatched(graft)
	}
	cluster := newStorageCluster(t, dialect, corestore.ModeAuthoritative, graft)
	for i := 0; i < nodes; i++ {
		cluster.addNode(t)
	}
	nodeOf := func(worker int) storageNode { return cluster.nodes[worker%nodes] }

	rel := cluster.nodes[0].createRepository(t).GetRelativePath()
	repos := make([]*sdk.Repo, workers)
	for i := range repos {
		repos[i] = nodeOf(i).agent(t, rel, tokens[workerName(i)])
	}

	base, err := commitFile(nodeOf(0).ctx, repos[0], workerName(0), "main", "", "README.md", "concurrent lands\n")
	require.NoError(t, err)

	// Every change is made on the node its agent lands from, from the same
	// base, before any land: the lands race for main.
	commits := make([]string, workers)
	for i := range repos {
		commits[i], err = commitFile(nodeOf(i).ctx, repos[i], workerName(i), "work/"+workerName(i), base,
			fmt.Sprintf("workers/%s.txt", workerName(i)), fmt.Sprintf("written by %s on %s\n", workerName(i), nodeOf(i).name))
		require.NoError(t, err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]*sdk.LandResult, workers)
	errs := make([]error, workers)
	for i := range repos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// The actor is the agent of the token, whatever user the request names.
			results[i], errs[i] = landOn(nodeOf(i).ctx, repos[i], "main", commits[i], false)
		}(i)
	}
	close(start)
	wg.Wait()

	retried := 0
	for i := range errs {
		require.NoError(t, errs[i], "land of %s on %s", workerName(i), nodeOf(i).name)
		require.False(t, results[i].AlreadyLanded, "land of %s", workerName(i))
		if results[i].Attempts > 1 {
			retried++
		}
	}
	t.Logf("%d of %d lands needed more than one attempt", retried, workers)

	// Every node serves the same tip.
	tip, err := repos[0].ResolveRevision(nodeOf(0).ctx, "main")
	require.NoError(t, err)
	for i := 1; i < nodes; i++ {
		got, err := repos[i].ResolveRevision(nodeOf(i).ctx, "main")
		require.NoError(t, err)
		require.Equal(t, tip, got, "%s serves the database tip", nodeOf(i).name)
	}

	// No lost update: the tip has every change in a linear history.
	a := cluster.nodes[0]
	files := a.git(t, rel, "ls-tree", "-r", "--name-only", "main")
	for i := 0; i < workers; i++ {
		require.Contains(t, files, fmt.Sprintf("workers/%s.txt", workerName(i)))
	}
	require.Empty(t, a.git(t, rel, "rev-list", "--merges", "main"), "history must be linear")
	require.Equal(t, fmt.Sprint(workers+1), a.git(t, rel, "rev-list", "--count", "main"))

	// Every landed request is in the database changelog exactly once.
	changesets := allChangesets(nodeOf(0).ctx, t, repos[0])
	requireDense(t, changesets)
	if batched {
		members := landBatchMembers(nodeOf(0).ctx, t, repos[0])
		require.Len(t, members, workers, "the land members are exactly the landed requests")
		for i, commit := range commits {
			require.Equal(t, workerName(i), members[commit], "change %d is recorded once, for its agent", i)
		}
	} else {
		lands := landsOn(changesets, "refs/heads/main")
		require.Len(t, lands, workers)
		actors := make([]string, 0, len(lands))
		for _, c := range lands {
			actors = append(actors, c.GetActor())
		}
		expected := make([]string, 0, workers)
		for i := 0; i < workers; i++ {
			expected = append(expected, workerName(i))
		}
		sort.Strings(actors)
		require.Equal(t, expected, actors, "each agent is the actor of exactly one land")
	}

	// Every node reads the same log from the database.
	for i := 1; i < nodes; i++ {
		require.Len(t, allChangesets(nodeOf(i).ctx, t, repos[i]), len(changesets), "%s", nodeOf(i).name)
	}

	for _, n := range cluster.nodes {
		n.requireConsistent(t, rel)
	}
}

// TestStorageFreshNodeHydrates starts a node with an empty disk after a
// repository has history. Its first request hydrates a complete reftable copy
// from the database and the bucket; it then serves reads and writes like any
// other node.
func TestStorageFreshNodeHydrates(t *testing.T) {
	t.Parallel()

	forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		graft, tokens := workerGraft(2)
		cluster := newStorageCluster(t, dialect, corestore.ModeAuthoritative, unbatched(graft))
		a := cluster.addNode(t)

		rel := a.createRepository(t).GetRelativePath()
		onA := a.agent(t, rel, tokens[workerName(0)])

		tip := ""
		for i := 0; i < 3; i++ {
			var err error
			tip, err = commitFile(a.ctx, onA, workerName(0), "main", tip, fmt.Sprintf("history/%d.txt", i), fmt.Sprintf("step %d\n", i))
			require.NoError(t, err)
		}
		feature, err := commitFile(a.ctx, onA, workerName(0), "feature/x", tip, "feature.txt", "feature\n")
		require.NoError(t, err)

		// The new node joins with an empty storage directory.
		fresh := cluster.addNode(t)
		require.NoDirExists(t, fresh.repoPath(rel))
		onFresh := fresh.agent(t, rel, tokens[workerName(1)])

		got, err := onFresh.ResolveRevision(fresh.ctx, "main")
		require.NoError(t, err)
		require.Equal(t, tip, got)
		got, err = onFresh.ResolveRevision(fresh.ctx, "feature/x")
		require.NoError(t, err)
		require.Equal(t, feature, got)
		for i := 0; i < 3; i++ {
			content, err := onFresh.ReadFile(fresh.ctx, "main", fmt.Sprintf("history/%d.txt", i))
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("step %d\n", i), string(content))
		}

		// The hydrated copy is a reftable repository with the whole history and a
		// watermark at the database head.
		require.Equal(t, "reftable", fresh.git(t, rel, "rev-parse", "--show-ref-format"))
		require.Equal(t, "4", fresh.git(t, rel, "rev-list", "--count", "feature/x"))
		watermark, ok, err := gitops.ReadWatermark(fresh.repoPath(rel))
		require.NoError(t, err)
		require.True(t, ok)
		rec := fresh.record(t, rel)
		require.Equal(t, corestore.Watermark{RepoID: rec.ID, Seq: rec.HeadSeq}, watermark)
		fresh.requireConsistent(t, rel)

		// The fresh node writes; the first node reads it.
		written, err := commitFile(fresh.ctx, onFresh, workerName(1), "main", tip, "fresh.txt", "from the fresh node\n")
		require.NoError(t, err)
		got, err = onA.ResolveRevision(a.ctx, "main")
		require.NoError(t, err)
		require.Equal(t, written, got)

		a.requireConsistent(t, rel)
		fresh.requireConsistent(t, rel)
	})
}

// TestStorageCreateOnOneNodeRemoveOnAnother creates a repository on node A,
// lets node B cache it, and removes it on B. The database forgets it, A's
// cached copy is treated as the stale cache of a removed repository (moved
// away, reported missing), and the repository can be created again with a new
// ID that both nodes serve.
func TestStorageCreateOnOneNodeRemoveOnAnother(t *testing.T) {
	t.Parallel()

	forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		graft, tokens := workerGraft(1)
		cluster := newStorageCluster(t, dialect, corestore.ModeAuthoritative, unbatched(graft))
		a := cluster.addNode(t)
		b := cluster.addNode(t)

		rel := a.createRepository(t).GetRelativePath()
		oldID := a.record(t, rel).ID
		onA := a.agent(t, rel, tokens[workerName(0)])
		onB := b.agent(t, rel, tokens[workerName(0)])

		written, err := commitFile(a.ctx, onA, workerName(0), "main", "", "README.md", "doomed\n")
		require.NoError(t, err)
		got, err := onB.ResolveRevision(b.ctx, "main")
		require.NoError(t, err)
		require.Equal(t, written, got)
		require.DirExists(t, b.repoPath(rel))

		repositoriesOn := func(n storageNode) gitalypb.RepositoryServiceClient {
			return gitalypb.NewRepositoryServiceClient(n.conn(t))
		}
		exists := func(t *testing.T, n storageNode) bool {
			t.Helper()
			resp, err := repositoriesOn(n).RepositoryExists(n.ctx, &gitalypb.RepositoryExistsRequest{Repository: n.repository(rel)})
			require.NoError(t, err)
			return resp.GetExists()
		}

		_, err = repositoriesOn(b).RemoveRepository(b.ctx, &gitalypb.RemoveRepositoryRequest{Repository: b.repository(rel)})
		require.NoError(t, err)

		_, err = b.core.Meta.GetRepository(b.ctx, corestore.KeyOf(b.repository(rel)))
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound, "the database forgets the repository")
		require.NoDirExists(t, b.repoPath(rel))
		require.DirExists(t, a.repoPath(rel), "node-a still has its cached copy")

		// Node-a's copy is a stale cache: it is moved away and the repository is
		// missing on node-a too.
		require.False(t, exists(t, a))
		require.NoDirExists(t, a.repoPath(rel))
		_, err = onA.ResolveRevision(a.ctx, "main")
		require.Equal(t, codes.NotFound, sdk.Code(err), "got %v", err)
		require.False(t, exists(t, b))

		// The repository can be created again, on node-a; it is a new repository.
		_, err = repositoriesOn(a).CreateRepository(a.setupCtx, &gitalypb.CreateRepositoryRequest{Repository: a.repository(rel)})
		require.NoError(t, err)
		newID := a.record(t, rel).ID
		require.NotEqual(t, oldID, newID, "a repository ID is never reused")
		require.True(t, exists(t, b), "node-b serves the new repository")
		_, err = onB.ResolveRevision(b.ctx, "main")
		require.Error(t, err, "the new repository has nothing of the removed one")
		require.NotContains(t, b.localRefs(t, rel), "refs/heads/main")

		again, err := commitFile(b.ctx, onB, workerName(0), "main", "", "README.md", "reborn\n")
		require.NoError(t, err)
		require.NotEqual(t, written, again)
		content, err := onA.ReadFile(a.ctx, "main", "README.md")
		require.NoError(t, err)
		require.Equal(t, "reborn\n", string(content))

		a.requireConsistent(t, rel)
		b.requireConsistent(t, rel)
	})
}
