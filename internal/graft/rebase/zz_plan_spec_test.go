package rebase

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/ancestry"
)

// Rule 1: an empty range (nil or empty) means the commit is already landed.
func TestSpecEmptyRangeIsAlreadyLanded(t *testing.T) {
	t.Parallel()

	tip, commit := git.ObjectID("tip"), git.ObjectID("commit")

	plan, err := planLanding(tip, commit, nil)
	require.NoError(t, err)
	require.Equal(t, LandAlreadyLanded, plan.Kind())

	plan, err = planLanding(tip, commit, []ancestry.RangeCommit{})
	require.NoError(t, err)
	require.Equal(t, LandAlreadyLanded, plan.Kind())
}

// Rule 2: tip is a parent of a range commit, so the plan is a fast-forward.
func TestSpecTipAsParentIsFastForward(t *testing.T) {
	t.Parallel()

	tip, commit := git.ObjectID("tip"), git.ObjectID("commit")
	rng := []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"tip"}},
	}

	plan, err := planLanding(tip, commit, rng)
	require.NoError(t, err)
	require.Equal(t, LandFastForward, plan.Kind())
}

// Rule 2: a fast-forward stays a fast-forward even when a range commit is a merge.
func TestSpecMergeWithTipParentIsFastForward(t *testing.T) {
	t.Parallel()

	tip, commit := git.ObjectID("tip"), git.ObjectID("commit")
	rng := []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"c1", "c2"}},
		{ID: "c1", Parents: []git.ObjectID{"tip"}},
		{ID: "c2", Parents: []git.ObjectID{"tip"}},
	}

	plan, err := planLanding(tip, commit, rng)
	require.NoError(t, err)
	require.Equal(t, LandFastForward, plan.Kind())
}

// Rule 3: when tip is not a parent of any range commit, the plan is diverged.
func TestSpecUnmergedRangeIsDiverged(t *testing.T) {
	t.Parallel()

	tip, commit := git.ObjectID("tip"), git.ObjectID("commit")
	rng := []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"c1"}},
		{ID: "c1", Parents: []git.ObjectID{"base"}},
	}

	plan, err := planLanding(tip, commit, rng)
	require.NoError(t, err)
	require.Equal(t, LandDiverged, plan.Kind())
}

// Rule 4: a merge in a diverged range makes Replay fail with ErrMergesInRange,
// including when the range also has a root (the merge check runs first).
func TestSpecMergeInDivergedRangeFails(t *testing.T) {
	t.Parallel()

	tip, commit := git.ObjectID("tip"), git.ObjectID("commit")

	mergeOnly := []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"c1", "c2"}},
		{ID: "c1", Parents: []git.ObjectID{"base"}},
		{ID: "c2", Parents: []git.ObjectID{"base"}},
	}
	plan, err := planLanding(tip, commit, mergeOnly)
	require.NoError(t, err)
	require.Equal(t, LandDiverged, plan.Kind())
	_, err = plan.Replay()
	require.ErrorIs(t, err, ErrMergesInRange)

	mergeAndRoot := []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"c1", "c2"}},
		{ID: "c1", Parents: []git.ObjectID{"base"}},
		{ID: "c2"},
	}
	plan, err = planLanding(tip, commit, mergeAndRoot)
	require.NoError(t, err)
	_, err = plan.Replay()
	require.ErrorIs(t, err, ErrMergesInRange)
}

// Rule 5: a diverged, merge-free range whose bottom commit is a root makes Replay fail with ErrUnrelated.
func TestSpecRootInDivergedRangeIsUnrelated(t *testing.T) {
	t.Parallel()

	tip, commit := git.ObjectID("tip"), git.ObjectID("commit")
	rng := []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"c1"}},
		{ID: "c1"},
	}

	plan, err := planLanding(tip, commit, rng)
	require.NoError(t, err)
	require.Equal(t, LandDiverged, plan.Kind())
	_, err = plan.Replay()
	require.ErrorIs(t, err, ErrUnrelated)
}

// Rule 6: Replay returns the chain oldest first, and the order of rng does not change the result.
func TestSpecReplayOrderIndependentOldestFirst(t *testing.T) {
	t.Parallel()

	tip, commit := git.ObjectID("tip"), git.ObjectID("commit")
	want := []git.ObjectID{"c1", "c2", "commit"}

	inOrder := []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"c2"}},
		{ID: "c2", Parents: []git.ObjectID{"c1"}},
		{ID: "c1", Parents: []git.ObjectID{"base"}},
	}
	shuffled := []ancestry.RangeCommit{
		{ID: "c1", Parents: []git.ObjectID{"base"}},
		{ID: commit, Parents: []git.ObjectID{"c2"}},
		{ID: "c2", Parents: []git.ObjectID{"c1"}},
	}

	for _, rng := range [][]ancestry.RangeCommit{inOrder, shuffled} {
		plan, err := planLanding(tip, commit, rng)
		require.NoError(t, err)
		todo, err := plan.Replay()
		require.NoError(t, err)
		require.Equal(t, want, todo)
	}
}

// Rule 7: Replay on a plan that is not diverged (already landed or fast-forward) returns an error.
func TestSpecReplayRejectsNonDivergedPlan(t *testing.T) {
	t.Parallel()

	tip, commit := git.ObjectID("tip"), git.ObjectID("commit")

	landed, err := planLanding(tip, commit, nil)
	require.NoError(t, err)
	_, err = landed.Replay()
	require.Error(t, err)

	fastForward := []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"tip"}},
	}
	ff, err := planLanding(tip, commit, fastForward)
	require.NoError(t, err)
	require.Equal(t, LandFastForward, ff.Kind())
	_, err = ff.Replay()
	require.Error(t, err)
}

// Rule 8: PlanLanding fails when a non-empty range does not contain commit, or lists an ID twice.
func TestSpecPlanLandingRejectsBadRange(t *testing.T) {
	t.Parallel()

	tip, commit := git.ObjectID("tip"), git.ObjectID("commit")

	missingCommit := []ancestry.RangeCommit{
		{ID: "c1", Parents: []git.ObjectID{"base"}},
	}
	_, err := planLanding(tip, commit, missingCommit)
	require.Error(t, err)

	duplicated := []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"base"}},
		{ID: commit, Parents: []git.ObjectID{"base"}},
	}
	_, err = planLanding(tip, commit, duplicated)
	require.Error(t, err)
}

// Rule 9: a single-commit diverged range replays to exactly that commit.
func TestSpecSingleCommitReplaysItself(t *testing.T) {
	t.Parallel()

	tip, commit := git.ObjectID("tip"), git.ObjectID("commit")
	rng := []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"base"}},
	}

	plan, err := planLanding(tip, commit, rng)
	require.NoError(t, err)
	require.Equal(t, LandDiverged, plan.Kind())
	todo, err := plan.Replay()
	require.NoError(t, err)
	require.Equal(t, []git.ObjectID{commit}, todo)
}
