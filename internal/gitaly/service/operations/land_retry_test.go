package operations

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// retryFixture is a repository whose branch main moved on from base with a commit that adds x,
// and a change from base that adds y, which lands on main by a rebase or a merge.
type retryFixture struct {
	repoProto *gitalypb.Repository
	repoPath  string
	base      git.ObjectID
	tip       git.ObjectID
	change    git.ObjectID
	// withoutY is the tree of main before the change.
	withoutY []gittest.TreeEntry
}

func newRetryFixture(t *testing.T, ctx context.Context, cfg config.Cfg) retryFixture {
	t.Helper()

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	withoutY := []gittest.TreeEntry{
		{Mode: "100644", Path: "a", Content: "a"},
		{Mode: "100644", Path: "x", Content: "x"},
	}
	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
	)
	tip := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithParents(base), gittest.WithTreeEntries(withoutY...))
	change := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
			gittest.TreeEntry{Mode: "100644", Path: "y", Content: "y"},
		),
	)

	return retryFixture{repoProto: repoProto, repoPath: repoPath, base: base, tip: tip, change: change, withoutY: withoutY}
}

// landEntry returns the changelog entry of the update of main to landed.
func landEntry(t *testing.T, repoPath string, landed git.ObjectID) changelog.Entry {
	t.Helper()

	entries, err := changelog.List(repoPath, 0, 1000)
	require.NoError(t, err)

	var found []changelog.Entry
	for _, entry := range entries {
		if entry.Reference == git.DefaultRef.String() && entry.NewOID == landed.String() {
			found = append(found, entry)
		}
	}
	require.Len(t, found, 1, "the changelog records the update of main to %s once", landed)
	return found[0]
}

// requireLandedEarlier checks the answer of a land that the record of the earlier land at entry
// answered: already landed as the commit of that land, with its sequence and the revert found.
func requireLandedEarlier(t *testing.T, resp *gitalypb.UserLandResponse, entry changelog.Entry, landed, revertedBy git.ObjectID) {
	t.Helper()

	require.True(t, resp.GetAlreadyLanded())
	require.False(t, resp.GetRewritten())
	require.Equal(t, landed.String(), resp.GetLandedCommitId())
	require.Equal(t, entry.Sequence, resp.GetLandedSequence())
	require.Equal(t, revertedBy.String(), resp.GetRevertedByCommitId())

	detail := fmt.Sprintf("landed as %s at changelog %d", landed, entry.Sequence)
	if revertedBy != "" {
		detail += fmt.Sprintf("; it was reverted later by %s", revertedBy)
	}
	require.Equal(t, detail, resp.GetAlreadyLandedDetail())
}

