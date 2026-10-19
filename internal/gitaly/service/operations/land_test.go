package operations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// landRequest returns a UserLand request which lands commit onto branch with the default
// strategy.
func landRequest(repo *gitalypb.Repository, branch string, commit git.ObjectID) *gitalypb.UserLandRequest {
	return &gitalypb.UserLandRequest{
		Repository: repo,
		User:       gittest.TestUser,
		Branch:     []byte(branch),
		CommitId:   commit.String(),
	}
}

// landErrorDetail returns the UserLandError detail attached to err.
func landErrorDetail(t *testing.T, err error) *gitalypb.UserLandError {
	t.Helper()

	for _, detail := range status.Convert(err).Details() {
		if landErr, ok := detail.(*gitalypb.UserLandError); ok {
			return landErr
		}
	}

	require.FailNow(t, "error carries no UserLandError detail", "error: %v", err)
	return nil
}

// requireBranchAt asserts that the branch points to the expected commit.
func requireBranchAt(t *testing.T, cfg config.Cfg, repoPath string, branch string, expected git.ObjectID) {
	t.Helper()
	require.Equal(t, expected, gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/"+branch))
}

func TestUserLand_fastForward(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
	)
	child := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "b"},
		),
	)

	resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, child))
	require.NoError(t, err)

	testhelper.ProtoEqual(t, &gitalypb.UserLandResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{
			CommitId: child.String(),
		},
		LandedCommitId: child.String(),
		Rewritten:      false,
		Attempts:       1,
		AlreadyLanded:  false,
		OldTip:         base.String(),
		BatchSize:      1,
		BatchPosition:  1,
	}, resp)

	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, child)
}

func TestUserLand_alreadyLanded(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	tip := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithBranch(git.DefaultBranch))

	for _, tc := range []struct {
		desc   string
		commit git.ObjectID
	}{
		{desc: "commit equals the tip", commit: tip},
		{desc: "commit is an ancestor of the tip", commit: base},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, tc.commit))
			require.NoError(t, err)

			require.True(t, resp.GetAlreadyLanded())
			require.False(t, resp.GetRewritten())
			require.False(t, resp.GetBranchUpdate().GetBranchCreated())
			require.Equal(t, tip.String(), resp.GetLandedCommitId())
			require.Equal(t, tip.String(), resp.GetOldTip())
			require.Equal(t, tip.String(), resp.GetBranchUpdate().GetCommitId())
			require.EqualValues(t, 1, resp.GetAttempts())

			requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)
		})
	}
}

func TestUserLand_rebaseOntoDivergedTip(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserLandRebaseOntoDivergedTip)
}

func testUserLandRebaseOntoDivergedTip(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
	)
	tip := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithParents(base),
		gittest.WithMessage("tip change"),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
			gittest.TreeEntry{Mode: "100644", Path: "x", Content: "x"},
		),
	)
	change := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithMessage("the change"),
		gittest.WithAuthorName("Change Author"),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
			gittest.TreeEntry{Mode: "100644", Path: "y", Content: "y"},
		),
	)

	resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, change))
	require.NoError(t, err)

	landed := git.ObjectID(resp.GetLandedCommitId())
	require.NotEqual(t, change, landed)
	require.NotEqual(t, tip, landed)
	require.True(t, resp.GetRewritten())
	require.False(t, resp.GetAlreadyLanded())
	require.Equal(t, tip.String(), resp.GetOldTip())
	require.Equal(t, landed.String(), resp.GetBranchUpdate().GetCommitId())
	require.EqualValues(t, 1, resp.GetAttempts())
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, landed)

	// The rebased commit sits on top of the tip with a single parent and keeps the
	// message and the author of the original change.
	landedCommit, err := repo.ReadCommit(ctx, landed.Revision())
	require.NoError(t, err)
	require.Equal(t, []string{tip.String()}, landedCommit.GetParentIds())
	require.Equal(t, "the change", string(landedCommit.GetSubject()))
	require.Equal(t, "Change Author", string(landedCommit.GetAuthor().GetName()))

	gittest.RequireTree(t, cfg, repoPath, landed.String(), []gittest.TreeEntry{
		{Mode: "100644", Path: "a", Content: "a"},
		{Mode: "100644", Path: "x", Content: "x"},
		{Mode: "100644", Path: "y", Content: "y"},
	})
}

func TestUserLand_rebaseKeepsLongMessage(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserLandRebaseKeepsLongMessage)
}

