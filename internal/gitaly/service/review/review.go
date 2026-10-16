package review

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/review/classify"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// CreateReview opens a review of head_commit_id against target_branch.
func (s *server) CreateReview(ctx context.Context, req *gitalypb.CreateReviewRequest) (*gitalypb.CreateReviewResponse, error) {
	if err := s.deps.GetLocator().ValidateRepository(ctx, req.GetRepository()); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	branch := string(req.GetTargetBranch())
	if !validBranchName(branch) {
		return nil, structerr.NewInvalidArgument("invalid target branch %q", branch)
	}

	repo, st, err := s.openRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}

	tip, err := resolveBranch(ctx, repo, branch)
	if err != nil {
		return nil, err
	}

	head, err := resolveCommit(ctx, repo, req.GetHeadCommitId(), "head_commit_id")
	if err != nil {
		return nil, err
	}

	var base git.ObjectID
	if req.GetBaseCommitId() != "" {
		base, err = resolveCommit(ctx, repo, req.GetBaseCommitId(), "base_commit_id")
		if err != nil {
			return nil, err
		}

		// The files of a review are what landing the head on the branch
		// would change, so an explicit base must be a merge base of the head
		// and the branch tip. Any other commit would misstate the change: the
		// head itself, or a commit with the head's tree, hides every change,
		// and an older common ancestor hides changes that revert what the
		// branch gained since.
		ok, err := isMergeBase(ctx, repo, base, head, tip)
		if err != nil {
			return nil, structerr.NewInternal("%w", err)
		}
		if !ok {
			return nil, structerr.NewFailedPrecondition(
				"base_commit_id %s is not a merge base of head %s and branch %q", base, head, branch)
		}
	} else {
		var found bool
		base, found, err = mergeBase(ctx, repo, head, tip)
		if err != nil {
			return nil, structerr.NewInternal("%w", err)
		}
		if !found {
			return nil, structerr.NewFailedPrecondition("head and branch %q have no common ancestor", branch)
		}
	}

	files, err := changedFiles(ctx, repo, base, head)
	if err != nil {
		return nil, structerr.NewInternal("%w", err)
	}

	stored, err := st.Create(&gitalypb.Review{
		TargetBranch: []byte(branch),
		BaseCommitId: base.String(),
		HeadCommitId: head.String(),
		Author:       identity.Actor(ctx, anonymousActor),
		Title:        req.GetTitle(),
		Description:  req.GetDescription(),
		State:        gitalypb.Review_STATE_OPEN_UNSPECIFIED,
		Files:        files,
	})
	if err != nil {
		return nil, toStatus(err)
	}

	return &gitalypb.CreateReviewResponse{Review: s.withRequirements(stored)}, nil
}

// GetReview returns one review with its classification, approvals and the
// evaluation of the approval rules.
func (s *server) GetReview(ctx context.Context, req *gitalypb.GetReviewRequest) (*gitalypb.GetReviewResponse, error) {
	if err := s.deps.GetLocator().ValidateRepository(ctx, req.GetRepository()); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}
	if req.GetId() == "" {
		return nil, structerr.NewInvalidArgument("review id is required")
	}

	_, st, err := s.openRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}

	r, err := st.Get(req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}

	return &gitalypb.GetReviewResponse{Review: s.withRequirements(r)}, nil
}

// ListReviews lists reviews, newest first. Listings carry neither comments nor
// verdict bodies.
func (s *server) ListReviews(req *gitalypb.ListReviewsRequest, stream gitalypb.ReviewService_ListReviewsServer) error {
	ctx := stream.Context()

	if err := s.deps.GetLocator().ValidateRepository(ctx, req.GetRepository()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	_, st, err := s.openRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}

	limit := int(req.GetLimit())
	if limit == 0 {
		limit = defaultListLimit
	}

	reviews, err := st.List(string(req.GetTargetBranch()), req.GetIncludeClosed(), limit)
	if err != nil {
		return toStatus(err)
	}

	for start := 0; start < len(reviews); start += listBatchSize {
		end := min(start+listBatchSize, len(reviews))

		batch := make([]*gitalypb.Review, 0, end-start)
		for _, r := range reviews[start:end] {
			r.Comments = nil
			for _, v := range r.GetVerdicts() {
				v.Body = nil
			}
			batch = append(batch, s.withRequirements(r))
		}

		if err := stream.Send(&gitalypb.ListReviewsResponse{Reviews: batch}); err != nil {
			return err
		}
	}

	return nil
}

