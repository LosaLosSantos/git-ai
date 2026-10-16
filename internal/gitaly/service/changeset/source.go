package changeset

import (
	"context"
	"errors"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/dblog"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// changelogSource returns where the changelog of repo is read from: the
// database in authoritative mode of the storage core, the file log of the
// repository otherwise.
func (s *server) changelogSource(ctx context.Context, repo *gitalypb.Repository) (changelog.Source, error) {
	repoPath, err := s.repoPath(ctx, repo)
	if err != nil {
		return nil, err
	}

	core := s.deps.GetGraftStorage()
	if !core.Authoritative() {
		return changelog.FileSource(repoPath), nil
	}

	r, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return nil, structerr.NewFailedPrecondition("repository is not imported into the storage core")
	}
	if err != nil {
		return nil, structerr.NewUnavailable("storage core: %w", err)
	}

	return dblog.New(core.Meta, r.ID), nil
}