func testUserLandRebaseKeepsLongMessage(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)
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
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "b"},
		),
	)

	// The message is longer than the commit bodies that Gitaly reads, which are cut to 10 KiB.
	message := "the change\n\n" + strings.Repeat("body line of the long message\n", 400) + "last line\n"
	require.Greater(t, len(message), 10*1024)

	change := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithMessage(message),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
			gittest.TreeEntry{Mode: "100644", Path: "c", Content: "c"},
		),
	)

	resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, change))
	require.NoError(t, err)
	require.True(t, resp.GetRewritten())

	landed := resp.GetLandedCommitId()
	require.Equal(t,
		string(gittest.Exec(t, cfg, "-C", repoPath, "log", "-1", "--format=%B", change.String())),
		string(gittest.Exec(t, cfg, "-C", repoPath, "log", "-1", "--format=%B", landed)),
	)
}

func TestUserLand_mergeStrategy(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserLandMergeStrategy)
}

func testUserLandMergeStrategy(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
	)
	tip := gittest.WriteCommit(t, cfg, repoPath,
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
	request.Strategy = gitalypb.UserLandRequest_STRATEGY_MERGE
	request.MergeMessage = []byte("land the change")

	resp, err := client.UserLand(ctx, request)
	require.NoError(t, err)

	landed := git.ObjectID(resp.GetLandedCommitId())
	require.True(t, resp.GetRewritten())
	require.Equal(t, tip.String(), resp.GetOldTip())
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, landed)

	landedCommit, err := repo.ReadCommit(ctx, landed.Revision())
	require.NoError(t, err)
	require.Equal(t, []string{tip.String(), change.String()}, landedCommit.GetParentIds())
	require.Equal(t, "land the change", string(landedCommit.GetSubject()))

	gittest.RequireTree(t, cfg, repoPath, landed.String(), []gittest.TreeEntry{
		{Mode: "100644", Path: "a", Content: "a"},
		{Mode: "100644", Path: "x", Content: "x"},
		{Mode: "100644", Path: "y", Content: "y"},
	})
}

func TestUserLand_ffOnly(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))

	t.Run("fast-forward succeeds", func(t *testing.T) {
		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
		child := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

		request := landRequest(repoProto, git.DefaultBranch, child)
		request.Strategy = gitalypb.UserLandRequest_STRATEGY_FF_ONLY

		resp, err := client.UserLand(ctx, request)
		require.NoError(t, err)
		require.Equal(t, child.String(), resp.GetLandedCommitId())
		require.False(t, resp.GetRewritten())
		requireBranchAt(t, cfg, repoPath, git.DefaultBranch, child)
	})

	t.Run("diverged tip fails with not_ancestor", func(t *testing.T) {
		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
		tip := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithBranch(git.DefaultBranch))
		change := gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithParents(base),
			gittest.WithMessage("change"),
			gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "c", Content: "c"}),
		)

		request := landRequest(repoProto, git.DefaultBranch, change)
		request.Strategy = gitalypb.UserLandRequest_STRATEGY_FF_ONLY

		resp, err := client.UserLand(ctx, request)
		testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
		require.Nil(t, resp)

		detail := landErrorDetail(t, err)
		require.Equal(t, []byte(tip.String()), detail.GetNotAncestor().GetParentRevision())
		require.Equal(t, []byte(change.String()), detail.GetNotAncestor().GetChildRevision())

		requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)
	})

	t.Run("already landed succeeds even with ff_only", func(t *testing.T) {
		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
		tip := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithBranch(git.DefaultBranch))

		request := landRequest(repoProto, git.DefaultBranch, base)
		request.Strategy = gitalypb.UserLandRequest_STRATEGY_FF_ONLY

		resp, err := client.UserLand(ctx, request)
		require.NoError(t, err)
		require.True(t, resp.GetAlreadyLanded())
		requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)
	})
}

func TestUserLand_mergeConflict(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserLandMergeConflict)
}

func testUserLandMergeConflict(t *testing.T, ctx context.Context) {
	t.Parallel()

	for _, strategy := range []gitalypb.UserLandRequest_Strategy{
		gitalypb.UserLandRequest_STRATEGY_REBASE_UNSPECIFIED,
		gitalypb.UserLandRequest_STRATEGY_MERGE,
	} {
		t.Run(strategy.String(), func(t *testing.T) {
			t.Parallel()

			ctx, cfg, client := setupOperationsService(t, ctx)
			repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

			base := gittest.WriteCommit(t, cfg, repoPath,
				gittest.WithBranch(git.DefaultBranch),
				gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base"}),
			)
			tip := gittest.WriteCommit(t, cfg, repoPath,
				gittest.WithBranch(git.DefaultBranch),
				gittest.WithParents(base),
				gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "tip"}),
			)
			change := gittest.WriteCommit(t, cfg, repoPath,
				gittest.WithParents(base),
				gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "change"}),
			)

			request := landRequest(repoProto, git.DefaultBranch, change)
			request.Strategy = strategy

			resp, err := client.UserLand(ctx, request)
			testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
			require.Nil(t, resp)

			conflict := landErrorDetail(t, err).GetMergeConflict()
			require.NotNil(t, conflict)
			require.Equal(t, []string{tip.String(), change.String()}, conflict.GetConflictingCommitIds())

			var files []string
			for _, file := range conflict.GetConflictingFiles() {
				files = append(files, string(file))
			}
			require.Contains(t, files, "a")

			requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)
		})
	}
}

