package operations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestUserLand_retryAfterALostResponse lands a change twice, as a client does that retries
// Unavailable after the response of a land that succeeded was lost (sdk.UnavailableRetry): the
// change is never landed twice. With a merge, the change itself is on the branch, so the second
// land reports it already landed. With a rebase, the branch holds a rebased copy: the second land
// replays the change, which comes out empty, and leaves the branch unchanged.
func TestUserLand_retryAfterALostResponse(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserLandRetryAfterALostResponse)
}

func testUserLandRetryAfterALostResponse(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)

	for _, tc := range []struct {
		desc     string
		strategy gitalypb.UserLandRequest_Strategy
		// alreadyLanded is what the second land reports.
		alreadyLanded bool
	}{
		{desc: "rebase", strategy: gitalypb.UserLandRequest_STRATEGY_REBASE_UNSPECIFIED},
		{desc: "merge", strategy: gitalypb.UserLandRequest_STRATEGY_MERGE, alreadyLanded: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
			base := gittest.WriteCommit(t, cfg, repoPath,
				gittest.WithBranch(git.DefaultBranch),
				gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
			)
			gittest.WriteCommit(t, cfg, repoPath,
				gittest.WithBranch(git.DefaultBranch),
				gittest.WithParents(base),
				gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
					gittest.TreeEntry{Mode: "100644", Path: "x", Content: "x"},
				),
			)
			change := gittest.WriteCommit(t, cfg, repoPath,
				gittest.WithParents(base),
				gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
					gittest.TreeEntry{Mode: "100644", Path: "y", Content: "y"},
				),
			)

			request := landRequest(repoProto, git.DefaultBranch, change)
			request.Strategy = tc.strategy

			first, err := client.UserLand(ctx, request)
			require.NoError(t, err)
			require.True(t, first.GetRewritten())
			landed := git.ObjectID(first.GetLandedCommitId())
			commits := gittest.Exec(t, cfg, "-C", repoPath, "rev-list", "--count", landed.String())

			// The retry of the same request.
			second, err := client.UserLand(ctx, request)
			require.NoError(t, err)
			require.Equal(t, tc.alreadyLanded, second.GetAlreadyLanded())
			require.False(t, second.GetRewritten())
			require.Equal(t, landed.String(), second.GetLandedCommitId(), "the branch is left at the first land")
			requireBranchAt(t, cfg, repoPath, git.DefaultBranch, landed)
			require.Equal(t, commits, gittest.Exec(t, cfg, "-C", repoPath, "rev-list", "--count", git.DefaultBranch))
		})
	}

	t.Run("rebase, after a revert of the change", func(t *testing.T) {
		t.Parallel()

		// A change that a later commit reverted is replayed again, as git's cherry-pick would.
		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		withoutY := []gittest.TreeEntry{
			{Mode: "100644", Path: "a", Content: "a"},
			{Mode: "100644", Path: "x", Content: "x"},
		}
		base := gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithBranch(git.DefaultBranch),
			gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
		)
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithParents(base), gittest.WithTreeEntries(withoutY...))
		change := gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithParents(base),
			gittest.WithTreeEntries(
				gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
				gittest.TreeEntry{Mode: "100644", Path: "y", Content: "y"},
			),
		)

		request := landRequest(repoProto, git.DefaultBranch, change)
		first, err := client.UserLand(ctx, request)
		require.NoError(t, err)
		reverted := gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithBranch(git.DefaultBranch),
			gittest.WithParents(git.ObjectID(first.GetLandedCommitId())),
			gittest.WithTreeEntries(withoutY...),
		)

		second, err := client.UserLand(ctx, request)
		require.NoError(t, err)
		require.True(t, second.GetRewritten())
		require.Equal(t, reverted.String(), second.GetOldTip())
		gittest.RequireTree(t, cfg, repoPath, second.GetLandedCommitId(), append(withoutY, gittest.TreeEntry{Mode: "100644", Path: "y", Content: "y"}))
	})
}
