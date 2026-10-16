package operations

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

type cherryPickOrRevertRequest interface {
	GetRepository() *gitalypb.Repository
	GetUser() *gitalypb.User
	GetCommit() *gitalypb.GitCommit
	GetBranchName() []byte
	GetMessage() []byte
}

func validateCherryPickOrRevertRequest(ctx context.Context, locator storage.Locator, req cherryPickOrRevertRequest) error {
	if err := locator.ValidateRepository(ctx, req.GetRepository()); err != nil {
		return err
	}

	if req.GetUser() == nil {
		return errors.New("empty User")
	}

	if req.GetCommit() == nil {
		return errors.New("empty Commit")
	}

	if len(req.GetBranchName()) == 0 {
		return errors.New("empty BranchName")
	}

	if len(req.GetMessage()) == 0 {
		return errors.New("empty Message")
	}

	return nil
}

// resolveRevision is a helper function to call ResolveRevision on the repo if the existing commit is not equal to the ZeroOID.
func resolveRevision(ctx context.Context, repo *localrepo.Repo, commit git.ObjectID) (git.ObjectID, error) {
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return commit, err
	}

	if commit == objectHash.ZeroOID {
		return commit, nil
	}

	return repo.ResolveRevision(
		ctx,
		git.Revision(fmt.Sprintf("%s^{object}", commit)),
	)
}