func TestUserLand_rebaseRejectsMergeCommits(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserLandRebaseRejectsMergeCommits)
}

func testUserLandRebaseRejectsMergeCommits(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	tip := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "t", Content: "t"}))

	left := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "l", Content: "l"}))
	right := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "r", Content: "r"}))
	merge := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(left, right),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "l", Content: "l"},
			gittest.TreeEntry{Mode: "100644", Path: "r", Content: "r"},
		))

	t.Run("rebase strategy is rejected", func(t *testing.T) {
		resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, merge))
		testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
		require.Nil(t, resp)
		requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)
	})

	t.Run("merge strategy lands the merge commit", func(t *testing.T) {
		request := landRequest(repoProto, git.DefaultBranch, merge)
		request.Strategy = gitalypb.UserLandRequest_STRATEGY_MERGE

		resp, err := client.UserLand(ctx, request)
		require.NoError(t, err)
		require.True(t, resp.GetRewritten())
		requireBranchAt(t, cfg, repoPath, git.DefaultBranch, git.ObjectID(resp.GetLandedCommitId()))
	})
}

func TestUserLand_createBranch(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))

	t.Run("creates a missing branch", func(t *testing.T) {
		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		commit := gittest.WriteCommit(t, cfg, repoPath)

		request := landRequest(repoProto, "feature", commit)
		request.CreateBranch = true

		resp, err := client.UserLand(ctx, request)
		require.NoError(t, err)

		testhelper.ProtoEqual(t, &gitalypb.UserLandResponse{
			BranchUpdate: &gitalypb.OperationBranchUpdate{
				CommitId:      commit.String(),
				BranchCreated: true,
			},
			LandedCommitId: commit.String(),
			OldTip:         gittest.DefaultObjectHash.ZeroOID.String(),
			Attempts:       1,
			BatchSize:      1,
			BatchPosition:  1,
		}, resp)

		requireBranchAt(t, cfg, repoPath, "feature", commit)
	})

	t.Run("missing branch without create_branch is not found", func(t *testing.T) {
		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		commit := gittest.WriteCommit(t, cfg, repoPath)

		resp, err := client.UserLand(ctx, landRequest(repoProto, "feature", commit))
		testhelper.RequireGrpcCode(t, err, codes.NotFound)
		require.Nil(t, resp)

		_, err = localrepo.NewTestRepo(t, cfg, repoProto).GetReference(ctx, "refs/heads/feature")
		require.ErrorIs(t, err, git.ErrReferenceNotFound)
	})
}

func TestUserLand_invalidCommit(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

	missing := strings.Repeat("1", gittest.DefaultObjectHash.EncodedLen())

	for _, tc := range []struct {
		desc     string
		commitID string
	}{
		{desc: "not hex", commitID: "not-a-commit"},
		{desc: "short object ID", commitID: base.String()[:8]},
		{desc: "object does not exist", commitID: missing},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			request := landRequest(repoProto, git.DefaultBranch, base)
			request.CommitId = tc.commitID

			resp, err := client.UserLand(ctx, request)
			testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
			require.Nil(t, resp)
			requireBranchAt(t, cfg, repoPath, git.DefaultBranch, base)
		})
	}
}

func TestUserLand_validation(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	child := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

	for _, tc := range []struct {
		desc   string
		modify func(*gitalypb.UserLandRequest)
	}{
		{desc: "missing repository", modify: func(r *gitalypb.UserLandRequest) { r.Repository = nil }},
		{desc: "missing user", modify: func(r *gitalypb.UserLandRequest) { r.User = nil }},
		{desc: "empty user name", modify: func(r *gitalypb.UserLandRequest) { r.User = &gitalypb.User{Email: []byte("a@example.com")} }},
		{desc: "empty user email", modify: func(r *gitalypb.UserLandRequest) { r.User = &gitalypb.User{Name: []byte("A")} }},
		{desc: "empty branch", modify: func(r *gitalypb.UserLandRequest) { r.Branch = nil }},
		{desc: "full reference name as branch", modify: func(r *gitalypb.UserLandRequest) { r.Branch = []byte("refs/heads/main") }},
		{desc: "empty commit ID", modify: func(r *gitalypb.UserLandRequest) { r.CommitId = "" }},
		{desc: "unknown strategy", modify: func(r *gitalypb.UserLandRequest) { r.Strategy = gitalypb.UserLandRequest_Strategy(42) }},
		{desc: "negative max attempts", modify: func(r *gitalypb.UserLandRequest) { r.MaxAttempts = -1 }},
		{desc: "resolution with fast-forward only", modify: func(r *gitalypb.UserLandRequest) {
			r.Strategy = gitalypb.UserLandRequest_STRATEGY_FF_ONLY
			r.Resolution = &gitalypb.LandResolution{Tip: base.String(), Commit: child.String()}
		}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			request := landRequest(repoProto, git.DefaultBranch, child)
			tc.modify(request)

			resp, err := client.UserLand(ctx, request)
			testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
			require.Nil(t, resp)
			requireBranchAt(t, cfg, repoPath, git.DefaultBranch, base)
		})
	}
}

