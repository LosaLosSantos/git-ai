package commit

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/git/log"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/graft/derived"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

func (s *server) LastCommitForPath(ctx context.Context, in *gitalypb.LastCommitForPathRequest) (*gitalypb.LastCommitForPathResponse, error) {
	if err := validateLastCommitForPathRequest(ctx, s.locator, in); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	resp, err := s.lastCommitForPath(ctx, in)
	if err != nil {
		return nil, structerr.NewInternal("%w", err)
	}

	return resp, nil
}

func (s *server) lastCommitForPath(ctx context.Context, in *gitalypb.LastCommitForPathRequest) (*gitalypb.LastCommitForPathResponse, error) {
	path := string(in.GetPath())
	if len(path) == 0 || path == "/" {
		path = "."
	}

	repo := s.localRepoFactory.Build(in.GetRepository())
	objectReader, cancel, err := s.catfileCache.ObjectReader(ctx, repo)
	if err != nil {
		return nil, err
	}
	defer cancel()

	options := in.GetGlobalOptions()

	// Preserve backwards compatibility with legacy LiteralPathspec
	// flag: These protobuf changes were not shipped in 13.1, so can be
	// remove before 13.2.
	if in.GetLiteralPathspec() {
		if options == nil {
			options = &gitalypb.GlobalOptions{}
		}

		options.LiteralPathspecs = true
	}

	if !s.cfg.Graft.Derived.Disabled && derived.SupportsPath(path) && derived.SupportsRevision(string(in.GetRevision())) {
		// Derived data answers the query from the cached last-modified commits. Pathspec
		// magic, trailing slashes, and revisions that do not name exactly one commit are
		// not supported there and keep using git log below. Any failure of the derived
		// data also falls back to git log, which is always correct.
		commit, found, err := s.lastCommitForPathFromDerived(ctx, repo, objectReader, in.GetRevision(), path)
		switch {
		case errors.Is(err, errUseGit):
			// Not a failure: git log below decides how to answer for such queries.
		case err != nil:
			s.logger.WithError(err).Warn("derived: last commit for path failed, using git log")
		case !found:
			return &gitalypb.LastCommitForPathResponse{}, nil
		default:
			return &gitalypb.LastCommitForPathResponse{Commit: commit.GitCommit}, nil
		}
	}

	commit, err := log.LastCommitForPath(ctx, objectReader, repo, git.Revision(in.GetRevision()), path, options)
	if err != nil {
		if errors.As(err, &catfile.NotFoundError{}) {
			return &gitalypb.LastCommitForPathResponse{}, nil
		}
		return nil, err
	}

	return &gitalypb.LastCommitForPathResponse{Commit: commit.GitCommit}, err
}

// errUseGit is returned by lastCommitForPathFromDerived when the derived data
// cannot answer the query, so that git log has to decide. This is the case for
// revisions that do not resolve to a commit, which is common client traffic and not
// a failure of the derived data, for shallow repositories, and for paths that do not
// exist at the revision: clients can send any path name, and answering those from
// derived data would walk the whole history one commit at a time, so git log answers
// them, as it does upstream. Histories whose objects are missing are reported as
// other errors, which also make git log answer.
var errUseGit = errors.New("derived data does not apply, using git log")

// lastCommitForPathFromDerived returns the commit that last changed path as of
// revision, read from the derived-data store. The boolean is false when path
// does not exist in the history of the revision. It returns errUseGit when the
// answer must come from git log instead.
func (s *server) lastCommitForPathFromDerived(
	ctx context.Context,
	repo *localrepo.Repo,
	objectReader catfile.ObjectContentReader,
	revision []byte,
	path string,
) (*catfile.Commit, bool, error) {
	deriver, err := derived.NewDeriver(ctx, s.logger, repo, objectReader, true)
	if err != nil {
		return nil, false, err
	}

	shallow, err := deriver.Shallow(ctx)
	if err != nil {
		return nil, false, err
	}
	if shallow {
		return nil, false, errUseGit
	}

	commitID, err := deriver.ResolveCommit(ctx, git.Revision(revision))
	if err != nil {
		if errors.As(err, &catfile.NotFoundError{}) {
			return nil, false, errUseGit
		}
		return nil, false, err
	}

	exists, err := deriver.PathExists(ctx, commitID, path)
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, errUseGit
	}

	// LastMod is used rather than LastCommitOf so that an absent path, reported as an
	// empty value, is told apart from a missing object. A missing object is an error
	// here, which makes git log answer instead of an empty response.
	lastMod, err := deriver.LastMod(ctx, commitID, path)
	if err != nil {
		return nil, false, err
	}
	if lastMod == "" {
		return nil, false, nil
	}

	commit, err := catfile.GetCommit(ctx, objectReader, lastMod.Revision())
	if err != nil {
		return nil, false, err
	}

	return commit, true, nil
}

func validateLastCommitForPathRequest(ctx context.Context, locator storage.Locator, in *gitalypb.LastCommitForPathRequest) error {
	if err := locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		return err
	}
	if err := git.ValidateRevision(in.GetRevision()); err != nil {
		return err
	}

	path := string(in.GetPath())
	switch {
	case path == "", path == "/":
		// We map both the empty path and "/" to instead refer to the root directory, so these are fine.
	case filepath.IsAbs(path):
		// Strictly speaking this is already handled by `filepath.IsLocal()`, but handling this case explicitly
		// allows us to generate a better error message.
		return structerr.NewInvalidArgument("path is an absolute path").WithMetadata("path", path)
	case !filepath.IsLocal(path):
		return structerr.NewInvalidArgument("path escapes repository").WithMetadata("path", path)
	}

	return nil
}
