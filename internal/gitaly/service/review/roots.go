package review

import (
	"context"
	"errors"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// maxRootsAttempts bounds the compare-and-swap of REVIEW ROOTS R1: a conflict
// means a checkpoint was recorded between the check and the write, and
// checkpoints are minutes apart at the least.
const maxRootsAttempts = 3

// rootsStore is the review store of authoritative mode (dbreview.Store), where
// a review's commits are roots of the storage core's object graph.
type rootsStore interface {
	// Refs returns the references in the storage core and the sequence of the
	// snapshot.
	Refs() (map[string]corestore.Target, uint64, error)
	// WithRootsAt returns the store whose writes require that no checkpoint
	// newer than rootsAt was recorded.
	WithRootsAt(rootsAt uint64) store.ReviewStore
}

// rootsCheck checks that commits a write is about to record as a review's
// head or base are reachable from the references of the snapshot the write
// carries.
type rootsCheck func(commits ...git.ObjectID) error

// writeRoots runs write, a review write that may record new head or base
// commits, under REVIEW ROOTS R1 (doc/graft/PLAN7.md). write calls check for
// the commits it records before it records them. In authoritative mode check
// verifies them against the references of a snapshot (refs, S) of the storage
// core, and write's store refuses the write with ErrReviewRootsStale when a
// checkpoint after S was recorded in between: then a new snapshot is taken and
// write runs again, at most maxRootsAttempts times in all, after which the
// request is Aborted. With the files store (the storage core is off or in
// shadow mode) the commits are not roots of the storage core and check
// accepts them.
func (s *server) writeRoots(ctx context.Context, repoProto *gitalypb.Repository, repo *localrepo.Repo, st store.ReviewStore, write func(store.ReviewStore, rootsCheck) error) error {
	rs, ok := st.(rootsStore)
	if !ok {
		return write(st, func(...git.ObjectID) error { return nil })
	}

	for attempt := 0; attempt < maxRootsAttempts; attempt++ {
		refs, seq, err := rs.Refs()
		switch {
		case errors.Is(err, corestore.ErrRepositoryNotFound):
			return structerr.NewFailedPrecondition("repository is not imported into the storage core")
		case err != nil:
			return structerr.NewUnavailable("storage core: %w", err)
		}

		snapshot := &rootsSnapshot{ctx: ctx, fresh: s.deps.GetGraftStorageSyncer(), repoProto: repoProto, repo: repo, refs: refs}
		err = write(rs.WithRootsAt(seq), snapshot.check)
		if !errors.Is(err, corestore.ErrReviewRootsStale) {
			return err
		}
	}

	return structerr.NewAborted("the commits of the review could not be checked against a stable snapshot: checkpoints were recorded concurrently")
}

// rootsSnapshot checks commits against the references of one snapshot, in the
// local repository.
type rootsSnapshot struct {
	ctx       context.Context
	fresh     corestore.Freshener
	repoProto *gitalypb.Repository
	repo      *localrepo.Repo
	refs      map[string]corestore.Target

	// tips are the object IDs the references point to, set once the local
	// repository is known to hold them (a write may check more than once: the
	// review store calls the write's function again on a version conflict).
	tips []git.ObjectID
}

func (r *rootsSnapshot) check(commits ...git.ObjectID) error {
	if r.tips == nil {
		tips, err := r.localTips()
		if err != nil {
			return err
		}
		r.tips = tips
	}

	for _, commit := range commits {
		reachable, err := gitops.IsReachable(r.ctx, r.repo, commit, r.tips)
		if err != nil {
			return structerr.NewInternal("check the review commit %s: %w", commit, err)
		}
		if !reachable {
			return structerr.NewFailedPrecondition("commit %s of the review is not reachable from the references of the repository in the storage core", commit)
		}
	}
	return nil
}

// localTips returns the object IDs the snapshot's references point to, after
// making sure the local repository holds them. The gate caught the repository
// up before the request, but a changeset committed since (on another node) can
// be in the snapshot: the repository is then caught up again, to a head read
// after this call, so at least the snapshot's.
func (r *rootsSnapshot) localTips() ([]git.ObjectID, error) {
	tips := make([]git.ObjectID, 0, len(r.refs))
	for _, target := range r.refs {
		if !target.IsSymbolic() {
			tips = append(tips, git.ObjectID(target))
		}
	}

	missing, err := gitops.MissingObjects(r.ctx, r.repo, tips)
	if err != nil {
		return nil, structerr.NewInternal("look up the references' objects: %w", err)
	}
	if len(missing) > 0 {
		if err := r.fresh.EnsureFresh(r.ctx, r.repoProto); err != nil {
			return nil, err
		}
	}
	return tips, nil
}
