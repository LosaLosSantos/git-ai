package sdk_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
)

// reviewBaseSource is the base version of the Go file that the review tests change.
const reviewBaseSource = "package x\n\nfunc F() {}\n"

// reviewCodeSource is a change of the code of reviewBaseSource.
const reviewCodeSource = "package x\n\nfunc F() int { return 1 }\n"

// reviewCommentSource adds a comment to reviewCodeSource only.
const reviewCommentSource = "package x\n\n// F returns one.\nfunc F() int { return 1 }\n"

// allowSelfApproval configures one rule that requires one approval of code
// changes and lets the author approve. The tests run without an identity, so
// every change and every verdict comes from the same anonymous actor.
func allowSelfApproval(cfg *config.Cfg) {
	cfg.Graft.Review = config.GraftReview{Rules: []config.GraftReviewRule{
		{Name: "code", Classes: []string{"code"}, Approvals: 1, AllowAuthor: true},
	}}
}

// createTestReview opens a review of head against branch.
func createTestReview(t *testing.T, env integrationEnv, branch, head string) *sdk.Review {
	t.Helper()

	review, err := env.repo.CreateReview(env.ctx, sdk.CreateReviewOptions{
		TargetBranch: branch,
		Head:         head,
		Title:        "change " + head[:8],
	})
	require.NoError(t, err)
	return review
}

func TestIntegration_CreateReview(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	base := env.writeCommit(t, "main", map[string]string{"x.go": reviewBaseSource})
	head := env.writeCommit(t, "", map[string]string{"x.go": reviewCodeSource}, base)

	t.Run("defaults the base to the merge base and classifies the files", func(t *testing.T) {
		review, err := env.repo.CreateReview(env.ctx, sdk.CreateReviewOptions{
			TargetBranch: "refs/heads/main",
			Head:         head.String(),
			Title:        "fix x",
			Description:  "details",
		})
		require.NoError(t, err)

		require.NotEmpty(t, review.GetId())
		require.Equal(t, "main", string(review.GetTargetBranch()), "the refs/heads/ prefix is not stored")
		require.Equal(t, base.String(), review.GetBaseCommitId())
		require.Equal(t, head.String(), review.GetHeadCommitId())
		require.Equal(t, sdk.ReviewOpen, review.GetState())
		require.Equal(t, "fix x", string(review.GetTitle()))
		require.Equal(t, "details", string(review.GetDescription()))
		require.Equal(t, "anonymous", review.GetAuthor())

		require.Len(t, review.GetFiles(), 1)
		require.Equal(t, "x.go", string(review.GetFiles()[0].GetPath()))
		require.Equal(t, gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, review.GetFiles()[0].GetClass())

		require.Len(t, review.GetRequirements(), 1)
		require.Equal(t, int32(1), review.GetRequirements()[0].GetRequiredApprovals())
		require.False(t, review.GetRequirements()[0].GetSatisfied())
		require.False(t, review.GetApproved())
	})

	t.Run("uses the given base", func(t *testing.T) {
		review, err := env.repo.CreateReview(env.ctx, sdk.CreateReviewOptions{
			TargetBranch: "main",
			Head:         head.String(),
			Base:         base.String(),
		})
		require.NoError(t, err)
		require.Equal(t, base.String(), review.GetBaseCommitId())
	})

	t.Run("validates the arguments", func(t *testing.T) {
		_, err := env.repo.CreateReview(env.ctx, sdk.CreateReviewOptions{Head: head.String()})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "missing branch")

		_, err = env.repo.CreateReview(env.ctx, sdk.CreateReviewOptions{TargetBranch: "main"})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "missing head")

		_, err = env.repo.CreateReview(env.ctx, sdk.CreateReviewOptions{TargetBranch: "bad..name", Head: head.String()})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "invalid branch name")
	})

	t.Run("unknown branch and head are not found", func(t *testing.T) {
		_, err := env.repo.CreateReview(env.ctx, sdk.CreateReviewOptions{TargetBranch: "no-such-branch", Head: head.String()})
		require.Equal(t, codes.NotFound, status.Code(err), "branch: %v", err)

		_, err = env.repo.CreateReview(env.ctx, sdk.CreateReviewOptions{TargetBranch: "main", Head: "no-such-rev"})
		require.Equal(t, codes.NotFound, status.Code(err), "head: %v", err)
	})
}

