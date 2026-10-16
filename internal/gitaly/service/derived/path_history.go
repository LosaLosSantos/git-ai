package derived

import (
	"context"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/feder-cr/git-ai/internal/graft/derived/changedpaths"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// PathHistory returns the commits that changed a path, newest first, following
// first parents, as `git log --first-parent -- path` does. The history is
// computed from the derived-data store and shared between queries.
func (s *server) PathHistory(in *gitalypb.PathHistoryRequest, stream gitalypb.DerivedDataService_PathHistoryServer) error {
	ctx := stream.Context()

	path := string(in.GetPath())
	if path == "" || path == "/" {
		// The empty path and "/" both name the repository root.
		path = "."
	}

	if err := validateRevisionAndPath(ctx, s.deps.GetLocator(), in.GetRepository(), in.GetRevision(), []byte(path)); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	limit := int(in.GetLimit())
	if limit == 0 {
		limit = defaultPathHistoryLimit
	}

	// The deriver and the changed-paths index share one object reader, so that a
	// request holds a single cat-file process.
	repo := s.deps.GetRepositoryFactory().Build(in.GetRepository())
	objectReader, cancel, err := s.deps.GetCatfileCache().ObjectReader(ctx, repo)
	if err != nil {
		return structerr.NewInternal("creating object reader: %w", err)
	}
	defer cancel()

	cache := !s.deps.GetCfg().Graft.Derived.Disabled
	deriver, err := derived.NewDeriver(ctx, s.deps.GetLogger(), repo, objectReader, cache)
	if err != nil {
		return structerr.NewInternal("creating deriver: %w", err)
	}

	commitID, err := resolveCommit(ctx, deriver, in.GetRevision())
	if err != nil {
		return err
	}

	var batch []*gitalypb.GitCommit
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := stream.Send(&gitalypb.PathHistoryResponse{Commits: batch}); err != nil {
			return err
		}
		batch = nil
		return nil
	}

	if err := s.walkPathHistory(ctx, in.GetRepository(), objectReader, deriver, commitID, path, limit, func(commit *catfile.Commit) error {
		batch = append(batch, commit.GitCommit)
		if len(batch) < pathHistoryBatchSize {
			return nil
		}
		return flush()
	}); err != nil {
		return structerr.NewInternal("%w", err)
	}

	return flush()
}

// walkPathHistory calls fn for the commits of the path history, as
// (*derived.Deriver).PathHistory does, and with the same algorithm: the
// lastmod-firstparent chain walk, whose stored values answer repeated queries in
// O(limit) reads. Unless derived-data caching is disabled, the stored changed-paths
// summaries are used as a pre-filter of that walk: a commit whose summary proves the
// path unchanged is skipped without reading the path's trees. Summaries are only read
// here, never computed: the eager warmer computes them, and a commit without one is
// compared as the Deriver compares it.
func (s *server) walkPathHistory(
	ctx context.Context,
	repoProto *gitalypb.Repository,
	objectReader catfile.ObjectContentReader,
	deriver *derived.Deriver,
	start git.ObjectID,
	path string,
	limit int,
	fn func(*catfile.Commit) error,
) error {
	if s.deps.GetCfg().Graft.Derived.Disabled {
		return deriver.PathHistory(ctx, start, path, limit, fn)
	}

	repo := s.deps.GetRepositoryFactory().Build(repoProto)
	repoPath, err := repo.Path(ctx)
	if err != nil {
		return structerr.NewInternal("getting repository path: %w", err)
	}

	index := changedpaths.NewIndexFor(objectReader, derived.NewStore(repoPath))
	return deriver.PathHistoryFiltered(ctx, start, path, limit, index.KnownUnchanged, fn)
}
