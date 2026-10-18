package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// RecoverLocalReferences moves aside every local repository that had reference
// writes that were not durable yet when the kernel restarted (doc/graft/PLAN7.md,
// LOCAL REFERENCE WRITES RULE): its reftable stack may be torn, and git reads a
// torn stack as an empty repository without an error. The next request
// hydrates it from the database and the bucket. gitaly serve calls it before
// its listeners open, so nothing reads such a repository before. A repository
// dirty from a process restart of the same boot is kept: the kernel kept every
// write, and the durable step settles it.
//
// Only a cache of the storage core is moved: the database must hold the
// repository with changesets, and the local repository must have its
// watermark. Any other marker (the repository was removed, or replaced by one
// being imported, whose local copy may be the only one) is dropped.
func (s *Syncer) RecoverLocalReferences(ctx context.Context, storages []string) error {
	d := s.core.RefDurability
	found, err := d.Recover(ctx, storages)
	if err != nil {
		return fmt.Errorf("storage core: recover the local references: %w", err)
	}

	for _, rec := range found {
		if !rec.Reboot {
			continue
		}

		cache, err := s.storageCoreCache(ctx, rec)
		if err != nil {
			return fmt.Errorf("storage core: recover the local references of %s:%s: %w", rec.Repo.Storage, rec.Repo.RelativePath, err)
		}
		if !cache {
			s.logger.WithField("repository_path", rec.Path).
				Warn("storage core: dropped the dirty marker of a local repository that is not a cache of the storage core")
			if err := d.Forget(ctx, rec.Repo, rec.Path); err != nil {
				return err
			}
			continue
		}

		if err := s.distrust(ctx, rec.Repo, rec.Path, localrefs.ReasonReboot, nil); err != nil {
			return err
		}
	}
	return nil
}

// storageCoreCache reports whether the local repository of rec is a cache of
// the storage core that a hydration can rebuild: the database holds it with
// changesets, and it has a watermark.
func (s *Syncer) storageCoreCache(ctx context.Context, rec localrefs.Recovered) (bool, error) {
	r, err := s.core.Meta.GetRepository(ctx, corestore.RepoKey{Storage: rec.Repo.Storage, RelativePath: rec.Repo.RelativePath})
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.HeadSeq == 0 {
		return false, nil
	}

	if _, err := os.Lstat(rec.Path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	_, ok, err := gitops.ReadWatermark(rec.Path)
	if err != nil {
		// A watermark that cannot be read is no proof that the storage core wrote
		// the repository: it is kept, as untracked() keeps it.
		return false, nil
	}
	return ok, nil
}

// RunDurableSteps runs the durable step of every dirty repository each interval
// until ctx ends (doc/graft/PLAN7.md, LOCAL REFERENCE WRITES RULE).
func (s *Syncer) RunDurableSteps(ctx context.Context, interval time.Duration) {
	if !s.core.RefDurability.Enabled() {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, path := range s.core.RefDurability.Dirty() {
			s.durableStep(ctx, path)
		}
	}
}

// durableStep makes the references of the repository at path durable under the
// repository lock, and moves the repository aside at once when the kernel
// reports that a write failed.
func (s *Syncer) durableStep(ctx context.Context, path string) {
	release, err := s.lock(ctx, path)
	if err != nil {
		return
	}
	defer release()

	err = s.core.RefDurability.Settle(path)
	var untrusted *localrefs.UntrustedError
	if errors.As(err, &untrusted) {
		if err := s.distrustLocked(ctx, untrusted.Repo, path, localrefs.ReasonSyncFailed, untrusted); err != nil {
			s.logger.WithError(err).WithField("repository_path", path).
				Error("storage core: could not move aside a repository whose references may have lost writes")
		}
		return
	}
	if err != nil {
		s.logger.WithError(err).WithField("repository_path", path).Error("storage core: durable step of the local references")
	}
}

// distrust is distrustLocked under the repository lock.
func (s *Syncer) distrust(ctx context.Context, repo localrefs.RepoRef, path, reason string, cause error) error {
	release, err := s.lock(ctx, path)
	if err != nil {
		return err
	}
	defer release()
	return s.distrustLocked(ctx, repo, path, reason, cause)
}

// distrustLocked moves the local repository at path into the temporary
// directory of its storage and removes it there in the background, then drops
// its dirty marker: the next request hydrates it. The move is made durable
// before the marker goes, so a crash in between moves it again. The caller
// holds the repository lock.
func (s *Syncer) distrustLocked(ctx context.Context, repo localrefs.RepoRef, path, reason string, cause error) error {
	d := s.core.RefDurability

	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return d.Forget(ctx, repo, path)
	} else if err != nil {
		return structerr.NewInternal("storage core: stat repository: %w", err)
	}

	tmpDir, err := s.tmpDir(repo.Storage)
	if err != nil {
		return err
	}
	stale, err := os.MkdirTemp(tmpDir, tmpStalePattern)
	if err != nil {
		return structerr.NewInternal("storage core: create stale directory: %w", err)
	}
	if err := os.Rename(path, filepath.Join(stale, "repo.git")); err != nil {
		_ = os.Remove(stale)
		return structerr.NewInternal("storage core: move aside an untrusted repository: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return structerr.NewInternal("storage core: make the move of an untrusted repository durable: %w", err)
	}

	s.forgetPending(path)
	if err := d.Forget(ctx, repo, path); err != nil {
		return err
	}
	d.CountUntrusted(reason)

	logger := s.logger.WithField("repository_path", path).WithField("stale_path", stale).WithField("reason", reason)
	if cause != nil {
		logger = logger.WithError(cause)
	}
	logger.Error("storage core: moved aside a local repository whose references may have lost writes, the next request hydrates it")

	go func() { _ = os.RemoveAll(stale) }()
	return nil
}

// syncDir makes the entries of the directory at path durable.
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
