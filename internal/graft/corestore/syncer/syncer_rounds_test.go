package syncer_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestSyncerSeedsInTheBackground verifies that the seed of a hydration from a
// checkpoint, a whole repository, is not bound to a sync round
// (doc/graft/PLAN7.md, "Seeding in the background"): while it runs, every
// round answers Unavailable and starts no second seed; once it is done, the
// next round catches up from the checkpoint. The seed is held by the test
// until several rounds have ended, as a pack too large for one round would.
func TestSyncerSeedsInTheBackground(t *testing.T) {
	t.Parallel()

	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c := a.writeCommit(t, src)
	repo := a.register(t, a.key(localRelPath), src, c)
	c = a.advanceMain(t, repo.ID, src, c)
	cp := a.checkpoint(t, repo.ID, src)
	for i := 0; i < 3; i++ {
		c = a.advanceMain(t, repo.ID, src, c)
	}

	b := a.secondNode(t, sqlstore.SQLite)
	b.core.SyncTimeout = 400 * time.Millisecond
	seeding := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	b.sy.SetTestHook(func(point string) {
		if point == "seed" {
			once.Do(func() { close(seeding) })
			<-release
		}
	})

	for i := 0; i < 3; i++ {
		err := b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath))
		require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
		require.ErrorContains(t, err, fmt.Sprintf("being seeded from its checkpoint at changeset %d", cp.Seq))
	}
	<-seeding
	require.Equal(t, float64(3), syncCount(t, b.sy, "seeding"))

	close(release)
	require.Eventually(t, func() bool {
		return b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)) == nil
	}, 30*time.Second, 10*time.Millisecond, "the round after the seed catches up")
	require.Equal(t, float64(1), counter(t, b.sy, "graft_storage_sync_checkpoint_seeds_total", ""), "one seed for every round")

	repo, err := b.core.Meta.GetRepository(b.ctx, repo.Key)
	require.NoError(t, err)
	b.verifySynced(t, repo)
}

// TestSyncerResumesAHydrationAcrossRounds verifies resumable rounds
// (doc/graft/PLAN7.md, "Resumable rounds"): a round that runs out of its budget
// keeps what it did. A hydration that does not reach the head installs the
// repository at the changeset it reached; the next round catches up from there.
// Each round answers Unavailable until the head is reached, so a partial copy
// is never served as current.
func TestSyncerResumesAHydrationAcrossRounds(t *testing.T) {
	t.Parallel()

	e := newEnv(t, envOptions{})
	src := e.newSource(t)
	c := e.writeCommit(t, src)
	repo := e.register(t, e.key(localRelPath), src, c)
	for i := 0; i < 9; i++ {
		c = e.advanceMain(t, repo.ID, src, c)
	}
	repo, err := e.core.Meta.GetRepository(e.ctx, repo.Key)
	require.NoError(t, err)
	require.EqualValues(t, 10, repo.HeadSeq)

	// The changesets were written on node-a; this node joins with an empty
	// disk. (Changesets of the node itself after a range are pulled into it:
	// the local repository may hold their effects, see newerLocalWrites.)
	e.core.NodeID = "node-b"
	e.sy.SetRoundLimit(4)
	for _, reached := range []uint64{4, 8} {
		err := e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath))
		require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
		require.ErrorContains(t, err, "at changeset")

		wm, ok, err := e.sy.Applied(e.ctx, e.pbRepo(localRelPath))
		require.NoError(t, err)
		require.True(t, ok, "the round installed what it reached")
		require.Equal(t, repo.ID, wm.RepoID)
		require.Equal(t, reached, wm.Seq)
	}

	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	require.Equal(t, float64(2), syncCount(t, e.sy, "partial"))
	require.Equal(t, float64(0), syncCount(t, e.sy, "hydrated"), "the first round's hydration was partial")
	require.Equal(t, float64(1), syncCount(t, e.sy, "caught_up"))
	e.verifySynced(t, repo)
}