func TestIntegration_GetReview(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	base := env.writeCommit(t, "main", map[string]string{"x.go": reviewBaseSource})
	head := env.writeCommit(t, "", map[string]string{"x.go": reviewCodeSource}, base)
	created := createTestReview(t, env, "main", head.String())

	got, err := env.repo.GetReview(env.ctx, created.GetId())
	require.NoError(t, err)
	require.Equal(t, created.GetId(), got.GetId())
	require.Equal(t, created.GetTitle(), got.GetTitle())
	require.Equal(t, head.String(), got.GetHeadCommitId())

	_, err = env.repo.GetReview(env.ctx, "")
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = env.repo.GetReview(env.ctx, "999999")
	require.True(t, sdk.IsNotFound(err), "got %v", err)
}

func TestIntegration_ListReviews(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	base := env.writeCommit(t, "main", map[string]string{"x.go": reviewBaseSource})
	env.writeCommit(t, "release", map[string]string{"x.go": reviewBaseSource})
	head := env.writeCommit(t, "", map[string]string{"x.go": reviewCodeSource}, base)

	first := createTestReview(t, env, "main", head.String())
	second := createTestReview(t, env, "main", head.String())
	third := createTestReview(t, env, "release", head.String())

	_, err := env.repo.CloseReview(env.ctx, first.GetId(), false)
	require.NoError(t, err)

	t.Run("open reviews, newest first", func(t *testing.T) {
		reviews, err := env.repo.ListReviews(env.ctx, sdk.ListReviewsOptions{})
		require.NoError(t, err)
		require.Len(t, reviews, 2, "the closed review is hidden")
		require.Equal(t, third.GetId(), reviews[0].GetId())
		require.Equal(t, second.GetId(), reviews[1].GetId())
	})

	t.Run("filters by target branch", func(t *testing.T) {
		reviews, err := env.repo.ListReviews(env.ctx, sdk.ListReviewsOptions{TargetBranch: "refs/heads/main"})
		require.NoError(t, err)
		require.Len(t, reviews, 1)
		require.Equal(t, second.GetId(), reviews[0].GetId())
	})

	t.Run("includes closed reviews", func(t *testing.T) {
		reviews, err := env.repo.ListReviews(env.ctx, sdk.ListReviewsOptions{TargetBranch: "main", IncludeClosed: true})
		require.NoError(t, err)
		require.Len(t, reviews, 2)
		require.Equal(t, second.GetId(), reviews[0].GetId())
		require.Equal(t, first.GetId(), reviews[1].GetId())
	})

	t.Run("limit bounds the listing", func(t *testing.T) {
		reviews, err := env.repo.ListReviews(env.ctx, sdk.ListReviewsOptions{IncludeClosed: true, Limit: 1})
		require.NoError(t, err)
		require.Len(t, reviews, 1)
		require.Equal(t, third.GetId(), reviews[0].GetId())
	})
}

func TestIntegration_UpdateReview(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	base := env.writeCommit(t, "main", map[string]string{"x.go": reviewBaseSource})
	head1 := env.writeCommit(t, "", map[string]string{"x.go": reviewCodeSource}, base)
	head2 := env.writeCommit(t, "", map[string]string{"x.go": reviewCodeSource + "\n"}, head1)
	review := createTestReview(t, env, "main", head1.String())

	t.Run("expected head must match", func(t *testing.T) {
		_, err := env.repo.UpdateReview(env.ctx, sdk.UpdateReviewOptions{
			ID:           review.GetId(),
			Head:         head2.String(),
			ExpectedHead: base.String(),
		})
		require.Equal(t, codes.FailedPrecondition, status.Code(err))

		updated, err := env.repo.UpdateReview(env.ctx, sdk.UpdateReviewOptions{
			ID:           review.GetId(),
			Head:         head2.String(),
			ExpectedHead: head1.String(),
		})
		require.NoError(t, err)
		require.Equal(t, head2.String(), updated.GetHeadCommitId())
	})

	t.Run("validates the arguments", func(t *testing.T) {
		_, err := env.repo.UpdateReview(env.ctx, sdk.UpdateReviewOptions{ID: review.GetId()})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "missing head")

		_, err = env.repo.UpdateReview(env.ctx, sdk.UpdateReviewOptions{Head: head1.String()})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "missing ID")
	})

	t.Run("unknown review is not found", func(t *testing.T) {
		_, err := env.repo.UpdateReview(env.ctx, sdk.UpdateReviewOptions{ID: "999999", Head: head1.String()})
		require.True(t, sdk.IsNotFound(err), "got %v", err)
	})
}

