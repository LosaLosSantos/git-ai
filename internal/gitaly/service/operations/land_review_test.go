package operations

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git/gittest"
)

// Tests of the review of m6 (9586fd2).

// TestResolveSlot_aPanickingResolverCallGivesItsSlotBack: a panic in the resolver call (recovered by the
// gRPC server) must not lose the resolve slot: a lost slot would refuse every later resolution.
func TestResolveSlot_aPanickingResolverCallGivesItsSlotBack(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())
	rcfg := enabledResolver(argv)
	rcfg.ResolveConcurrency = 1
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)
	s.landResolveHook = func() { panic("a resolution failed") }

	require.PanicsWithValue(t, "a resolution failed", func() {
		_, _, _ = s.landResolveAt(fx.ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer(), landVerdicts{})
	})
	require.Equal(t, 0, resolverCalls(t, dir), "the hook ran before the call")

	ctx, cancel := context.WithTimeout(fx.ctx, time.Second)
	defer cancel()
	slot, err := s.conflictResolver.AcquireResolveSlot(ctx)
	require.NoError(t, err, "the slot of the panicking call was given back")
	slot.Release()
}

// TestLandBatchMetrics_aLostRoundIsNotAnError is not parallel: the metrics are process-wide, so the
// difference of their values is only this land when no other batch runs meanwhile. A land whose files
// change under it is handed back by the lane for each round it loses (PLAN10 15.8): that is a
// needs_resolution, not an error. Losing k rounds gives k of them and one landed.
func TestLandBatchMetrics_aLostRoundIsNotAnError(t *testing.T) {
	dir, argv := resolverScript(t, answerByCall)
	writeAnswer(t, dir, validAnswer())
	writeAnswerN(t, dir, 2, answerOf("a"))
	writeAnswerN(t, dir, 3, answerOf("a"))
	graft := batchingGraft()
	graft.Land.Resolver = enabledResolver(argv)
	fx := newResolveFixtureIn(t, graft,
		entries("a", "base\n", "b", "base\n", "c", "c\n"),
		entries("a", "ours\n", "b", "ours\n", "c", "c\n"),
		entries("a", "theirs\n", "b", "theirs\n", "c", "c\n", "d", "d\n"),
	)
	// While the first and the second resolutions run, a writer changes a: the land loses two rounds.
	w1 := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip),
		gittest.WithTreeEntries(entries("a", "ours-1\n", "b", "ours\n", "c", "c\n")...))
	w2 := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(w1),
		gittest.WithTreeEntries(entries("a", "ours-2\n", "b", "ours\n", "c", "c\n")...))
	writeMoves(t, dir, fx, w1, w2)

	_, before := landBatchMetricCounts(t)
	resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	require.NoError(t, err)
	_, after := landBatchMetricCounts(t)

	require.Equal(t, 3, resolverCalls(t, dir), "two rounds were lost, the third landed")
	require.Equal(t, w2.String(), resp.GetOldTip())
	delta := func(outcome string) float64 { return after[outcome] - before[outcome] }
	require.Equal(t, 2.0, delta(landBatchOutcomeNeedsResolution), "one per lost round")
	require.Equal(t, 1.0, delta(landBatchOutcomeLanded))
	require.Equal(t, 0.0, delta(landBatchOutcomeError), "a lost round is not an error")
	require.Equal(t, 0.0, delta(landBatchOutcomeConflict))
}

// TestUserLand_aFailureThatIsNoConflictIsNotAMergeConflict: only a conflict that the resolution left
// unresolved is a merge conflict. When the conflicts cannot even be listed (here, the change's tree is
// missing from the repository), the land fails with the cause, as an internal error.
func TestUserLand_aFailureThatIsNoConflictIsNotAMergeConflict(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())
	fx := newResolveFixture(t, enabledResolver(argv))
	tree := gittest.ResolveRevision(t, fx.cfg, fx.repoPath, fx.commit.String()+"^{tree}").String()
	require.NoError(t, os.Remove(filepath.Join(fx.repoPath, "objects", tree[:2], tree[2:])))

	_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "resolving the conflicts of the land")
	require.False(t, isLandMergeConflict(err), "no merge conflict was found: the error is the cause")
	require.Equal(t, 0, resolverCalls(t, dir))
	requireBranchAt(t, fx.cfg, fx.repoPath, "main", fx.tip)
}

// TestUserLand_aConflictWithoutAConflictingFileAtTheLanesTip: while the land is resolved, the branch moves
// to a tip where the merge is unclean without a conflicting file (a directory rename split). No file can
// be resolved there, so asking again for files would only repeat the round until the deadline and end with
// "files kept changing:" and no name. The land is refused at once, with the package that says why.
func TestUserLand_aConflictWithoutAConflictingFileAtTheLanesTip(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, answerByCall)
	writeAnswer(t, dir, answerOf("m"))
	dirsOf := func(x, y string) []gittest.TreeEntry {
		return []gittest.TreeEntry{
			{Mode: "100644", Path: x, Content: "one\none\none\none\n"},
			{Mode: "100644", Path: y, Content: "two\ntwo\ntwo\ntwo\n"},
		}
	}
	with := func(m string, entries ...gittest.TreeEntry) []gittest.TreeEntry {
		return append(entries, gittest.TreeEntry{Mode: "100644", Path: "m", Content: m})
	}
	// One round: the lane's tip is the last thing the land sees, as when the rounds or the deadline
	// run out on it. Another round would pre-resolve at the new tip and find the same state itself.
	rcfg := enabledResolver(argv)
	rcfg.ResolveRounds = 1
	fx := newResolveFixtureWith(t, rcfg,
		with("base\n", dirsOf("A/x", "A/y")...),
		with("ours\n", dirsOf("A/x", "A/y")...),
		with("theirs\n", append(dirsOf("A/x", "A/y"), gittest.TreeEntry{Mode: "100644", Path: "A/z", Content: "new\n"})...),
	)
	// The writer splits the directory A over B and C and takes the change's m: no file conflicts.
	split := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip),
		gittest.WithTreeEntries(nestTreeEntries(t, fx.cfg, fx.repoPath, with("theirs\n", dirsOf("B/x", "C/y")...))...))
	writeMoves(t, dir, fx, split)

	_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	requireMergeConflict(t, err)
	require.NotContains(t, err.Error(), "files kept changing")
	pkg := landConflictDetail(err)
	require.NotNil(t, pkg)
	require.Contains(t, pkg.GetReason(), "conflicts without a conflicting file")
	require.Equal(t, 1, resolverCalls(t, dir), "no round was repeated")
	requireBranchAt(t, fx.cfg, fx.repoPath, "main", split)
}
