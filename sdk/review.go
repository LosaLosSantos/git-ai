package sdk

import (
	"context"
	"errors"
	"io"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// Review is one code review: a range of commits that targets a branch, with
// its file classification, verdicts, comments and approval state.
type Review = gitalypb.Review

// ReviewFile is the classification of one file changed by a review.
type ReviewFile = gitalypb.ReviewFile

// ReviewVerdict is an approval or a change request given on a review.
type ReviewVerdict = gitalypb.ReviewVerdict

// ReviewComment is a comment on a review, optionally anchored to a file line.
type ReviewComment = gitalypb.ReviewComment

// ReviewRequirement is the evaluation of one approval rule for a review.
type ReviewRequirement = gitalypb.ReviewRequirement

// ReviewState is the lifecycle state of a review.
type ReviewState = gitalypb.Review_State

// Review states.
const (
	// ReviewOpen is a review that has not landed and is not closed.
	ReviewOpen = gitalypb.Review_STATE_OPEN_UNSPECIFIED
	// ReviewLanded is a review whose change landed.
	ReviewLanded = gitalypb.Review_STATE_LANDED
	// ReviewClosed is a review closed without landing.
	ReviewClosed = gitalypb.Review_STATE_CLOSED
)

// CreateReviewOptions describes a review to open.
type CreateReviewOptions struct {
	// TargetBranch is the branch the change lands on, without the refs/heads/
	// prefix.
	TargetBranch string
	// Head is the commit ID of the change.
	Head string
	// Base is the commit ID the change is based on. Empty means the merge base
	// of Head and the tip of TargetBranch.
	Base string
	// Title is a one-line summary.
	Title string
	// Description is free text.
	Description string
}

// ListReviewsOptions filters ListReviews.
type ListReviewsOptions struct {
	// TargetBranch limits the listing to one target branch. Empty lists all.
	TargetBranch string
	// IncludeClosed also lists landed and closed reviews.
	IncludeClosed bool
	// Limit bounds the number of reviews. Zero means the server default.
	Limit uint32
}

// UpdateReviewOptions moves a review to a new head commit.
type UpdateReviewOptions struct {
	// ID identifies the review.
	ID string
	// Head is the new head commit ID.
	Head string
	// ExpectedHead, if set, must be the current head. Otherwise the update
	// fails with a FailedPrecondition error.
	ExpectedHead string
}

// CommentReviewOptions describes a comment on a review.
type CommentReviewOptions struct {
	// ID identifies the review.
	ID string
	// Body is the comment text.
	Body string
	// Path anchors the comment to a file of the review. Optional.
	Path string
	// Line anchors the comment to a 1-based line of Path. Zero means no line.
	Line int32
}

// CreateReview opens a review of opts.Head against opts.TargetBranch. The
// server classifies the changed files and evaluates the approval rules.
func (r *Repo) CreateReview(ctx context.Context, opts CreateReviewOptions) (*Review, error) {
	switch {
	case reviewBranchName(opts.TargetBranch) == "":
		return nil, status.Error(codes.InvalidArgument, "target branch is required")
	case opts.Head == "":
		return nil, status.Error(codes.InvalidArgument, "head commit is required")
	}

	resp, err := r.reviews().CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository:   r.repository(),
		TargetBranch: []byte(reviewBranchName(opts.TargetBranch)),
		HeadCommitId: opts.Head,
		BaseCommitId: opts.Base,
		Title:        []byte(opts.Title),
		Description:  []byte(opts.Description),
	})
	if err != nil {
		return nil, err
	}
	return resp.GetReview(), nil
}

// GetReview returns the review with the given ID, with its requirements and
// approval state evaluated for the current head.
func (r *Repo) GetReview(ctx context.Context, id string) (*Review, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "review ID is required")
	}

	resp, err := r.reviews().GetReview(ctx, &gitalypb.GetReviewRequest{
		Repository: r.repository(),
		Id:         id,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetReview(), nil
}

// ListReviews returns reviews, newest first. The reviews carry no comments
// and no verdict bodies, to keep listings small; use GetReview for those.
func (r *Repo) ListReviews(ctx context.Context, opts ListReviewsOptions) ([]*Review, error) {
	stream, err := r.reviews().ListReviews(ctx, &gitalypb.ListReviewsRequest{
		Repository:    r.repository(),
		TargetBranch:  []byte(reviewBranchName(opts.TargetBranch)),
		IncludeClosed: opts.IncludeClosed,
		Limit:         opts.Limit,
	})
	if err != nil {
		return nil, err
	}

	var reviews []*Review
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return reviews, nil
		}
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, msg.GetReviews()...)
	}
}

