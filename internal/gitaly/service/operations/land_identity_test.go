package operations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
)

// TestLandBatch_retryAfterALostResponse lands two changes by rebase in one batch, and then lands
// them again, as clients do whose responses were lost: the batch records the identity of every
// member (doc/graft/PLAN11.md), and each retried member answers already landed as the commit its
// first land put on the branch.
func TestLandBatch_retryAfterALostResponse(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchRetryAfterALostResponse)
}

func testLandBatchRetryAfterALostResponse(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupAgentLandService(t, ctx, agentBatchingGraft(nil))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(baseEntry))
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithParents(base),
		gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: "moved", Content: "moved"}))
	change := func(name string) git.ObjectID {
		return gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
			gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: name, Content: name}))
	}
	a, b := change("a"), change("b")

	first, errs := landInOrder(ctx, client, repoProto,
		agentLanding{agent: "agent-a", commit: a},
		agentLanding{agent: "agent-b", commit: b},
	)
	for i := range errs {
		require.NoError(t, errs[i])
		require.Equal(t, int32(2), first[i].GetBatchSize(), "both changes land in one batch")
		require.True(t, first[i].GetRewritten())
	}
	landedA, landedB := git.ObjectID(first[0].GetLandedCommitId()), git.ObjectID(first[1].GetLandedCommitId())

	// The batch lands its members in the order they reached the lane, which the test does not
	// fix: the tip is the commit of the last of them.
	tip := gittest.ResolveRevision(t, cfg, repoPath, git.DefaultRef.String())
	require.Contains(t, []git.ObjectID{landedA, landedB}, tip)

	entry := landEntry(t, repoPath, tip)
	require.Equal(t, changelog.OperationLandBatch, entry.Operation)
	require.ElementsMatch(t, []changelog.Land{
		{Original: a.String(), Landed: landedA.String()},
		{Original: b.String(), Landed: landedB.String()},
	}, entry.Lands)
	require.Equal(t, tip.String(), entry.Lands[len(entry.Lands)-1].Landed, "the lands are in the order of the batch")

	second, errs := landInOrder(ctx, client, repoProto,
		agentLanding{agent: "agent-a", commit: a},
		agentLanding{agent: "agent-b", commit: b},
	)
	for i, landed := range []git.ObjectID{landedA, landedB} {
		require.NoError(t, errs[i])
		requireLandedEarlier(t, second[i], entry, landed, "")
		require.Equal(t, tip.String(), second[i].GetBranchUpdate().GetCommitId())
	}
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)
}

// TestLandBatch_retryOfABatchOfOne retries a change that landed alone in its lane, which publishes
// it as a land of its own (UpdateSolo): its identity is recorded, and the retry answers from it.
func TestLandBatch_retryOfABatchOfOne(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchRetryOfABatchOfOne)
}

func testLandBatchRetryOfABatchOfOne(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupAgentLandService(t, ctx, batchingGraft())
	fx := newRetryFixture(t, ctx, cfg)

	request := landRequest(fx.repoProto, git.DefaultBranch, fx.change)
	first, err := client.UserLand(ctx, request)
	require.NoError(t, err)
	require.Equal(t, int32(1), first.GetBatchSize())
	require.True(t, first.GetRewritten())
	landed := git.ObjectID(first.GetLandedCommitId())

	entry := landEntry(t, fx.repoPath, landed)
	require.Equal(t, changelogOperationLand, entry.Operation)
	require.Equal(t, []changelog.Land{{Original: fx.change.String(), Landed: landed.String()}}, entry.Lands)

	second, err := client.UserLand(ctx, request)
	require.NoError(t, err)
	requireLandedEarlier(t, second, entry, landed, "")
	requireBranchAt(t, cfg, fx.repoPath, git.DefaultBranch, landed)
}
