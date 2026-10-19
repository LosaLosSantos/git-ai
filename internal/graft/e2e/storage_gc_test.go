package e2e

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// TestStorageGCAcrossNodes runs the compaction of milestone 3 on a cluster of
// real servers sharing one database and one bucket (doc/graft/PLAN7.md): node
// A checkpoints a repository written on two nodes, node B collects the packs
// the checkpoint covers once it is gc_grace old, and a node that joins
// afterwards hydrates from the checkpoint and serves every write, before and
// after the collection. Orphan packs and removed repositories are collected
// too.
func TestStorageGCAcrossNodes(t *testing.T) {
	forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		graft, tokens := workerGraft(1)
		cluster := newStorageCluster(t, dialect, corestore.ModeAuthoritative, unbatched(graft))
		// A short grace, so that the test sees packs collected. The writers'
		// deadline (grace - margin) stays long enough for an upload under load.
		cluster.adjust = func(core *corestore.Core) {
			core.GCGrace = 2 * time.Second
			core.GCMargin = 200 * time.Millisecond
		}
		a := cluster.addNode(t)
		b := cluster.addNode(t)

		rel := a.createRepository(t).GetRelativePath()
		onA := a.agent(t, rel, tokens[workerName(0)])
		onB := b.agent(t, rel, tokens[workerName(0)])

		written := map[string]string{}
		base, err := commitFile(a.ctx, onA, workerName(0), "main", "", "README.md", "base\n")
		require.NoError(t, err)
		written["main"] = base
		for i, repo := range []*sdk.Repo{onA, onB, onA, onB} {
			branch := fmt.Sprintf("topic/%d", i)
			commit, err := commitFile(a.ctx, repo, workerName(0), branch, base, branch+".txt", branch+"\n")
			require.NoError(t, err)
			written[branch] = commit
		}
		before := a.referencedPacks(t, rel)
		require.GreaterOrEqual(t, len(before), len(written), "one pack per write")

		cp, err := a.operator(t, rel).StorageCheckpoint(a.ctx)
		require.NoError(t, err)
		require.True(t, cp.Created)
		require.Equal(t, a.record(t, rel).HeadSeq, cp.Sequence)

		// Node B collects once the checkpoint is gc_grace old.
		var collected *sdk.StorageGC
		require.Eventually(t, func() bool {
			res, err := b.operator(t, rel).StorageGC(b.ctx, false)
			require.NoError(t, err)
			collected = res
			return res.Boundary == cp.Sequence && res.CoveredDeleted > 0
		}, 60*time.Second, 200*time.Millisecond)
		require.Equal(t, uint32(len(before)), collected.CoveredDeleted, "every changeset pack is covered by the checkpoint")
		inBucket := bucketPacks(t, cluster.bucketURL)
		for key := range before {
			require.False(t, inBucket[key], "covered pack %s deleted", key)
		}

		// A node that joins now has only the checkpoint to hydrate from.
		c := cluster.addNode(t)
		onC := c.agent(t, rel, tokens[workerName(0)])
		for branch, commit := range written {
			got, err := onC.ResolveRevision(c.ctx, branch)
			require.NoError(t, err, "branch %s", branch)
			require.Equal(t, commit, got, "branch %s", branch)
		}
		content, err := onC.ReadFile(c.ctx, "topic/3", "topic/3.txt")
		require.NoError(t, err)
		require.Equal(t, "topic/3\n", string(content))
		c.requireConsistent(t, rel)

		// Writes go on after the collection, and every node serves them.
		after, err := commitFile(b.ctx, onB, workerName(0), "after-gc", base, "after.txt", "after the collection\n")
		require.NoError(t, err)
		for _, node := range []struct {
			node storageNode
			repo *sdk.Repo
		}{{a, onA}, {b, onB}, {c, onC}} {
			got, err := node.repo.ResolveRevision(node.node.ctx, "after-gc")
			require.NoError(t, err, node.node.name)
			require.Equal(t, after, got, node.node.name)
			node.node.requireConsistent(t, rel)
		}

		// An orphan (the pack of a write that died before its database commit)
		// is deleted gc_grace after a collection observed it.
		orphan := []byte("the pack of a write that died")
		orphanKey := corestore.NewPackKey(a.record(t, rel).ID, sha256.Sum256(orphan))
		require.NoError(t, a.core.Blobs.Put(a.ctx, orphanKey, bytes.NewReader(orphan)))
		require.Eventually(t, func() bool {
			res, err := a.operator(t, rel).StorageGC(a.ctx, false)
			require.NoError(t, err)
			return res.OrphansDeleted == 1
		}, 60*time.Second, 200*time.Millisecond)
		require.False(t, bucketPacks(t, cluster.bucketURL)[orphanKey])

		// A removed repository's objects go once its tombstone is gc_grace old.
		repoID := a.record(t, rel).ID
		_, err = gitalypb.NewRepositoryServiceClient(a.conn(t)).RemoveRepository(a.setupCtx, &gitalypb.RemoveRepositoryRequest{Repository: a.repository(rel)})
		require.NoError(t, err)
		client, err := sdk.Dial(a.ctx, a.cfg.SocketPath, sdk.WithStorage(a.cfg.Storages[0].Name), sdk.WithToken(sharedTestToken))
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })
		require.Eventually(t, func() bool {
			res, err := client.StorageGCRemoved(a.ctx, "", false)
			require.NoError(t, err)
			return res.Repositories == 1
		}, 60*time.Second, 200*time.Millisecond)
		prefix := fmt.Sprintf("repos/%d/", repoID)
		for key := range bucketPacks(t, cluster.bucketURL) {
			require.NotContains(t, key, prefix, "an object of the removed repository is left")
		}
	})
}

