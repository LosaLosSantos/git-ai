package syncer_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