func TestUserLand_hookRejection(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	tip := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithBranch(git.DefaultBranch))
	child := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(tip))

	gittest.WriteCustomHook(t, repoPath, "pre-receive", []byte("#!/bin/sh\necho 'rejected by policy' >&2\nexit 1\n"))

	resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, child))
	testhelper.RequireGrpcCode(t, err, codes.PermissionDenied)
	require.Nil(t, resp)

	custom := landErrorDetail(t, err).GetCustomHook()
	require.NotNil(t, custom)
	require.Equal(t, gitalypb.CustomHookError_HOOK_TYPE_PRERECEIVE, custom.GetHookType())
	require.Contains(t, string(custom.GetStderr()), "rejected by policy")

	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)
}

func TestUserLand_retriesAfterConcurrentMove(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserLandRetriesAfterConcurrentMove)
}

func testUserLandRetriesAfterConcurrentMove(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	tmpDir := testhelper.TempDir(t)

	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
	)
	concurrent := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
			gittest.TreeEntry{Mode: "100644", Path: "concurrent", Content: "concurrent"},
		),
	)
	change := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
			gittest.TreeEntry{Mode: "100644", Path: "change", Content: "change"},
		),
	)

	// The first run of the pre-receive hook moves the branch as a concurrent writer would. This
	// makes the first compare-and-swap fail, so the second attempt has to land on top of it.
	marker := filepath.Join(tmpDir, "moved")
	gittest.WriteCustomHook(t, repoPath, "pre-receive", []byte(fmt.Sprintf(`#!/bin/sh
read oldval newval ref
if [ ! -e %[1]q ]; then
	touch %[1]q
	git -C %[2]q update-ref refs/heads/%[3]s %[4]s
fi
exit 0
`, marker, repoPath, git.DefaultBranch, concurrent)))

	resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, change))
	require.NoError(t, err)

	require.EqualValues(t, 2, resp.GetAttempts())
	require.True(t, resp.GetRewritten())
	require.Equal(t, concurrent.String(), resp.GetOldTip())

	landed := git.ObjectID(resp.GetLandedCommitId())
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, landed)

	landedCommit, err := repo.ReadCommit(ctx, landed.Revision())
	require.NoError(t, err)
	require.Equal(t, []string{concurrent.String()}, landedCommit.GetParentIds())

	gittest.RequireTree(t, cfg, repoPath, landed.String(), []gittest.TreeEntry{
		{Mode: "100644", Path: "a", Content: "a"},
		{Mode: "100644", Path: "change", Content: "change"},
		{Mode: "100644", Path: "concurrent", Content: "concurrent"},
	})
}

func TestUserLand_retriesExhausted(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserLandRetriesExhausted)
}

func testUserLandRetriesExhausted(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	tmpDir := testhelper.TempDir(t)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	change := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "change", Content: "change"}),
	)

	// Every run of the pre-receive hook moves the branch to the next commit of a list, so every
	// compare-and-swap loses against a concurrent writer.
	var moves []git.ObjectID
	for i := 0; i < 5; i++ {
		moves = append(moves, gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithParents(base),
			gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "m", Content: fmt.Sprint(i)}),
		))
	}

	listPath := filepath.Join(tmpDir, "moves")
	var list strings.Builder
	for _, move := range moves {
		list.WriteString(move.String() + "\n")
	}
	require.NoError(t, os.WriteFile(listPath, []byte(list.String()), 0o600))

	counterPath := filepath.Join(tmpDir, "counter")
	gittest.WriteCustomHook(t, repoPath, "pre-receive", []byte(fmt.Sprintf(`#!/bin/sh
read oldval newval ref
n=0
if [ -e %[1]q ]; then n=$(cat %[1]q); fi
n=$((n+1))
echo $n > %[1]q
git -C %[2]q update-ref refs/heads/%[3]s $(sed -n "${n}p" %[4]q)
exit 0
`, counterPath, repoPath, git.DefaultBranch, listPath)))

	request := landRequest(repoProto, git.DefaultBranch, change)
	request.MaxAttempts = 3

	resp, err := client.UserLand(ctx, request)
	testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
	require.Nil(t, resp)

	refUpdate := landErrorDetail(t, err).GetReferenceUpdate()
	require.NotNil(t, refUpdate)
	require.Equal(t, []byte("refs/heads/"+git.DefaultBranch), refUpdate.GetReferenceName())
	// The last attempt read the tip set by the second move.
	require.Equal(t, moves[1].String(), refUpdate.GetOldOid())

	// The branch was moved by the hook on every attempt, the landing never happened.
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, moves[2])
}

