package operations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// Tests of the server side of the design step (PLAN10 15.10): the intent of OURS goes to the resolver, and
// the composition report of a model's resolution goes to the agent with the proposal.

// TestUserLand_theResolverGetsTheIntentOfOurs: each file of the request carries the commit message that
// explains OURS' change to it: the newest commit between the merge base and the tip that touched the
// path. A path that only another commit touched gets the message of that commit, not the tip's.
func TestUserLand_theResolverGetsTheIntentOfOurs(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())
	rcfg := enabledResolver(argv)
	fx := newResolveFixtureWith(t, rcfg,
		entries("a", "base\n", "b", "base\n", "c", "c\n"),
		entries("a", "ours\n", "b", "ours\n", "c", "c\n"),
		entries("a", "theirs\n", "b", "theirs\n", "c", "c\n", "d", "d\n"),
	)
	// The tip's history on the path: a was changed by an older commit, then b by the tip.
	older := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base), gittest.WithMessage("rework a for the new loader\n\nbody of the message"),
		gittest.WithTreeEntries(entries("a", "ours\n", "b", "base\n", "c", "c\n")...))
	tip := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(older), gittest.WithMessage("tune b"), gittest.WithBranch("main"),
		gittest.WithTreeEntries(entries("a", "ours\n", "b", "ours\n", "c", "c\n")...))
	require.NotEqual(t, fx.tip, tip)

	_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	require.NoError(t, err)

	req := resolverRequest(t, dir, 1)
	messages := map[string]string{}
	for _, f := range req.Files {
		messages[f.Path] = f.OursMessage
	}
	require.Equal(t, "rework a for the new loader\n\nbody of the message", messages["a"], "the commit that touched a, not the tip's")
	require.Equal(t, "tune b", messages["b"])
}

// TestUserLand_theReportGoesWithTheProposal: the composition report of a model's resolution is the start
// of the explanation of its proposal, cut to the size of a reason.
func TestUserLand_theReportGoesWithTheProposal(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	answer := modelAnswer()
	answer.Files[0].Report = []string{`region 1: drops THEIRS' change at base line 6: the line "T6" it adds is missing`, "1 contested changes resolved freely: nothing line-level to verify"}
	writeAnswer(t, dir, answer)
	fx := newResolveFixture(t, enabledResolver(argv))

	_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	requireMergeConflict(t, err)
	pkg := landConflictDetail(err)
	a := packageFile(t, pkg, "a")
	require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, a.GetOutcome())
	explanation := a.GetProposal().GetExplanation()
	require.True(t, strings.HasPrefix(explanation, `report: region 1: drops THEIRS' change at base line 6`), explanation)
	require.Contains(t, explanation, "; 1 contested changes resolved freely")
	require.Contains(t, explanation, ". patched by the model", "the explanation of the answer follows the report")
	// The other file has no report: its explanation is the answer's.
	require.Equal(t, "patched by the model", packageFile(t, pkg, "b").GetProposal().GetExplanation())
}

func TestLandProposalExplanation(t *testing.T) {
	t.Parallel()

	long := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		long = append(long, strings.Repeat("x", 40))
	}
	got := landProposalExplanation(resolver.ResponseFile{Report: long}, "from the answer")
	require.LessOrEqual(t, len(got), landPackageReasonBytes)
	require.True(t, strings.HasPrefix(got, "report: xxxx"))
	require.Equal(t, "from the answer", landProposalExplanation(resolver.ResponseFile{}, "from the answer"))
	require.Equal(t, "report: one; two. from the answer", landProposalExplanation(resolver.ResponseFile{Report: []string{"one", "two"}}, "from the answer"))
}