// UpdateReview moves the review to a new head commit. Verdicts given on the
// previous head stop counting unless the change kept them: the server keeps
// them when only comments and docs changed.
func (r *Repo) UpdateReview(ctx context.Context, opts UpdateReviewOptions) (*Review, error) {
	switch {
	case opts.ID == "":
		return nil, status.Error(codes.InvalidArgument, "review ID is required")
	case opts.Head == "":
		return nil, status.Error(codes.InvalidArgument, "head commit is required")
	}

	resp, err := r.reviews().UpdateReview(ctx, &gitalypb.UpdateReviewRequest{
		Repository:           r.repository(),
		Id:                   opts.ID,
		HeadCommitId:         opts.Head,
		ExpectedHeadCommitId: opts.ExpectedHead,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetReview(), nil
}

// CommentReview adds a comment to the review, optionally anchored to a line
// of one of its files.
func (r *Repo) CommentReview(ctx context.Context, opts CommentReviewOptions) (*ReviewComment, error) {
	switch {
	case opts.ID == "":
		return nil, status.Error(codes.InvalidArgument, "review ID is required")
	case opts.Body == "":
		return nil, status.Error(codes.InvalidArgument, "comment body is required")
	case opts.Line < 0:
		return nil, status.Error(codes.InvalidArgument, "line must not be negative")
	}

	resp, err := r.reviews().AddReviewComment(ctx, &gitalypb.AddReviewCommentRequest{
		Repository: r.repository(),
		Id:         opts.ID,
		Body:       []byte(opts.Body),
		Path:       []byte(opts.Path),
		Line:       opts.Line,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetComment(), nil
}

// VerdictOptions describes an approval or a change request.
type VerdictOptions struct {
	// ID identifies the review.
	ID string
	// Head is the commit the verdict is given on. It must be the review's
	// current head, otherwise the server fails the call with FailedPrecondition.
	// Pass the head that was reviewed, so that a verdict never lands on code the
	// reviewer did not see. Empty means the review's current head as it is at
	// the time of the call; that is only safe when the caller has just read the
	// review and checked the head, so prefer setting Head.
	Head string
	// Body is an optional message.
	Body string
}

// ApproveReview records an approval of the review's current head, the same as
// ApproveReviewAt with an empty Head. body is an optional message. Use
// ApproveReviewAt to approve a specific head.
func (r *Repo) ApproveReview(ctx context.Context, id, body string) (*Review, error) {
	return r.ApproveReviewAt(ctx, VerdictOptions{ID: id, Body: body})
}

// ApproveReviewAt records an approval of opts.Head. If the review has moved to
// another head since the caller read it, the call fails with FailedPrecondition
// and no approval is recorded.
func (r *Repo) ApproveReviewAt(ctx context.Context, opts VerdictOptions) (*Review, error) {
	return r.submitVerdict(ctx, opts, gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED)
}

// RequestChanges records a change request on the review's current head, the
// same as RequestChangesAt with an empty Head. A later approval by the same
// actor supersedes it.
func (r *Repo) RequestChanges(ctx context.Context, id, body string) (*Review, error) {
	return r.RequestChangesAt(ctx, VerdictOptions{ID: id, Body: body})
}

// RequestChangesAt records a change request on opts.Head. It fails with
// FailedPrecondition when the review has moved to another head.
func (r *Repo) RequestChangesAt(ctx context.Context, opts VerdictOptions) (*Review, error) {
	return r.submitVerdict(ctx, opts, gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES)
}

// submitVerdict records a verdict on opts.Head. With an empty head it first
// reads the current head of the review and uses that.
func (r *Repo) submitVerdict(ctx context.Context, opts VerdictOptions, kind gitalypb.ReviewVerdict_Kind) (*Review, error) {
	if opts.ID == "" {
		return nil, status.Error(codes.InvalidArgument, "review ID is required")
	}

	head := opts.Head
	if head == "" {
		current, err := r.GetReview(ctx, opts.ID)
		if err != nil {
			return nil, err
		}
		head = current.GetHeadCommitId()
	}

	resp, err := r.reviews().SubmitReviewVerdict(ctx, &gitalypb.SubmitReviewVerdictRequest{
		Repository:   r.repository(),
		Id:           opts.ID,
		Kind:         kind,
		Body:         []byte(opts.Body),
		HeadCommitId: head,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetReview(), nil
}

// CloseReview closes the review without landing it. With landed set, it marks
// the review landed instead, which is for tools that land the change
// themselves.
func (r *Repo) CloseReview(ctx context.Context, id string, landed bool) (*Review, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "review ID is required")
	}

	resp, err := r.reviews().CloseReview(ctx, &gitalypb.CloseReviewRequest{
		Repository: r.repository(),
		Id:         id,
		Landed:     landed,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetReview(), nil
}

// reviews returns the review service client on the connection of the client.
func (r *Repo) reviews() gitalypb.ReviewServiceClient {
	return gitalypb.NewReviewServiceClient(r.client.conn)
}

// reviewBranchName strips the refs/heads/ prefix, because the server names
// target branches without it.
func reviewBranchName(branch string) string {
	return strings.TrimPrefix(branch, "refs/heads/")
}