// TestUserLand_retryAfterALostResponse lands a change twice, as a client does that retries
// Unavailable after the response of a land that succeeded was lost (sdk.UnavailableRetry): the
// change is never landed twice (doc/graft/PLAN11.md). With a merge or a fast-forward, the change
// itself is on the branch, so the second land finds it reachable. With a rebase, the branch holds a
// rebased copy, and the record of the first land answers.
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
		// fastForward lands the change onto base, which it fast-forwards.
		fastForward bool
		// commits is the number of commits of the change; the request lands the last.
		commits int
		// fromRecord is whether the record of the first land answers the second.
		fromRecord bool
	}{
		{desc: "rebase", strategy: gitalypb.UserLandRequest_STRATEGY_REBASE_UNSPECIFIED, commits: 1, fromRecord: true},
		{desc: "rebase of several commits", strategy: gitalypb.UserLandRequest_STRATEGY_REBASE_UNSPECIFIED, commits: 3, fromRecord: true},
		{desc: "merge", strategy: gitalypb.UserLandRequest_STRATEGY_MERGE, commits: 1},
		{desc: "fast-forward", strategy: gitalypb.UserLandRequest_STRATEGY_FF_ONLY, fastForward: true, commits: 1},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			fx := newRetryFixture(t, ctx, cfg)
			if tc.fastForward {
				gittest.Exec(t, cfg, "-C", fx.repoPath, "update-ref", git.DefaultRef.String(), fx.base.String())
			}

			change := fx.change
			for i := 1; i < tc.commits; i++ {
				change = gittest.WriteCommit(t, cfg, fx.repoPath,
					gittest.WithParents(change),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
						gittest.TreeEntry{Mode: "100644", Path: "y", Content: fmt.Sprintf("y%d", i)},
					),
				)
			}

			request := landRequest(fx.repoProto, git.DefaultBranch, change)
			request.Strategy = tc.strategy

			first, err := client.UserLand(ctx, request)
			require.NoError(t, err)
			require.Equal(t, !tc.fastForward, first.GetRewritten())
			landed := git.ObjectID(first.GetLandedCommitId())
			commits := gittest.Exec(t, cfg, "-C", fx.repoPath, "rev-list", "--count", landed.String())

			// Every land records its identity: the commit of the request and the commit it landed as.
			entry := landEntry(t, fx.repoPath, landed)
			require.Equal(t, []changelog.Land{{Original: change.String(), Landed: landed.String()}}, entry.Lands)

			// The retry of the same request.
			second, err := client.UserLand(ctx, request)
			require.NoError(t, err)
			require.True(t, second.GetAlreadyLanded())
			require.False(t, second.GetRewritten())
			require.Equal(t, landed.String(), second.GetLandedCommitId(), "the answer names the commit of the first land")
			require.Equal(t, landed.String(), second.GetBranchUpdate().GetCommitId())
			if tc.fromRecord {
				requireLandedEarlier(t, second, entry, landed, "")
			} else {
				require.Zero(t, second.GetLandedSequence())
				require.Empty(t, second.GetAlreadyLandedDetail())
			}

			requireBranchAt(t, cfg, fx.repoPath, git.DefaultBranch, landed)
			require.Equal(t, commits, gittest.Exec(t, cfg, "-C", fx.repoPath, "rev-list", "--count", git.DefaultBranch))
			landEntry(t, fx.repoPath, landed)
		})
	}

	t.Run("after a revert of the change", func(t *testing.T) {
		t.Parallel()

		for _, revert := range []struct {
			desc string
			// message is the message of the revert of landed, empty for a revert that does not say
			// what it reverts.
			message func(landed git.ObjectID) string
			named   bool
		}{
			{
				desc: "git revert",
				message: func(landed git.ObjectID) string {
					return fmt.Sprintf("Revert \"y\"\n\nThis reverts commit %s.\n", landed)
				},
				named: true,
			},
			{
				desc: "graft-verify",
				message: func(landed git.ObjectID) string {
					return fmt.Sprintf("graft-verify: revert %s\n\nThis reverts commit %s, which failed the verification of main.\n", landed.String()[:12], landed)
				},
				named: true,
			},
			{
				desc:    "a revert that does not say so",
				message: func(git.ObjectID) string { return "drop y\n" },
			},
		} {
			t.Run(revert.desc, func(t *testing.T) {
				t.Parallel()

				// The change landed and a later commit reverted it. The retry of its land, or
				// a new land of the same commit, does not land it again: it answers already
				// landed, the revert stays, and nothing is replayed.
				fx := newRetryFixture(t, ctx, cfg)
				request := landRequest(fx.repoProto, git.DefaultBranch, fx.change)
				first, err := client.UserLand(ctx, request)
				require.NoError(t, err)
				landed := git.ObjectID(first.GetLandedCommitId())

				reverted := gittest.WriteCommit(t, cfg, fx.repoPath,
					gittest.WithBranch(git.DefaultBranch),
					gittest.WithParents(landed),
					gittest.WithTreeEntries(fx.withoutY...),
					gittest.WithMessage(revert.message(landed)),
				)
				later := gittest.WriteCommit(t, cfg, fx.repoPath,
					gittest.WithBranch(git.DefaultBranch),
					gittest.WithParents(reverted),
					gittest.WithTreeEntries(append(fx.withoutY, gittest.TreeEntry{Mode: "100644", Path: "z", Content: "z"})...),
				)

				second, err := client.UserLand(ctx, request)
				require.NoError(t, err)

				var revertedBy git.ObjectID
				if revert.named {
					revertedBy = reverted
				}
				requireLandedEarlier(t, second, landEntry(t, fx.repoPath, landed), landed, revertedBy)
				require.Equal(t, later.String(), second.GetBranchUpdate().GetCommitId())
				require.Equal(t, later.String(), second.GetOldTip())
				requireBranchAt(t, cfg, fx.repoPath, git.DefaultBranch, later)
			})
		}
	})

	t.Run("after the branch was reset past the land", func(t *testing.T) {
		t.Parallel()

		// The change landed, and the branch was reset to the tip before it: the change is no
		// longer on the branch, and the land lands it again, as a new land.
		fx := newRetryFixture(t, ctx, cfg)
		request := landRequest(fx.repoProto, git.DefaultBranch, fx.change)
		first, err := client.UserLand(ctx, request)
		require.NoError(t, err)

		moved := gittest.WriteCommit(t, cfg, fx.repoPath,
			gittest.WithParents(fx.tip),
			gittest.WithTreeEntries(append(fx.withoutY, gittest.TreeEntry{Mode: "100644", Path: "w", Content: "w"})...),
		)
		gittest.Exec(t, cfg, "-C", fx.repoPath, "update-ref", git.DefaultRef.String(), moved.String(), first.GetLandedCommitId())

		second, err := client.UserLand(ctx, request)
		require.NoError(t, err)
		require.False(t, second.GetAlreadyLanded())
		require.True(t, second.GetRewritten())
		require.Zero(t, second.GetLandedSequence())
		require.Equal(t, moved.String(), second.GetOldTip())
		relanded := git.ObjectID(second.GetLandedCommitId())
		gittest.RequireTree(t, cfg, fx.repoPath, relanded.String(), []gittest.TreeEntry{
			{Mode: "100644", Path: "a", Content: "a"},
			{Mode: "100644", Path: "w", Content: "w"},
			{Mode: "100644", Path: "x", Content: "x"},
			{Mode: "100644", Path: "y", Content: "y"},
		})

		// The new land replaced the record: a retry of it answers with the new land.
		third, err := client.UserLand(ctx, request)
		require.NoError(t, err)
		requireLandedEarlier(t, third, landEntry(t, fx.repoPath, relanded), relanded, "")
	})

	t.Run("on another branch", func(t *testing.T) {
		t.Parallel()

		// The identity of a land is the branch and the commit: the change landed on main is a new
		// land on another branch.
		fx := newRetryFixture(t, ctx, cfg)
		gittest.Exec(t, cfg, "-C", fx.repoPath, "branch", "other", fx.tip.String())

		_, err := client.UserLand(ctx, landRequest(fx.repoProto, git.DefaultBranch, fx.change))
		require.NoError(t, err)

		other, err := client.UserLand(ctx, landRequest(fx.repoProto, "other", fx.change))
		require.NoError(t, err)
		require.False(t, other.GetAlreadyLanded())
		require.True(t, other.GetRewritten())
	})
}

