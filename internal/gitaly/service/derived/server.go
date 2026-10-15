// Package derived implements the DerivedDataService, one of the Graft
// extensions that move Gitaly toward the design of an agent-scale,
// server-centric VCS. See doc/graft/PLAN.md.
package derived

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"path/filepath"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/graft/derived"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

const (
	// blameBatchSize is the number of lines per BlameResponse message.
	blameBatchSize = 1000
	// pathHistoryBatchSize is the number of commits per PathHistoryResponse message.
	pathHistoryBatchSize = 100
	// defaultPathHistoryLimit is the number of commits returned when the request sets no limit.
	defaultPathHistoryLimit = 100
)

type server struct {
	gitalypb.UnimplementedDerivedDataServiceServer
	deps *service.Dependencies
}

// NewServer creates a new instance of the DerivedDataService server.
func NewServer(deps *service.Dependencies) gitalypb.DerivedDataServiceServer {
	return &server{deps: deps}
}

// newDeriver returns a deriver for the repository. The returned function must be
// called once the deriver is no longer used.
func (s *server) newDeriver(ctx context.Context, repoProto *gitalypb.Repository) (*derived.Deriver, func(), error) {
	repo := s.deps.GetRepositoryFactory().Build(repoProto)

	objectReader, cancel, err := s.deps.GetCatfileCache().ObjectReader(ctx, repo)
	if err != nil {
		return nil, nil, structerr.NewInternal("creating object reader: %w", err)
	}

	cache := !s.deps.GetCfg().Graft.Derived.Disabled
	deriver, err := derived.NewDeriver(ctx, s.deps.GetLogger(), repo, objectReader, cache)
	if err != nil {
		cancel()
		return nil, nil, structerr.NewInternal("creating deriver: %w", err)
	}

	return deriver, cancel, nil
}

// resolveCommit resolves the revision to a commit. Unknown revisions are reported
// as NotFound.
func resolveCommit(ctx context.Context, deriver *derived.Deriver, revision []byte) (git.ObjectID, error) {
	commitID, err := deriver.ResolveCommit(ctx, git.Revision(revision))
	if err != nil {
		var notFound catfile.NotFoundError
		if errors.As(err, &notFound) {
			return "", structerr.NewNotFound("revision not found").WithMetadata("revision", string(revision))
		}
		return "", structerr.NewInternal("%w", err)
	}

	return commitID, nil
}

func validateRevisionAndPath(ctx context.Context, locator storage.Locator, repo *gitalypb.Repository, revision, path []byte) error {
	if err := locator.ValidateRepository(ctx, repo); err != nil {
		return err
	}
	if err := git.ValidateRevision(revision); err != nil {
		return err
	}

	// The derived-data queries name exactly one commit. Ranges and exclusions select
	// several commits, which is not what these RPCs answer.
	if !derived.SupportsRevision(string(revision)) {
		return structerr.NewInvalidArgument("revision must name a single commit").
			WithMetadata("revision", string(revision))
	}

	if len(path) == 0 {
		return fmt.Errorf("empty path")
	}

	// Git cannot represent a NUL byte in a path, so such a path names nothing.
	if bytes.IndexByte(path, 0) >= 0 {
		return structerr.NewInvalidArgument("path must not contain NUL bytes")
	}

	if !filepath.IsLocal(string(path)) {
		return structerr.NewInvalidArgument("path escapes repository root").
			WithMetadata("path", string(path))
	}

	// Git normalizes paths such as "a/", "a/." or "a/x/.." before matching them, and
	// these RPCs look up the literal path. Rejecting them keeps both behaviours apart.
	if pathpkg.Clean(string(path)) != string(path) {
		return structerr.NewInvalidArgument("path must not have empty, \".\" or \"..\" components or a trailing slash").
			WithMetadata("path", string(path))
	}

	return nil
}