func TestIntegration_CommentReview(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	base := env.writeCommit(t, "main", map[string]string{"x.go": reviewBaseSource})
	head := env.writeCommit(t, "", map[string]string{"x.go": reviewCodeSource}, base)
	review := createTestReview(t, env, "main", head.String())

	comment, err := env.repo.CommentReview(env.ctx, sdk.CommentReviewOptions{
		ID:   review.GetId(),
		Body: "nit",
		Path: "x.go",
		Line: 3,
	})
	require.NoError(t, err)
	require.Equal(t, "nit", string(comment.GetBody()))
	require.Equal(t, "x.go", string(comment.GetPath()))
	require.Equal(t, int32(3), comment.GetLine())
	require.Equal(t, head.String(), comment.GetCommitId())

	t.Run("validates the arguments", func(t *testing.T) {
		_, err := env.repo.CommentReview(env.ctx, sdk.CommentReviewOptions{ID: review.GetId()})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "empty body")

		_, err = env.repo.CommentReview(env.ctx, sdk.CommentReviewOptions{ID: review.GetId(), Body: "x", Line: -1})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "negative line")

		_, err = env.repo.CommentReview(env.ctx, sdk.CommentReviewOptions{Body: "x"})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "missing ID")
	})

	t.Run("path must be a file of the review", func(t *testing.T) {
		_, err := env.repo.CommentReview(env.ctx, sdk.CommentReviewOptions{ID: review.GetId(), Body: "x", Path: "other.go"})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
	})

	t.Run("unknown review is not found", func(t *testing.T) {
		_, err := env.repo.CommentReview(env.ctx, sdk.CommentReviewOptions{ID: "999999", Body: "x"})
		require.True(t, sdk.IsNotFound(err), "got %v", err)
	})

	t.Run("comments are stored in order", func(t *testing.T) {
		got, err := env.repo.GetReview(env.ctx, review.GetId())
		require.NoError(t, err)
		require.Len(t, got.GetComments(), 1, "refused comments are not stored")
		require.Equal(t, "nit", string(got.GetComments()[0].GetBody()))
	})
}

func TestIntegration_CommentReviewConcurrently(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	base := env.writeCommit(t, "main", map[string]string{"x.go": reviewBaseSource})
	head := env.writeCommit(t, "", map[string]string{"x.go": reviewCodeSource}, base)
	review := createTestReview(t, env, "main", head.String())

	const n = 20
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			comment, err := env.repo.CommentReview(env.ctx, sdk.CommentReviewOptions{
				ID:   review.GetId(),
				Body: fmt.Sprintf("comment %d", i),
			})
			errs[i] = err
			if comment != nil {
				ids[i] = comment.GetId()
			}
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	for i := range n {
		require.NoError(t, errs[i])
		require.False(t, seen[ids[i]], "comment ID %q is not unique", ids[i])
		seen[ids[i]] = true
	}

	got, err := env.repo.GetReview(env.ctx, review.GetId())
	require.NoError(t, err)
	require.Len(t, got.GetComments(), n)
}

func TestIntegration_ApproveAndRequestChanges(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, allowSelfApproval)
	base := env.writeCommit(t, "main", map[string]string{"x.go": reviewBaseSource})
	head := env.writeCommit(t, "", map[string]string{"x.go": reviewCodeSource}, base)
	review := createTestReview(t, env, "main", head.String())
	require.False(t, review.GetApproved())

	approved, err := env.repo.ApproveReview(env.ctx, review.GetId(), "lgtm")
	require.NoError(t, err)
	require.Len(t, approved.GetVerdicts(), 1)
	verdict := approved.GetVerdicts()[0]
	require.Equal(t, gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED, verdict.GetKind())
	require.Equal(t, head.String(), verdict.GetCommitId(), "the verdict names the head it was given on")
	require.True(t, verdict.GetCurrent())
	require.True(t, approved.GetApproved())

	changed, err := env.repo.RequestChanges(env.ctx, review.GetId(), "wait")
	require.NoError(t, err)
	require.Len(t, changed.GetVerdicts(), 2)
	require.Equal(t, gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES, changed.GetVerdicts()[1].GetKind())
	require.False(t, changed.GetApproved(), "a change request blocks approval")

	reapproved, err := env.repo.ApproveReview(env.ctx, review.GetId(), "")
	require.NoError(t, err)
	require.True(t, reapproved.GetApproved(), "a later approval by the same actor supersedes the request")

	t.Run("unknown review is not found", func(t *testing.T) {
		_, err := env.repo.ApproveReview(env.ctx, "999999", "")
		require.True(t, sdk.IsNotFound(err), "got %v", err)
	})
}