// TestStorageCheckpointBackfillsReviewRoots: the review roots are a
// precondition of a checkpoint, and so of garbage collection (integrated
// review m3). An open review written before the REVIEW ROOTS RULE has no
// keep-around references; when its branch is gone, only a changeset pack holds
// its head commit. The checkpoint build writes the references first, so the
// checkpoint holds the commit and the collection of the packs it covers loses
// nothing: a node that joins afterwards still has it.
func TestStorageCheckpointBackfillsReviewRoots(t *testing.T) {
	forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		graft, tokens := workerGraft(1)
		cluster := newStorageCluster(t, dialect, corestore.ModeAuthoritative, unbatched(graft))
		cluster.adjust = func(core *corestore.Core) {
			core.GCGrace = 2 * time.Second
			core.GCMargin = 200 * time.Millisecond
		}
		a := cluster.addNode(t)

		rel := a.createRepository(t).GetRelativePath()
		onA := a.agent(t, rel, tokens[workerName(0)])
		base, err := commitFile(a.ctx, onA, workerName(0), "main", "", "README.md", "base\n")
		require.NoError(t, err)
		head, err := commitFile(a.ctx, onA, workerName(0), "topic", base, "topic.txt", "topic\n")
		require.NoError(t, err)
		_, err = gitalypb.NewRefServiceClient(a.conn(t)).DeleteRefs(a.ctx, &gitalypb.DeleteRefsRequest{
			Repository: a.repository(rel),
			Refs:       [][]byte{[]byte("refs/heads/topic")},
		})
		require.NoError(t, err)
		require.True(t, a.dbRefs(t, rel)["refs/heads/topic"].Absent())

		// A review from before the rule: recorded without its roots.
		repoID := a.record(t, rel).ID
		_, err = a.core.Meta.InsertReview(a.ctx, repoID, func(id string) (*gitalypb.Review, error) {
			return &gitalypb.Review{Id: id, TargetBranch: []byte("main"), HeadCommitId: head, BaseCommitId: base, Title: []byte("before the rule")}, nil
		}, false)
		require.NoError(t, err)

		cp, err := a.operator(t, rel).StorageCheckpoint(a.ctx)
		require.NoError(t, err)
		require.True(t, cp.Created)
		snapshot, err := a.core.Meta.CheckpointRefs(a.ctx, repoID, cp.Sequence)
		require.NoError(t, err)
		require.Equal(t, corestore.Target(head), snapshot[corestore.KeepAroundRef(head)], "the checkpoint holds the review head")
		require.Equal(t, corestore.Target(base), snapshot[corestore.KeepAroundRef(base)], "the checkpoint holds the review base")

		covered := a.referencedPacks(t, rel)
		require.Eventually(t, func() bool {
			res, err := a.operator(t, rel).StorageGC(a.ctx, false)
			require.NoError(t, err)
			return res.Boundary == cp.Sequence && res.CoveredDeleted > 0
		}, 60*time.Second, 200*time.Millisecond)
		inBucket := bucketPacks(t, cluster.bucketURL)
		for key := range covered {
			require.False(t, inBucket[key], "covered pack %s deleted", key)
		}

		// Only the checkpoint is left to hydrate from, and it has the commit.
		b := cluster.addNode(t)
		onB := b.agent(t, rel, tokens[workerName(0)])
		got, err := onB.ResolveRevision(b.ctx, "main")
		require.NoError(t, err)
		require.Equal(t, base, got)
		require.Equal(t, "commit", b.git(t, rel, "cat-file", "-t", head))
		b.requireConsistent(t, rel)
	})
}
