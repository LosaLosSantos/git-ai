package ref

import (
	"context"
	"errors"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

func (s *server) FindBranch(ctx context.Context, req *gitalypb.FindBranchRequest) (*gitalypb.FindBranchResponse, error) {
	repository := req.GetRepository()
	if err := s.locator.ValidateRepository(ctx, repository); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}
	if len(req.GetName()) == 0 {
		return nil, structerr.NewInvalidArgument("Branch name cannot be empty")
	}

	repo := s.localRepoFactory.Build(repository)

	branchName := git.NewReferenceNameFromBranchName(string(req.GetName()))
	branchRef, err := repo.GetReference(ctx, branchName)
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return &gitalypb.FindBranchResponse{}, nil
		}

		if errors.Is(err, git.ErrReferenceAmbiguous) {
			return nil, structerr.NewInvalidArgument("target reference is ambiguous: %w", err)
		}

		return nil, err
	}
	commit, err := repo.ReadCommit(ctx, git.Revision(branchRef.Target))
	if err != nil {
		return nil, err
	}

	branch, ok := branchName.Branch()
	if !ok {
		return nil, structerr.NewInvalidArgument("reference is not a branch")
	}

	return &gitalypb.FindBranchResponse{
		Branch: &gitalypb.Branch{
			Name:         []byte(branch),
			TargetCommit: commit,
		},
	}, nil
}