func TestUserLand_concurrentLandings(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserLandConcurrentLandings)
}

func testUserLandConcurrentLandings(t *testing.T, ctx context.Context) {
	t.Parallel()

	const landers = 12

	ctx, cfg, client := setupOperationsService(t, ctx)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}),
	)

	// Every change adds a distinct file on top of the same old tip.
	var changes []git.ObjectID
	var entries []gittest.TreeEntry
	entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"})
	for i := 0; i < landers; i++ {
		name := fmt.Sprintf("file-%02d", i)
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: name, Content: name})

		changes = append(changes, gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithParents(base),
			gittest.WithMessage("land "+name),
			gittest.WithTreeEntries(
				gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"},
				gittest.TreeEntry{Mode: "100644", Path: name, Content: name},
			),
		))
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		responses []*gitalypb.UserLandResponse
		errs      []error
	)
	for _, change := range changes {
		wg.Add(1)
		go func(change git.ObjectID) {
			defer wg.Done()

			resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, change))

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			responses = append(responses, resp)
		}(change)
	}
	wg.Wait()

	require.Empty(t, errs)
	require.Len(t, responses, landers)

	landedIDs := make(map[string]bool, landers)
	for _, resp := range responses {
		require.False(t, resp.GetAlreadyLanded())
		require.NotEmpty(t, resp.GetLandedCommitId())
		landedIDs[resp.GetLandedCommitId()] = true
	}
	require.Len(t, landedIDs, landers, "every landing must produce its own commit")

	tip := gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/"+git.DefaultBranch)

	// All files of all changes are in the final tree.
	gittest.RequireTree(t, cfg, repoPath, tip.String(), entries)

	// The history is linear: every commit has at most one parent, no merge commits exist and
	// the history consists of the base commit plus one commit per landing.
	revList := gittest.Exec(t, cfg, "-C", repoPath, "rev-list", "--parents", tip.String())
	lines := strings.Split(strings.TrimSpace(string(revList)), "\n")
	require.Len(t, lines, landers+1)
	for _, line := range lines {
		fields := strings.Fields(line)
		require.LessOrEqual(t, len(fields), 2, "commit %q has more than one parent", fields[0])
	}

	merges := strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-list", "--merges", tip.String())))
	require.Empty(t, merges)
}

// writeLandHookCounter writes a pre-receive hook that appends a line to counterPath on every run.
// It lets tests assert how often the hooks ran, which is how retries show up.
func writeLandHookCounter(t *testing.T, repoPath, counterPath string) {
	t.Helper()

	gittest.WriteCustomHook(t, repoPath, "pre-receive", []byte(fmt.Sprintf(`#!/bin/sh
echo run >> %q
exit 0
`, counterPath)))
}

// landHookRuns returns how often the hook written by writeLandHookCounter ran.
func landHookRuns(t *testing.T, counterPath string) int {
	t.Helper()

	content, err := os.ReadFile(counterPath)
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)

	return strings.Count(string(content), "\n")
}

func TestUserLand_invalidBranchNames(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

	counterPath := filepath.Join(testhelper.TempDir(t), "hook-runs")
	writeLandHookCounter(t, repoPath, counterPath)

	// Names that pass the revision check but are not valid branch names must be rejected
	// up front. Before, they reached the compare-and-swap and were retried until exhaustion.
	for _, branch := range []string{
		"../../evil",
		"a..b",
		"foo.lock",
		"x/",
		"@{-1}",
		"x~1",
		"x^",
		"HEAD",
		"-leading-dash",
		"with space",
	} {
		t.Run(branch, func(t *testing.T) {
			request := landRequest(repoProto, branch, commit)
			request.CreateBranch = true

			resp, err := client.UserLand(ctx, request)
			testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
			require.Nil(t, resp)
		})
	}

	require.Zero(t, landHookRuns(t, counterPath), "invalid branch names must not reach the hooks")
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, base)
}

