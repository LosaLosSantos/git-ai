package review

import (
	"context"
	"errors"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/reviewroots"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// maxRootsAttempts bounds the review writes that lose a keep-around reference
// to a concurrent sweep between writing it and recording the review.
const maxRootsAttempts = 3

// requiredRootsStore is the review store of authoritative mode
// (dbreview.Store), which can require a review's roots in the database.
type requiredRootsStore interface {
	WithRequiredRoots() store.ReviewStore
}

// rootsProtect writes the keep-around references of commits that a review
// write is about to record.
type rootsProtect func(commits ...git.ObjectID) error

// writeRoots runs write, a review write that may record new head or base
// commits, under the REVIEW ROOTS RULE (doc/graft/PLAN7.md). write calls
// protect with the commits it records before it records them, which writes
// their keep-around references through the reference-transaction hook, in
// every mode: local housekeeping keeps what references reach, and in shadow and
// authoritative mode the voter records them and uploads the objects the
// storage core does not have. In authoritative mode the store of write requires
// the references in the database; a sweep that deleted one between the two
// steps makes it fail with ErrReviewRootsMissing, and write runs again (which
// writes the reference again), at most maxRootsAttempts times in all, after
// which the request is Aborted.
func (s *server) writeRoots(ctx context.Context, repo *localrepo.Repo, st store.ReviewStore, write func(store.ReviewStore, rootsProtect) error) error {
	protect := func(commits ...git.ObjectID) error {
		if err := reviewroots.Protect(ctx, repo, commits); err != nil {
			return structerr.NewInternal("protect the commits of the review: %w", err)
		}
		if s.rootsProtected != nil {
			s.rootsProtected(ctx, repo, st)
		}
		return nil
	}

	rs, ok := st.(requiredRootsStore)
	if !ok {
		return write(st, protect)
	}

	required := rs.WithRequiredRoots()
	for attempt := 0; attempt < maxRootsAttempts; attempt++ {
		err := write(required, protect)
		if !errors.Is(err, corestore.ErrReviewRootsMissing) {
			return err
		}
	}
	return structerr.NewAborted("the keep-around references of the review's commits were deleted concurrently %d times", maxRootsAttempts)
}

// releaseRoots deletes the keep-around references of the commits of a review
// that was just closed, unless another open review uses them. The close is
// already recorded: a reference that cannot be deleted now only keeps its
// objects alive longer, until the next close of a review of the same commit or
// the maintenance sweep of the storage core.
func (s *server) releaseRoots(ctx context.Context, repo *localrepo.Repo, st store.ReviewStore, commits []git.ObjectID) {
	if _, err := reviewroots.Sweep(ctx, repo, st, commits); err != nil {
		s.deps.GetLogger().WithError(err).WithField("commits", commits).WarnContext(ctx,
			"review: the keep-around references of a closed review were not deleted; they keep their objects until a later sweep")
	}
}