func TestIntegration_AuthorApprovalDoesNotCountByDefault(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	base := env.writeCommit(t, "main", map[string]string{"x.go": reviewBaseSource})
	head := env.writeCommit(t, "", map[string]string{"x.go": reviewCodeSource}, base)
	review := createTestReview(t, env, "main", head.String())

	got, err := env.repo.ApproveReview(env.ctx, review.GetId(), "")
	require.NoError(t, err)
	require.False(t, got.GetApproved(), "the author cannot approve their own change under the default rules")
	require.Len(t, got.GetRequirements(), 1)
	require.Equal(t, int32(0), got.GetRequirements()[0].GetApprovals())
}

func TestIntegration_UpdateKeepsApprovalsForCommentOnlyChanges(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, allowSelfApproval)
	base := env.writeCommit(t, "main", map[string]string{"x.go": reviewBaseSource})
	head1 := env.writeCommit(t, "", map[string]string{"x.go": reviewCodeSource}, base)
	commentOnly := env.writeCommit(t, "", map[string]string{"x.go": reviewCommentSource}, head1)
	codeChange := env.writeCommit(t, "", map[string]string{"x.go": "package x\n\nfunc F() int { return 2 }\n"}, commentOnly)

	review := createTestReview(t, env, "main", head1.String())
	_, err := env.repo.ApproveReview(env.ctx, review.GetId(), "")
	require.NoError(t, err)

	kept, err := env.repo.UpdateReview(env.ctx, sdk.UpdateReviewOptions{ID: review.GetId(), Head: commentOnly.String()})
	require.NoError(t, err)
	require.True(t, kept.GetApproved(), "a comment-only change keeps the approval")
	require.True(t, kept.GetVerdicts()[0].GetCurrent())

	dropped, err := env.repo.UpdateReview(env.ctx, sdk.UpdateReviewOptions{ID: review.GetId(), Head: codeChange.String()})
	require.NoError(t, err)
	require.False(t, dropped.GetApproved(), "a code change needs a new approval")
	require.False(t, dropped.GetVerdicts()[0].GetCurrent())
}

func TestIntegration_CloseReview(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	base := env.writeCommit(t, "main", map[string]string{"x.go": reviewBaseSource})
	head := env.writeCommit(t, "", map[string]string{"x.go": reviewCodeSource}, base)

	landed := createTestReview(t, env, "main", head.String())
	closed, err := env.repo.CloseReview(env.ctx, landed.GetId(), true)
	require.NoError(t, err)
	require.Equal(t, sdk.ReviewLanded, closed.GetState())

	abandoned := createTestReview(t, env, "main", head.String())
	closed, err = env.repo.CloseReview(env.ctx, abandoned.GetId(), false)
	require.NoError(t, err)
	require.Equal(t, sdk.ReviewClosed, closed.GetState())

	t.Run("closed reviews accept no changes", func(t *testing.T) {
		_, err := env.repo.CommentReview(env.ctx, sdk.CommentReviewOptions{ID: landed.GetId(), Body: "late"})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "comment: %v", err)

		_, err = env.repo.ApproveReview(env.ctx, abandoned.GetId(), "")
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "approve: %v", err)

		_, err = env.repo.CloseReview(env.ctx, abandoned.GetId(), true)
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "close again: %v", err)
	})

	t.Run("validates the ID and reports unknown reviews", func(t *testing.T) {
		_, err := env.repo.CloseReview(env.ctx, "", false)
		require.Equal(t, codes.InvalidArgument, status.Code(err))

		_, err = env.repo.CloseReview(env.ctx, "999999", false)
		require.True(t, sdk.IsNotFound(err), "got %v", err)
	})
}