func TestUserLand_branchDirectoryConflict(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

	counterPath := filepath.Join(testhelper.TempDir(t), "hook-runs")
	writeLandHookCounter(t, repoPath, counterPath)

	// refs/heads/main/sub cannot exist next to refs/heads/main. This is a permanent error, so the
	// landing must fail once instead of being retried until the attempts are exhausted.
	request := landRequest(repoProto, git.DefaultBranch+"/sub", commit)
	request.CreateBranch = true

	resp, err := client.UserLand(ctx, request)
	testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
	require.Nil(t, resp)
	require.Contains(t, err.Error(), "file directory conflict")
	require.NotContains(t, err.Error(), "kept moving", "a permanent error must not be reported as a lost race")

	require.LessOrEqual(t, landHookRuns(t, counterPath), 1, "a permanent error must not be retried")
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, base)
}

func TestUserLand_annotatedTagCommit(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	child := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

	// The tag object is a fast-forward of the tip once peeled. Before, the tag OID itself became the
	// new tip, which update-ref refuses for a branch, so the landing never succeeded.
	tag := gittest.WriteTag(t, cfg, repoPath, "release", child.Revision(), gittest.WriteTagConfig{Message: "release"})
	require.NotEqual(t, child, tag)

	resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, tag))
	require.NoError(t, err)

	require.Equal(t, child.String(), resp.GetLandedCommitId())
	require.Equal(t, child.String(), resp.GetBranchUpdate().GetCommitId())
	require.Equal(t, base.String(), resp.GetOldTip())
	require.False(t, resp.GetRewritten())
	require.EqualValues(t, 1, resp.GetAttempts())
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, child)
}

func TestUserLand_unrelatedHistory(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserLandUnrelatedHistory)
}

func testUserLandUnrelatedHistory(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)

	t.Run("rebase strategy is rejected", func(t *testing.T) {
		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		base := gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithBranch(git.DefaultBranch),
			gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
		)
		orphan := gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "o", Content: "o"}),
		)

		resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, orphan))
		testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
		require.Nil(t, resp)
		requireBranchAt(t, cfg, repoPath, git.DefaultBranch, base)
	})

	t.Run("merge strategy lands unrelated history", func(t *testing.T) {
		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		base := gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithBranch(git.DefaultBranch),
			gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
		)
		orphan := gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "o", Content: "o"}),
		)

		request := landRequest(repoProto, git.DefaultBranch, orphan)
		request.Strategy = gitalypb.UserLandRequest_STRATEGY_MERGE

		resp, err := client.UserLand(ctx, request)
		require.NoError(t, err)
		require.True(t, resp.GetRewritten())

		landed := git.ObjectID(resp.GetLandedCommitId())
		requireBranchAt(t, cfg, repoPath, git.DefaultBranch, landed)

		repo := localrepo.NewTestRepo(t, cfg, repoProto)
		landedCommit, err := repo.ReadCommit(ctx, landed.Revision())
		require.NoError(t, err)
		require.Equal(t, []string{base.String(), orphan.String()}, landedCommit.GetParentIds())
	})
}

func TestUserLand_accessCheck(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc            string
		allowed         bool
		allowedMessage  string
		allowedErr      error
		expectedMessage string
	}{
		{desc: "allowed", allowed: true},
		{desc: "disallowed", allowed: false, allowedMessage: "you shall not pass", expectedMessage: "you shall not pass"},
		{desc: "failing", allowedErr: errors.New("failure"), expectedMessage: "failure"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			cfg := testcfg.Build(t)
			backchannelRegistry := backchannel.NewRegistry()
			txManager := transaction.NewManager(cfg, testhelper.SharedLogger(t), backchannelRegistry)

			hookManager := hook.NewManager(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t), gittest.NewCommandFactory(t, cfg), nil, txManager, gitlab.NewMockClient(
				t,
				func(context.Context, gitlab.AllowedParams) (bool, string, error) {
					return tc.allowed, tc.allowedMessage, tc.allowedErr
				},
				gitlab.MockPreReceive,
				gitlab.MockPostReceive,
			), hook.NewPostReceiveRegistry())

			ctx, cfg, client := setupOperationsServiceWithCfg(
				t, testhelper.Context(t), cfg,
				testserver.WithBackchannelRegistry(backchannelRegistry),
				testserver.WithTransactionManager(txManager),
				testserver.WithHookManager(hookManager),
				testserver.WithPostReceiveRegistry(hookManager.PostReceiveRegistry()),
			)

			repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
			base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
			child := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

			resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, child))
			if tc.allowed {
				require.NoError(t, err)
				require.Equal(t, child.String(), resp.GetLandedCommitId())
				requireBranchAt(t, cfg, repoPath, git.DefaultBranch, child)
				return
			}

			testhelper.RequireGrpcCode(t, err, codes.PermissionDenied)
			require.Nil(t, resp)

			accessCheck := landErrorDetail(t, err).GetAccessCheck()
			require.NotNil(t, accessCheck)
			require.Equal(t, tc.expectedMessage, accessCheck.GetErrorMessage())
			require.Equal(t, gittest.GlID, accessCheck.GetUserId())
			require.Contains(t, string(accessCheck.GetChanges()), "refs/heads/"+git.DefaultBranch)

			requireBranchAt(t, cfg, repoPath, git.DefaultBranch, base)
		})
	}
}

