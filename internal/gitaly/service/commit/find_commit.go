package commit

import (
	"context"
	"errors"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func validateFindCommitRequest(ctx context.Context, locator storage.Locator, in *gitalypb.FindCommitRequest) error {
	if err := locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		return err
	}
	if err := git.ValidateRevision(in.GetRevision()); err != nil {
		return err
	}
	return nil
}

func (s *server) FindCommit(ctx context.Context, in *gitalypb.FindCommitRequest) (*gitalypb.FindCommitResponse, error) {
	if err := validateFindCommitRequest(ctx, s.locator, in); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}
	repo := s.localRepoFactory.Build(in.GetRepository())

	var opts []localrepo.ReadCommitOpt
	if in.GetTrailers() {
		opts = []localrepo.ReadCommitOpt{localrepo.WithTrailers()}
	}

	commit, err := repo.ReadCommit(ctx, git.Revision(in.GetRevision()), opts...)
	if err != nil {
		if errors.Is(err, localrepo.ErrObjectNotFound) {
			return &gitalypb.FindCommitResponse{}, nil
		}
		return &gitalypb.FindCommitResponse{}, err
	}

	return &gitalypb.FindCommitResponse{Commit: commit}, nil
}
