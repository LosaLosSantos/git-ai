// Package review implements the ReviewService, one of the Graft extensions
// that move Gitaly toward the design of an agent-scale, server-centric VCS.
// See doc/graft/PLAN5.md.
//
// Reviews are stored per repository by the store package. Every read
// recomputes the approval requirements from the configured rules.
package review

import (
	"context"
	"errors"

	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/dbreview"
	"github.com/feder-cr/git-ai/internal/graft/review/rules"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	// maxBodyBytes bounds the body of comments and verdicts.
	maxBodyBytes = 64 * 1024
	// defaultListLimit is the number of reviews listed when the request sets no limit.
	defaultListLimit = 100
	// listBatchSize is the number of reviews per ListReviews message.
	listBatchSize = 50
	// anonymousActor names the actor of requests without an authenticated identity.
	anonymousActor = "anonymous"
)

type server struct {
	gitalypb.UnimplementedReviewServiceServer
	deps *service.Dependencies
}

// NewServer creates a new instance of the ReviewService server.
func NewServer(deps *service.Dependencies) gitalypb.ReviewServiceServer {
	return newServer(deps)
}

func newServer(deps *service.Dependencies) *server {
	return &server{deps: deps}
}

// openRepo returns the repository and its review store. In authoritative mode
// the reviews live in the storage core's database; otherwise they are the files
// of the repository.
func (s *server) openRepo(ctx context.Context, repoProto *gitalypb.Repository) (*localrepo.Repo, store.ReviewStore, error) {
	repo := s.deps.GetRepositoryFactory().Build(repoProto)

	path, err := repo.Path(ctx)
	if err != nil {
		return nil, nil, structerr.NewInternal("repository path: %w", err)
	}

	core := s.deps.GetGraftStorage()
	if !core.Authoritative() {
		return repo, store.Open(path), nil
	}

	tracked, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repoProto))
	switch {
	case errors.Is(err, corestore.ErrRepositoryNotFound):
		return nil, nil, structerr.NewFailedPrecondition("repository is not imported into the storage core")
	case err != nil:
		return nil, nil, structerr.NewUnavailable("storage core: %w", err)
	}

	return repo, dbreview.New(ctx, core.Meta, tracked.ID), nil
}

// withRequirements sets the requirements and approval of r from the configured
// rules. r must be a copy owned by the caller.
func (s *server) withRequirements(r *gitalypb.Review) *gitalypb.Review {
	requirements, approved := rules.Evaluate(s.deps.GetCfg().Graft.Review.RulesOrDefault(), r.GetFiles(), r.GetAuthor(), r.GetVerdicts())
	r.Requirements = requirements
	r.Approved = approved
	return r
}
