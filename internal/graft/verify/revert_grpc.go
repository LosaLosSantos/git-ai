package verify

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// OperationReverter implements Reverter with the UserRevert RPC of the
// OperationService. It acts as User on Repository.
type OperationReverter struct {
	Operations gitalypb.OperationServiceClient
	Repository *gitalypb.Repository
	// User is the user the revert is performed as. When nil, the committer of
	// the RevertRequest is used.
	User *gitalypb.User
	// TipOf resolves a branch to its tip. It is used by Tip and to tell a
	// branch that moved from another failure. See BranchTip.
	TipOf func(ctx context.Context, branch string) (string, error)
}

var _ Reverter = (*OperationReverter)(nil)

// BranchTip returns a TipOf function that resolves refs/heads/<branch> with
// backend.
func BranchTip(backend workspace.Backend) func(context.Context, string) (string, error) {
	return func(ctx context.Context, branch string) (string, error) {
		return backend.ResolveRevision(ctx, "refs/heads/"+branch)
	}
}

// Tip implements Reverter.
func (r *OperationReverter) Tip(ctx context.Context, branch string) (string, error) {
	if r.TipOf == nil {
		return "", errors.New("verify: operation reverter has no tip lookup")
	}
	return r.TipOf(ctx, branch)
}

// Revert implements Reverter. It refuses to revert without an expected old
// tip: the server would then overwrite the branch whatever it points to.
func (r *OperationReverter) Revert(ctx context.Context, req RevertRequest) (string, error) {
	if r.Operations == nil || r.Repository == nil {
		return "", errors.New("verify: operation reverter is not configured")
	}
	if req.Branch == "" || req.Commit == "" {
		return "", errors.New("verify: revert needs a branch and a commit")
	}
	if req.ExpectedOld == "" {
		return "", errors.New("verify: revert needs the expected old tip of the branch")
	}

	user, err := r.user(req)
	if err != nil {
		return "", err
	}

	resp, err := r.Operations.UserRevert(ctx, &gitalypb.UserRevertRequest{
		Repository:     r.Repository,
		User:           user,
		Commit:         &gitalypb.GitCommit{Id: req.Commit},
		BranchName:     []byte(req.Branch),
		Message:        []byte(req.Message),
		ExpectedOldOid: req.ExpectedOld,
	})
	if err != nil {
		if known := revertStatusError(err); known != nil {
			return "", known
		}
		// The compare-and-swap of the branch fails with an unstructured error.
		// When the branch no longer points to the expected tip, that is the
		// cause, and the caller retries onto the new tip.
		if r.branchMoved(ctx, req) {
			return "", fmt.Errorf("%w: %v", ErrBranchMoved, err)
		}
		return "", fmt.Errorf("user revert: %w", err)
	}

	// Older servers report the failures in the response instead of as errors.
	switch resp.GetCreateTreeErrorCode() {
	case gitalypb.UserRevertResponse_EMPTY:
		return "", ErrAlreadyReverted
	case gitalypb.UserRevertResponse_CONFLICT:
		return "", fmt.Errorf("%w: %s", ErrRevertConflict, resp.GetCreateTreeError())
	}
	if msg := resp.GetCreateTreeError(); msg != "" {
		return "", fmt.Errorf("create revert tree: %s", msg)
	}
	if msg := resp.GetCommitError(); msg != "" {
		return "", fmt.Errorf("%w: %s", ErrBranchMoved, msg)
	}
	if msg := resp.GetPreReceiveError(); msg != "" {
		return "", fmt.Errorf("pre-receive hook rejected the revert: %s", msg)
	}

	id := resp.GetBranchUpdate().GetCommitId()
	if id == "" {
		return "", errors.New("user revert returned no commit")
	}
	return id, nil
}

// user returns the user the revert is performed as.
func (r *OperationReverter) user(req RevertRequest) (*gitalypb.User, error) {
	if r.User != nil {
		return r.User, nil
	}
	if req.Committer.Name == "" || req.Committer.Email == "" {
		return nil, errors.New("verify: revert needs a committer name and email")
	}
	return &gitalypb.User{
		Name:  []byte(req.Committer.Name),
		Email: []byte(req.Committer.Email),
	}, nil
}

// branchMoved reports whether the branch of req no longer points to the
// expected old tip. It is false when the tip cannot be read.
func (r *OperationReverter) branchMoved(ctx context.Context, req RevertRequest) bool {
	if r.TipOf == nil || ctx.Err() != nil {
		return false
	}
	tip, err := r.TipOf(ctx, req.Branch)
	return err == nil && tip != "" && tip != req.ExpectedOld
}

// revertStatusError maps the structured details of a UserRevert error to the
// Reverter sentinel errors. It returns nil for other errors.
func revertStatusError(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return nil
	}
	for _, detail := range st.Details() {
		revertErr, ok := detail.(*gitalypb.UserRevertError)
		if !ok {
			continue
		}
		switch revertErr.GetError().(type) {
		case *gitalypb.UserRevertError_MergeConflict:
			return fmt.Errorf("%w: %s", ErrRevertConflict, st.Message())
		case *gitalypb.UserRevertError_ChangesAlreadyApplied:
			return ErrAlreadyReverted
		case *gitalypb.UserRevertError_NotAncestor:
			return fmt.Errorf("%w: %s", ErrBranchMoved, st.Message())
		case *gitalypb.UserRevertError_CustomHook:
			return fmt.Errorf("pre-receive hook rejected the revert: %s", st.Message())
		}
	}
	return nil
}