// TestUserLand_retryOfAResolvedLand retries a land whose conflicts the resolver resolved into a
// merge commit: commit_id is a parent of that commit, so the retry finds it on the branch, as for a
// merge, and the resolver is not asked again.
func TestUserLand_retryOfAResolvedLand(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())
	fx := newResolveFixture(t, enabledResolver(argv))
	request := fx.request("Land agent work")

	first, err := fx.client.UserLand(fx.ctx, request)
	require.NoError(t, err)
	require.True(t, first.GetRewritten())
	require.NotEmpty(t, first.GetResolvedPaths())
	require.Equal(t, 1, resolverCalls(t, dir))
	landed := git.ObjectID(first.GetLandedCommitId())

	require.Equal(t, []changelog.Land{{Original: fx.commit.String(), Landed: landed.String()}}, landEntry(t, fx.repoPath, landed).Lands)

	second, err := fx.client.UserLand(fx.ctx, request)
	require.NoError(t, err)
	require.True(t, second.GetAlreadyLanded())
	require.Equal(t, landed.String(), second.GetLandedCommitId())
	require.Zero(t, second.GetLandedSequence())
	require.Empty(t, second.GetResolvedPaths())
	require.Equal(t, 1, resolverCalls(t, dir), "the retry does not resolve again")
	requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, landed)
}
