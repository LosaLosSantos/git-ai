package operations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// Tests of the server side of the design step (PLAN10 15.11): the composition report of a model's
// resolution goes to the agent with the proposal.

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