func TestUserLand_updateHookRejection(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	child := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

	gittest.WriteCustomHook(t, repoPath, "update", []byte("#!/bin/sh\necho 'rejected by update hook' >&2\nexit 1\n"))

	resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, child))
	testhelper.RequireGrpcCode(t, err, codes.PermissionDenied)
	require.Nil(t, resp)

	custom := landErrorDetail(t, err).GetCustomHook()
	require.NotNil(t, custom)
	require.Equal(t, gitalypb.CustomHookError_HOOK_TYPE_UPDATE, custom.GetHookType())
	require.Contains(t, string(custom.GetStderr()), "rejected by update hook")

	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, base)
}

func TestUserLand_concurrentLandingsMVCC(t *testing.T) {
	t.Parallel()

	testhelper.SkipWithReftable(t, "MVCC backend selection is overridden by GIT_DEFAULT_REF_FORMAT")
	testhelper.SkipWithPraefect(t, "MVCC repositories cannot be registered in Praefect's database")
	gittest.SkipWithoutMVCCGit(t)

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	ctx, cfg, client := setupOperationsService(t, ctx)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		ReferenceBackend:       git.ReferenceBackendMVCC,
		SkipCreationViaService: true,
	})

	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}),
	)

	const landers = 8
	var changes []git.ObjectID
	for i := 0; i < landers; i++ {
		name := fmt.Sprintf("file-%02d", i)
		changes = append(changes, gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithParents(base),
			gittest.WithTreeEntries(
				gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"},
				gittest.TreeEntry{Mode: "100644", Path: name, Content: name},
			),
		))
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		landed    []*gitalypb.UserLandResponse
		failures  []error
		succeeded int
	)
	for _, change := range changes {
		wg.Add(1)
		go func(change git.ObjectID) {
			defer wg.Done()

			resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, change))

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			succeeded++
			landed = append(landed, resp)
		}(change)
	}
	wg.Wait()

	// On MVCC, the concurrent writer is detected when the snapshot is committed. The losers must
	// get the FailedPrecondition with the reference_update detail that tells the client to retry,
	// instead of a bare Aborted.
	require.NotZero(t, succeeded, "at least one landing must win")
	require.Equal(t, landers, succeeded+len(failures))
	for _, err := range failures {
		testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
		require.NotNil(t, landErrorDetail(t, err).GetReferenceUpdate())
	}

	for _, resp := range landed {
		require.EqualValues(t, 1, resp.GetAttempts())
	}

	// The updates of the losers were thrown away with their snapshots, so the changeset log
	// lists only the landings that happened, as a chain from the base to the final tip.
	entries, err := changelog.List(repoPath, 0, 1000)
	require.NoError(t, err)

	var lands []changelog.Entry
	for _, entry := range entries {
		if entry.Reference == git.DefaultRef.String() && entry.Operation == "land" {
			lands = append(lands, entry)
		}
	}
	require.Len(t, lands, succeeded)

	previous := base.String()
	for _, entry := range lands {
		require.Equal(t, previous, entry.OldOID)
		previous = entry.NewOID
	}
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, git.ObjectID(previous))
}

func TestUserLand_reappliesRevertedCherryPick(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserLandReappliesRevertedCherryPick)
}

