package verify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// ScratchBranchPrefix is the namespace of the temporary branches that hold the
// scratch commits of a bisection. A branch is created for one test run and
// deleted after it; one that is left behind (the verifier was killed during a
// run) can be deleted by hand.
const ScratchBranchPrefix = "graft-verify/scratch/"

// scratchReleaseTimeout bounds the deletion of a scratch branch, which runs
// even when the context of the bisection is done.
const scratchReleaseTimeout = 30 * time.Second

// OperationScratch implements ScratchReverter with the UserRevert RPC of the
// OperationService. The scratch commit is built by the server, with the same
// merge as an automatic revert, on a temporary branch under
// ScratchBranchPrefix, which UserDeleteBranch removes on release. The verified
// branch is never touched.
type OperationScratch struct {
	Operations gitalypb.OperationServiceClient
	Repository *gitalypb.Repository
	// Committer is the user the scratch reverts are performed as.
	Committer Signature
}

var _ ScratchReverter = (*OperationScratch)(nil)

// RevertOnto implements ScratchReverter.
func (s *OperationScratch) RevertOnto(ctx context.Context, base string, culprits []string) (string, func() error, error) {
	noop := func() error { return nil }
	if s.Operations == nil || s.Repository == nil {
		return "", noop, errors.New("verify: scratch reverter is not configured")
	}
	if base == "" {
		return "", noop, errors.New("verify: scratch revert needs a base commit")
	}
	if s.Committer.Name == "" || s.Committer.Email == "" {
		return "", noop, errors.New("verify: scratch revert needs a committer name and email")
	}
	user := &gitalypb.User{Name: []byte(s.Committer.Name), Email: []byte(s.Committer.Email)}

	branch, err := scratchBranchName()
	if err != nil {
		return "", noop, err
	}

	tip := ""
	release := noop
	for _, culprit := range culprits {
		req := &gitalypb.UserRevertRequest{
			Repository: s.Repository,
			User:       user,
			Commit:     &gitalypb.GitCommit{Id: culprit},
			BranchName: []byte(branch),
			Message:    []byte(fmt.Sprintf("graft-verify scratch revert of %s\n", culprit)),
		}
		if tip == "" {
			// The first revert creates the branch from base. A full object ID is
			// taken as the object, never as a reference of the same name.
			req.StartBranchName = []byte(base)
		} else {
			req.ExpectedOldOid = tip
		}

		id, err := s.revert(ctx, req)
		switch {
		case errors.Is(err, ErrAlreadyReverted):
			continue
		case err != nil:
			return "", noop, errors.Join(fmt.Errorf("scratch revert of %s onto %s: %w", culprit, base, err), release())
		}
		tip = id
		release = s.releaser(ctx, branch, user, tip)
	}

	if tip == "" {
		return base, noop, nil
	}
	return tip, release, nil
}

// revert sends one UserRevert and maps its failures like OperationReverter.
func (s *OperationScratch) revert(ctx context.Context, req *gitalypb.UserRevertRequest) (string, error) {
	resp, err := s.Operations.UserRevert(ctx, req)
	if err != nil {
		if known := revertStatusError(err); known != nil {
			return "", known
		}
		return "", fmt.Errorf("user revert: %w", err)
	}
	switch resp.GetCreateTreeErrorCode() {
	case gitalypb.UserRevertResponse_EMPTY:
		return "", ErrAlreadyReverted
	case gitalypb.UserRevertResponse_CONFLICT:
		return "", fmt.Errorf("%w: %s", ErrRevertConflict, resp.GetCreateTreeError())
	}
	for _, msg := range []string{resp.GetCreateTreeError(), resp.GetCommitError(), resp.GetPreReceiveError()} {
		if msg != "" {
			return "", fmt.Errorf("user revert: %s", msg)
		}
	}
	id := resp.GetBranchUpdate().GetCommitId()
	if id == "" {
		return "", errors.New("user revert returned no commit")
	}
	return id, nil
}

// releaser returns the function that deletes branch, provided it still points
// to tip. It runs without the cancellation of ctx, so that a bisection that is
// cancelled still removes its branch.
func (s *OperationScratch) releaser(ctx context.Context, branch string, user *gitalypb.User, tip string) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scratchReleaseTimeout)
		defer cancel()
		if _, err := s.Operations.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{
			Repository:     s.Repository,
			BranchName:     []byte(branch),
			User:           user,
			ExpectedOldOid: tip,
		}); err != nil {
			return fmt.Errorf("delete scratch branch %s: %w", branch, err)
		}
		return nil
	}
}

// scratchBranchName returns a new branch name under ScratchBranchPrefix.
func scratchBranchName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("scratch branch name: %w", err)
	}
	return ScratchBranchPrefix + hex.EncodeToString(b[:]), nil
}