// UpdateReview moves the review to a new head commit. Only the author may move
// the head: the rules exclude the author from approving their own change, and
// that exclusion would mean nothing if another identity could put its own code
// under the author's name and approve it. The new head must descend from the
// review's base, otherwise the files computed against the base would not be
// the change of the head. A verdict stays current only when every file changed
// since the verdict's commit is a comment-only change or a change of a file
// that is documentation by name.
func (s *server) UpdateReview(ctx context.Context, req *gitalypb.UpdateReviewRequest) (*gitalypb.UpdateReviewResponse, error) {
	if err := s.deps.GetLocator().ValidateRepository(ctx, req.GetRepository()); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}
	if req.GetId() == "" {
		return nil, structerr.NewInvalidArgument("review id is required")
	}

	repo, st, err := s.openRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}

	head, err := resolveCommit(ctx, repo, req.GetHeadCommitId(), "head_commit_id")
	if err != nil {
		return nil, err
	}

	updated, err := st.Update(req.GetId(), func(r *gitalypb.Review) error {
		if err := checkAuthor(ctx, r, "update"); err != nil {
			return err
		}
		if err := checkOpen(r); err != nil {
			return err
		}
		if expected := req.GetExpectedHeadCommitId(); expected != "" && expected != r.GetHeadCommitId() {
			return structerr.NewFailedPrecondition("review head is %s, expected %s", r.GetHeadCommitId(), expected)
		}
		if head.String() == r.GetHeadCommitId() {
			return nil
		}

		base := git.ObjectID(r.GetBaseCommitId())
		descends, err := repo.IsAncestor(ctx, base.Revision(), head.Revision())
		if err != nil {
			if !errors.As(err, new(localrepo.InvalidCommitError)) {
				return structerr.NewInternal("%w", err)
			}
			descends = false
		}
		if !descends {
			return structerr.NewFailedPrecondition("head %s does not descend from the review base %s", head, base)
		}

		files, err := changedFiles(ctx, repo, base, head)
		if err != nil {
			return structerr.NewInternal("%w", err)
		}

		// Verdicts on the same commit have the same outcome, so the change
		// since that commit is computed once per distinct commit.
		survivesByCommit := map[string]bool{}
		for _, v := range r.GetVerdicts() {
			if !v.GetCurrent() {
				continue
			}
			survives, ok := survivesByCommit[v.GetCommitId()]
			if !ok {
				survives, err = onlyCommentOrDocs(ctx, repo, git.ObjectID(v.GetCommitId()), head)
				if err != nil {
					return structerr.NewInternal("%w", err)
				}
				survivesByCommit[v.GetCommitId()] = survives
			}
			v.Current = survives
		}

		r.Files = files
		r.HeadCommitId = head.String()
		return nil
	})
	if err != nil {
		return nil, toStatus(err)
	}

	return &gitalypb.UpdateReviewResponse{Review: s.withRequirements(updated)}, nil
}

// onlyCommentOrDocs reports whether every file changed between from and to is
// a comment-only or docs change. An empty change qualifies. A from commit that
// is no longer in the object database (for example, pruned after a force push)
// cannot be compared, so the change does not qualify.
//
// A DOCS change qualifies only for a file that is documentation by its own
// name (classify.IsDocumentationFile). The classifier also calls a file DOCS
// when it merely sits under a doc or docs directory, and such a file can be
// source code that is compiled (internal/doc/handler.go): changing it after an
// approval must not keep the approval.
func onlyCommentOrDocs(ctx context.Context, repo *localrepo.Repo, from, to git.ObjectID) (bool, error) {
	if from == to {
		return true, nil
	}

	if _, err := repo.ReadObjectInfo(ctx, from.Revision()); err != nil {
		if errors.As(err, new(localrepo.InvalidObjectError)) {
			return false, nil
		}
		return false, err
	}

	files, err := changedFiles(ctx, repo, from, to)
	if err != nil {
		return false, err
	}

	for _, f := range files {
		switch f.GetClass() {
		case gitalypb.ReviewFile_CHANGE_CLASS_COMMENT_ONLY:
		case gitalypb.ReviewFile_CHANGE_CLASS_DOCS:
			if !classify.IsDocumentationFile(string(f.GetPath())) {
				return false, nil
			}
		default:
			return false, nil
		}
	}

	return true, nil
}

// AddReviewComment adds a comment, optionally anchored to a file line.
func (s *server) AddReviewComment(ctx context.Context, req *gitalypb.AddReviewCommentRequest) (*gitalypb.AddReviewCommentResponse, error) {
	if err := s.deps.GetLocator().ValidateRepository(ctx, req.GetRepository()); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}
	if req.GetId() == "" {
		return nil, structerr.NewInvalidArgument("review id is required")
	}
	if len(req.GetBody()) == 0 {
		return nil, structerr.NewInvalidArgument("comment body is required")
	}
	if len(req.GetBody()) > maxBodyBytes {
		return nil, structerr.NewInvalidArgument("comment body exceeds %d bytes", maxBodyBytes)
	}
	if req.GetLine() < 0 {
		return nil, structerr.NewInvalidArgument("line must not be negative")
	}

	_, st, err := s.openRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}

	var comment *gitalypb.ReviewComment
	_, err = st.Update(req.GetId(), func(r *gitalypb.Review) error {
		if err := checkOpen(r); err != nil {
			return err
		}
		if path := req.GetPath(); len(path) > 0 && !hasFile(r, path) {
			return structerr.NewInvalidArgument("path %q is not a file of the review", path)
		}

		// Ids are positions: comments are only appended, under the store lock.
		comment = &gitalypb.ReviewComment{
			Id:        strconv.Itoa(len(r.GetComments()) + 1),
			Actor:     identity.Actor(ctx, anonymousActor),
			Body:      req.GetBody(),
			Path:      req.GetPath(),
			Line:      req.GetLine(),
			CommitId:  r.GetHeadCommitId(),
			CreatedAt: timestamppb.Now(),
		}
		r.Comments = append(r.Comments, comment)
		return nil
	})
	if err != nil {
		return nil, toStatus(err)
	}

	return &gitalypb.AddReviewCommentResponse{Comment: comment}, nil
}

