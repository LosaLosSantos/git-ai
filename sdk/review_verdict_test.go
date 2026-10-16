package sdk_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// TestIntegration_VerdictBindsToTheReviewedHead checks that a verdict given on
// an explicit head is refused once the review has moved to another head. The
// reviewer read the reviewed head, so an approval must never land on the newer
// code. The subtests run in order: each one builds on the state of the last.
func TestIntegration_VerdictBindsToTheReviewedHead(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, allowSelfApproval)
	base := env.writeCommit(t, "main", map[string]string{"x.go": reviewBaseSource})
	reviewed := env.writeCommit(t, "", map[string]string{"x.go": reviewCodeSource}, base)
	moved := env.writeCommit(t, "", map[string]string{"x.go": "package x\n\nfunc F() int { return 99 }\n"}, reviewed)
	review := createTestReview(t, env, "main", reviewed.String())

	_, err := env.repo.UpdateReview(env.ctx, sdk.UpdateReviewOptions{ID: review.GetId(), Head: moved.String()})
	require.NoError(t, err)

	t.Run("approval of a head the review has left is refused", func(t *testing.T) {
		_, err := env.repo.ApproveReviewAt(env.ctx, sdk.VerdictOptions{ID: review.GetId(), Head: reviewed.String(), Body: "lgtm"})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "got %v", err)

		got, err := env.repo.GetReview(env.ctx, review.GetId())
		require.NoError(t, err)
		require.Empty(t, got.GetVerdicts(), "the refused verdict is not recorded")
		require.False(t, got.GetApproved())
	})

	t.Run("change request on a head the review has left is refused", func(t *testing.T) {
		_, err := env.repo.RequestChangesAt(env.ctx, sdk.VerdictOptions{ID: review.GetId(), Head: reviewed.String()})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "got %v", err)
	})

	t.Run("approval of the current head is recorded on that head", func(t *testing.T) {
		approved, err := env.repo.ApproveReviewAt(env.ctx, sdk.VerdictOptions{ID: review.GetId(), Head: moved.String(), Body: "lgtm"})
		require.NoError(t, err)
		require.Len(t, approved.GetVerdicts(), 1)
		require.Equal(t, gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED, approved.GetVerdicts()[0].GetKind())
		require.Equal(t, moved.String(), approved.GetVerdicts()[0].GetCommitId())
		require.True(t, approved.GetApproved())
	})

	t.Run("empty head means the current head", func(t *testing.T) {
		changed, err := env.repo.RequestChangesAt(env.ctx, sdk.VerdictOptions{ID: review.GetId(), Body: "wait"})
		require.NoError(t, err)
		require.Len(t, changed.GetVerdicts(), 2)
		require.Equal(t, gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES, changed.GetVerdicts()[1].GetKind())
		require.Equal(t, moved.String(), changed.GetVerdicts()[1].GetCommitId())
		require.False(t, changed.GetApproved(), "a change request blocks approval")
	})

	t.Run("validates the ID", func(t *testing.T) {
		_, err := env.repo.ApproveReviewAt(env.ctx, sdk.VerdictOptions{Head: moved.String()})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)

		_, err = env.repo.RequestChangesAt(env.ctx, sdk.VerdictOptions{Head: moved.String()})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
	})
}