func testUserLandReappliesRevertedCherryPick(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	fEntry := gittest.TreeEntry{Mode: "100644", Path: "f", Content: "f"}
	gEntry := gittest.TreeEntry{Mode: "100644", Path: "g", Content: "g"}
	uEntry := gittest.TreeEntry{Mode: "100644", Path: "u", Content: "u"}

	// The branch applied a commit with the same patch as the first commit of the change and
	// reverted it afterwards. The change is not on the branch, so it must be landed in full.
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(baseEntry))
	u1 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(baseEntry, uEntry))
	same := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(u1), gittest.WithTreeEntries(baseEntry, uEntry, fEntry))
	revert := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(same), gittest.WithTreeEntries(baseEntry, uEntry))

	addF := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(baseEntry, fEntry))
	addG := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(addF), gittest.WithTreeEntries(baseEntry, fEntry, gEntry))

	for _, tc := range []struct {
		desc            string
		branch          string
		change          git.ObjectID
		expectedEntries []gittest.TreeEntry
		expectedCommits int
	}{
		{
			desc:            "single commit",
			branch:          "single",
			change:          addF,
			expectedEntries: []gittest.TreeEntry{baseEntry, fEntry, uEntry},
			expectedCommits: 1,
		},
		{
			desc:            "first of several commits",
			branch:          "several",
			change:          addG,
			expectedEntries: []gittest.TreeEntry{baseEntry, fEntry, gEntry, uEntry},
			expectedCommits: 2,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			gittest.WriteRef(t, cfg, repoPath, git.NewReferenceNameFromBranchName(tc.branch), revert)

			resp, err := client.UserLand(ctx, landRequest(repoProto, tc.branch, tc.change))
			require.NoError(t, err)

			require.False(t, resp.GetAlreadyLanded())
			require.True(t, resp.GetRewritten())
			require.Equal(t, revert.String(), resp.GetOldTip())

			landed := git.ObjectID(resp.GetLandedCommitId())
			require.NotEqual(t, revert, landed)
			requireBranchAt(t, cfg, repoPath, tc.branch, landed)
			gittest.RequireTree(t, cfg, repoPath, landed.String(), tc.expectedEntries)

			count := strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-list", "--count", revert.String()+".."+landed.String())))
			require.Equal(t, fmt.Sprint(tc.expectedCommits), count)
		})
	}
}

func TestUserLand_changeAlreadyAppliedUpstream(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserLandChangeAlreadyAppliedUpstream)
}

func testUserLandChangeAlreadyAppliedUpstream(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	fEntry := gittest.TreeEntry{Mode: "100644", Path: "f", Content: "f"}
	uEntry := gittest.TreeEntry{Mode: "100644", Path: "u", Content: "u"}

	// The branch holds a cherry-pick of the change, but not the change itself.
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(baseEntry))
	u1 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(baseEntry, uEntry))
	tip := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(u1),
		gittest.WithTreeEntries(baseEntry, uEntry, fEntry),
		gittest.WithBranch(git.DefaultBranch),
	)
	change := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(baseEntry, fEntry))

	resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, change))
	require.NoError(t, err)

	// The change is not reachable from the tip, so it is not reported as already landed. Its
	// changes are on the tip, so the branch is left unchanged.
	testhelper.ProtoEqual(t, &gitalypb.UserLandResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{
			CommitId: tip.String(),
		},
		LandedCommitId: tip.String(),
		Attempts:       1,
		OldTip:         tip.String(),
		BatchSize:      1,
		BatchPosition:  1,
	}, resp)

	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)
}

func TestUserLand_branchDeletedConcurrently(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))

	for _, tc := range []struct {
		desc         string
		createBranch bool
	}{
		{desc: "without create_branch", createBranch: false},
		{desc: "with create_branch", createBranch: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
			tmpDir := testhelper.TempDir(t)

			base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
			child := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

			// The first run of the pre-receive hook deletes the branch as a concurrent
			// writer would, so that locking the branch fails because it cannot be resolved.
			marker := filepath.Join(tmpDir, "deleted")
			counterPath := filepath.Join(tmpDir, "counter")
			gittest.WriteCustomHook(t, repoPath, "pre-receive", []byte(fmt.Sprintf(`#!/bin/sh
echo run >> %[3]q
if [ ! -e %[1]q ]; then
	touch %[1]q
	git -C %[2]q update-ref -d refs/heads/%[4]s
fi
exit 0
`, marker, repoPath, counterPath, git.DefaultBranch)))

			request := landRequest(repoProto, git.DefaultBranch, child)
			request.CreateBranch = tc.createBranch

			resp, err := client.UserLand(ctx, request)
			if !tc.createBranch {
				// The retry sees that the branch is gone.
				testhelper.RequireGrpcCode(t, err, codes.NotFound)
				require.Nil(t, resp)
				require.Equal(t, 1, landHookRuns(t, counterPath))

				_, err = localrepo.NewTestRepo(t, cfg, repoProto).GetReference(ctx, git.NewReferenceNameFromBranchName(git.DefaultBranch))
				require.ErrorIs(t, err, git.ErrReferenceNotFound)
				return
			}

			// The retry creates the branch at the commit.
			require.NoError(t, err)
			testhelper.ProtoEqual(t, &gitalypb.UserLandResponse{
				BranchUpdate: &gitalypb.OperationBranchUpdate{
					CommitId:      child.String(),
					BranchCreated: true,
				},
				LandedCommitId: child.String(),
				OldTip:         gittest.DefaultObjectHash.ZeroOID.String(),
				Attempts:       2,
				BatchSize:      1,
				BatchPosition:  1,
			}, resp)
			require.Equal(t, 2, landHookRuns(t, counterPath))
			requireBranchAt(t, cfg, repoPath, git.DefaultBranch, child)
		})
	}
}