// SubmitReviewVerdict records an approval or a change request by the caller for
// the review's current head commit.
func (s *server) SubmitReviewVerdict(ctx context.Context, req *gitalypb.SubmitReviewVerdictRequest) (*gitalypb.SubmitReviewVerdictResponse, error) {
	if err := s.deps.GetLocator().ValidateRepository(ctx, req.GetRepository()); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}
	if req.GetId() == "" {
		return nil, structerr.NewInvalidArgument("review id is required")
	}
	switch req.GetKind() {
	case gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED, gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES:
	default:
		return nil, structerr.NewInvalidArgument("unknown verdict kind %d", req.GetKind())
	}
	if req.GetHeadCommitId() == "" {
		return nil, structerr.NewInvalidArgument("head_commit_id is required")
	}
	if len(req.GetBody()) > maxBodyBytes {
		return nil, structerr.NewInvalidArgument("verdict body exceeds %d bytes", maxBodyBytes)
	}

	_, st, err := s.openRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}

	updated, err := st.Update(req.GetId(), func(r *gitalypb.Review) error {
		if err := checkOpen(r); err != nil {
			return err
		}
		if req.GetHeadCommitId() != r.GetHeadCommitId() {
			return structerr.NewFailedPrecondition("review head is %s, verdict was given on %s", r.GetHeadCommitId(), req.GetHeadCommitId())
		}

		// Earlier verdicts by the same actor are not rewritten: the rules
		// count only the latest verdict of each actor.
		r.Verdicts = append(r.Verdicts, &gitalypb.ReviewVerdict{
			Actor:     identity.Actor(ctx, anonymousActor),
			Kind:      req.GetKind(),
			CommitId:  r.GetHeadCommitId(),
			Body:      req.GetBody(),
			CreatedAt: timestamppb.Now(),
			Current:   true,
		})
		return nil
	})
	if err != nil {
		return nil, toStatus(err)
	}

	return &gitalypb.SubmitReviewVerdictResponse{Review: s.withRequirements(updated)}, nil
}

// CloseReview closes a review without landing it, or marks it landed. Only the
// author may do either: a closed review accepts no further changes, so any
// other identity could otherwise end someone else's review for good.
func (s *server) CloseReview(ctx context.Context, req *gitalypb.CloseReviewRequest) (*gitalypb.CloseReviewResponse, error) {
	if err := s.deps.GetLocator().ValidateRepository(ctx, req.GetRepository()); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}
	if req.GetId() == "" {
		return nil, structerr.NewInvalidArgument("review id is required")
	}

	_, st, err := s.openRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}

	updated, err := st.Update(req.GetId(), func(r *gitalypb.Review) error {
		if err := checkAuthor(ctx, r, "close"); err != nil {
			return err
		}
		if err := checkOpen(r); err != nil {
			return err
		}

		r.State = gitalypb.Review_STATE_CLOSED
		if req.GetLanded() {
			r.State = gitalypb.Review_STATE_LANDED
		}
		return nil
	})
	if err != nil {
		return nil, toStatus(err)
	}

	return &gitalypb.CloseReviewResponse{Review: s.withRequirements(updated)}, nil
}

// checkAuthor returns PermissionDenied unless the caller is the author of the
// review. action names what the caller tried to do.
func checkAuthor(ctx context.Context, r *gitalypb.Review, action string) error {
	if actor := identity.Actor(ctx, anonymousActor); actor != r.GetAuthor() {
		return structerr.NewPermissionDenied("only the author %q may %s review %s, not %q", r.GetAuthor(), action, r.GetId(), actor)
	}
	return nil
}

// checkOpen returns FailedPrecondition unless the review is open.
func checkOpen(r *gitalypb.Review) error {
	if r.GetState() == gitalypb.Review_STATE_OPEN_UNSPECIFIED {
		return nil
	}
	return structerr.NewFailedPrecondition("review is %s and accepts no changes", stateName(r.GetState()))
}

func stateName(state gitalypb.Review_State) string {
	switch state {
	case gitalypb.Review_STATE_LANDED:
		return "landed"
	case gitalypb.Review_STATE_CLOSED:
		return "closed"
	default:
		return "open"
	}
}

// hasFile reports whether path is one of the files of the review.
func hasFile(r *gitalypb.Review, path []byte) bool {
	for _, f := range r.GetFiles() {
		if string(f.GetPath()) == string(path) {
			return true
		}
	}
	return false
}

// toStatus maps errors of the review store to gRPC status errors. Errors that
// already carry a status are returned unchanged.
func toStatus(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return structerr.NewNotFound("review not found")
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return structerr.NewInternal("%w", fmt.Errorf("review store: %w", err))
}
