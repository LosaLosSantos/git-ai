package operations

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// skewedReviewHistory writes the history that the review of phase 4b reported, on the default
// branch: a root R dated 2026-01, A on it dated 2026-02, and ten commits on A dated 2025-06, the
// last of which is the tip. git rev-list tip..A lists A and R although the tip reaches both. It
// also writes C on A, dated after A, which no branch has. Each commit adds a file of its own.
func skewedReviewHistory(t *testing.T, cfg config.Cfg, repoPath string) (a, c, tip git.ObjectID) {
	t.Helper()

	var entries []gittest.TreeEntry
	write := func(name string, date time.Time, parents ...git.ObjectID) git.ObjectID {
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: name, Content: name + "\n"})
		return gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithParents(parents...),
			gittest.WithTreeEntries(entries...),
			gittest.WithMessage(name),
			gittest.WithAuthorDate(date),
			gittest.WithCommitterDate(date),
		)
	}

	root := write("r", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	a = write("a", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), root)
	tip = a
	for i := 0; i < 10; i++ {
		tip = write(fmt.Sprintf("x%d", i), time.Date(2025, 6, 1, 0, 0, i, 0, time.UTC), tip)
	}
	gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/"+git.DefaultBranch, tip.String())

	// C is written on a copy of A's tree, not on the tip's.
	entries = entries[:2]
	c = write("c", time.Date(2026, 2, 1, 0, 1, 0, 0, time.UTC), a)

	listed := strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-list", tip.String()+".."+a.String())))
	require.ElementsMatch(t, []string{a.String(), root.String()}, listed, "git rev-list no longer shows the skew this test is about")

	return a, c, tip
}

// TestUserLand_skewedCommitterDates lands on the history of the review, with every strategy:
// landing A again (a retry) reports it as already landed and leaves the branch alone, and C, which
// diverges from the tip and shares A with it, lands as the strategy says, instead of failing as
// unrelated or merging a commit that is already on the branch.
func TestUserLand_skewedCommitterDates(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testUserLandSkewedCommitterDates)
}

func testUserLandSkewedCommitterDates(t *testing.T, ctx context.Context) {
	ctx, cfg, client := setupOperationsService(t, ctx)

	for _, strategy := range []gitalypb.UserLandRequest_Strategy{
		gitalypb.UserLandRequest_STRATEGY_REBASE_UNSPECIFIED,
		gitalypb.UserLandRequest_STRATEGY_MERGE,
		gitalypb.UserLandRequest_STRATEGY_FF_ONLY,
	} {
		t.Run(strategy.String(), func(t *testing.T) {
			repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
			repo := localrepo.NewTestRepo(t, cfg, repoProto)
			a, c, tip := skewedReviewHistory(t, cfg, repoPath)

			request := landRequest(repoProto, git.DefaultBranch, a)
			request.Strategy = strategy
			resp, err := client.UserLand(ctx, request)
			require.NoError(t, err)
			require.True(t, resp.GetAlreadyLanded())
			require.Equal(t, tip.String(), resp.GetLandedCommitId())
			requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)

			request = landRequest(repoProto, git.DefaultBranch, c)
			request.Strategy = strategy
			resp, err = client.UserLand(ctx, request)

			switch strategy {
			case gitalypb.UserLandRequest_STRATEGY_FF_ONLY:
				testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
				require.NotNil(t, landErrorDetail(t, err).GetNotAncestor())
				requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)
				return
			case gitalypb.UserLandRequest_STRATEGY_MERGE:
				require.NoError(t, err)
				landed, err := repo.ReadCommit(ctx, git.Revision(resp.GetLandedCommitId()))
				require.NoError(t, err)
				require.Equal(t, []string{tip.String(), c.String()}, landed.GetParentIds())
			default:
				require.NoError(t, err)
				require.True(t, resp.GetRewritten())
				landed, err := repo.ReadCommit(ctx, git.Revision(resp.GetLandedCommitId()))
				require.NoError(t, err)
				// C alone is replayed onto the tip.
				require.Equal(t, []string{tip.String()}, landed.GetParentIds())
				require.Equal(t, []byte("c"), landed.GetSubject())
			}

			requireBranchAt(t, cfg, repoPath, git.DefaultBranch, git.ObjectID(resp.GetLandedCommitId()))
			content := gittest.Exec(t, cfg, "-C", repoPath, "show", resp.GetLandedCommitId()+":c")
			require.Equal(t, "c\n", string(content))
		})
	}
}

// TestUserLand_skewedCommitterDatesInABatch lands A again and C in one batch on the history of the
// review: A is already landed and C is rebased onto the tip.
func TestUserLand_skewedCommitterDatesInABatch(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, func(t *testing.T, ctx context.Context) {
		ctx, cfg, client := setupLandBatchService(t, ctx, batchingGraft())
		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		repo := localrepo.NewTestRepo(t, cfg, repoProto)
		a, c, tip := skewedReviewHistory(t, cfg, repoPath)

		responses, errs := landAll(ctx, client, repoProto, []git.ObjectID{a, c})
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])

		require.True(t, responses[0].GetAlreadyLanded())
		require.False(t, responses[1].GetAlreadyLanded())
		require.True(t, responses[1].GetRewritten())

		head := gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/"+git.DefaultBranch)
		landed, err := repo.ReadCommit(ctx, head.Revision())
		require.NoError(t, err)
		require.Equal(t, []string{tip.String()}, landed.GetParentIds())
		require.Equal(t, []byte("c"), landed.GetSubject())
	})
}
