package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// finalStepTimeout bounds the durable steps that run when the process stops.
const finalStepTimeout = 30 * time.Second

// StartLocalReferences starts the deferred durability of the node's local
// references (doc/graft/PLAN7.md, LOCAL REFERENCE WRITES RULE). gitaly serve
// calls it before its listeners open.
//
// When the process takes the ownership of the storages' deferred durability,
// it recovers them before returning (RecoverLocalReferences): a repository
// that had writes not yet durable when the kernel restarted is moved aside, so
// nothing reads its possibly torn stack. When another process owns them (a
// graceful upgrade overlaps the old and the new process), that process
// recovered them when it started, in this boot; this one writes references
// with fsync and tries again every DurableStepInterval, recovering when it
// takes the ownership.
//
// The returned stop ends the durable steps and runs a last one for every dirty
// repository, without waiting for it to be idle, so that a clean stop leaves
// no marker of a repository whose writes ended.
func (s *Syncer) StartLocalReferences(ctx context.Context, storages []string) (stop func(), err error) {
	d := s.core.RefDurability
	if !d.Enabled() {
		return func() {}, nil
	}

	owned, err := d.TryOwn(ctx, storages)
	if err != nil {
		return nil, fmt.Errorf("storage core: own the deferred durability: %w", err)
	}
	recovered := false
	if owned {
		if err := s.RecoverLocalReferences(ctx, storages); err != nil {
			return nil, err
		}
		d.Activate()
		recovered = true
	} else {
		s.logger.Warn("storage core: another gitaly process owns the deferred durability of the storages; references are written with fsync until it exits")
	}

	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runDurableSteps(loopCtx, storages, recovered)
	}()

	return func() {
		cancel()
		wg.Wait()
		if !d.Deferring() {
			return
		}
		finalCtx, cancelFinal := context.WithTimeout(context.WithoutCancel(ctx), finalStepTimeout)
		defer cancelFinal()
		for _, path := range d.Dirty() {
			s.durableStep(finalCtx, path, true)
		}
	}, nil
}

// runDurableSteps runs, every DurableStepInterval until ctx ends, the durable
// step of every dirty repository, or, while another process owns the deferred
// durability, an attempt to take it (and the recovery once taken).
func (s *Syncer) runDurableSteps(ctx context.Context, storages []string, recovered bool) {
	d := s.core.RefDurability
	ticker := time.NewTicker(localrefs.DurableStepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if !recovered {
			owned, err := d.TryOwn(ctx, storages)
			if err != nil {
				s.logger.WithError(err).Error("storage core: own the deferred durability")
				continue
			}
			if !owned {
				continue
			}
			if err := s.RecoverLocalReferences(ctx, storages); err != nil {
				s.logger.WithError(err).Error("storage core: recover the local references after taking the deferred durability; retried")
				continue
			}
			d.Activate()
			recovered = true
			s.logger.Info("storage core: this process owns the deferred durability of the storages now")
		}

		for _, path := range d.Dirty() {
			s.durableStep(ctx, path, false)
		}
	}
}

// RecoverLocalReferences moves aside every local repository that had reference
// writes that were not durable yet when the kernel restarted (doc/graft/PLAN7.md,
// LOCAL REFERENCE WRITES RULE): its reftable stack may be torn, and git reads a
// torn stack as an empty repository without an error. The next request
// hydrates it from the database and the bucket. The process owns the deferred
// durability. A repository dirty from a process restart of the same boot is
// kept: the kernel kept every write, and the durable step settles it.
//
// A repository whose marker says that a durable step of the previous process
// found a failed fsync is moved aside the same way: its writes may be lost,
// although the kernel would report nothing more.
//
// Only a cache of the storage core is moved (storageCoreCache). Any other
// marker of another boot (the repository was removed, or replaced by one being
// imported, whose local copy may be the only one) is dropped; an untrusted one
// is kept, with its repository untrusted, and logged.
func (s *Syncer) RecoverLocalReferences(ctx context.Context, storages []string) error {
	d := s.core.RefDurability
	found, err := d.Recover(ctx, storages)
	if err != nil {
		return fmt.Errorf("storage core: recover the local references: %w", err)
	}

	for _, rec := range found {
		if !rec.Reboot && !rec.Untrusted {
			continue
		}
		reason := localrefs.ReasonReboot
		if !rec.Reboot {
			reason = localrefs.ReasonSyncFailed
		}

		cache, keep, err := s.storageCoreCache(ctx, rec.Repo, rec.Path)
		if err != nil {
			return fmt.Errorf("storage core: recover the local references of %s:%s: %w", rec.Repo.Storage, rec.Repo.RelativePath, err)
		}
		if !cache {
			if rec.Reboot {
				s.logger.WithField("repository_path", rec.Path).
					Warn("storage core: dropped the dirty marker of a local repository that is not a cache of the storage core")
				if err := d.Forget(ctx, rec.Repo, rec.Path); err != nil {
					return err
				}
			} else {
				s.logger.WithField("repository_path", rec.Path).
					Error("storage core: the references of a repository that is not a cache of the storage core may have lost writes; it is kept in place, untrusted")
			}
			continue
		}

		if err := s.distrust(ctx, rec.Repo, rec.Path, reason, nil, keep); err != nil {
			return err
		}
	}
	return nil
}

// storageCoreCache reports whether the local repository at path is a cache of
// the storage core that a hydration can rebuild: the database holds it with
// changesets (an import that is not complete has none, and its local copy may
// be the only one). keep reports that the copy holds history written in shadow
// mode and not imported yet (checkReplaceable, the guard of a sync's
// hydration): the copy is moved aside all the same, so that it is never served,
// but kept, so that the history is not lost. An error (the database, or the
// repository's files, cannot be read) is returned: the caller tries again.
func (s *Syncer) storageCoreCache(ctx context.Context, repo localrefs.RepoRef, path string) (cache, keep bool, err error) {
	r, err := s.core.Meta.GetRepository(ctx, corestore.RepoKey{Storage: repo.Storage, RelativePath: repo.RelativePath})
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if r.HeadSeq == 0 {
		return false, false, nil
	}

	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	} else if err != nil {
		return false, false, err
	}

	wm, ok := s.readWatermark(path)
	if err := s.checkReplaceable(ctx, r, path, wm, ok); err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			return true, true, nil
		}
		return false, false, err
	}
	return true, false, nil
}

// durableStep makes the references of the repository at path durable under the
// repository lock (final: without waiting for the repository to be idle). When
// the kernel reports that a write failed, the repository is moved aside at
// once, if it is a cache of the storage core; any other failure is retried at
// the next step.
func (s *Syncer) durableStep(ctx context.Context, path string, final bool) {
	release, err := s.lock(ctx, path)
	if err != nil {
		return
	}
	defer release()

	d := s.core.RefDurability
	err = d.Settle(path, final)
	if err == nil {
		return
	}

	logger := s.logger.WithError(err).WithField("repository_path", path)
	var untrusted *localrefs.UntrustedError
	if !errors.As(err, &untrusted) {
		d.CountStepError()
		logger.Warn("storage core: durable step of the local references failed, retried at the next step")
		return
	}

	// A repository that is gone (moved aside as a stale cache meanwhile) has
	// nothing left to serve: its marker is dropped.
	if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
		if err := s.distrustLocked(ctx, untrusted.Repo, path, localrefs.ReasonSyncFailed, untrusted, false); err != nil {
			logger.WithField("forget_error", err.Error()).Error("storage core: could not drop the marker of a repository that is gone; retried at the next step")
		}
		return
	}

	cache, keep, cacheErr := s.storageCoreCache(ctx, untrusted.Repo, path)
	if cacheErr != nil {
		d.CountStepError()
		logger.WithField("cache_check_error", cacheErr.Error()).
			Error("storage core: the references of a repository may have lost writes; whether it can be hydrated again is not known yet, retried at the next step")
		return
	}
	if !cache {
		// Its marker stays (the untrusted state is sticky): after a reboot the
		// recovery decides again.
		logger.Error("storage core: the references of a repository that is not a cache of the storage core may have lost writes; it is kept in place")
		return
	}
	if err := s.distrustLocked(ctx, untrusted.Repo, path, localrefs.ReasonSyncFailed, untrusted, keep); err != nil {
		// The untrusted state is sticky: the next step moves it aside again, and
		// the marker is never cleared meanwhile.
		logger.WithField("move_error", err.Error()).
			Error("storage core: could not move aside a repository whose references may have lost writes; retried at the next step")
	}
}

// distrust is distrustLocked under the repository lock.
func (s *Syncer) distrust(ctx context.Context, repo localrefs.RepoRef, path, reason string, cause error, keep bool) error {
	release, err := s.lock(ctx, path)
	if err != nil {
		return err
	}
	defer release()
	return s.distrustLocked(ctx, repo, path, reason, cause, keep)
}

// KeptDirName is the directory of a storage where the untrusted repositories
// that hold history not imported yet are moved: they are never served, and an
// operator imports what they hold or removes them.
const KeptDirName = "+gitaly/graft-untrusted"

// distrustLocked moves the local repository at path out of the way, then drops
// its dirty marker: the next request hydrates it. It goes into the temporary
// directory of its storage and is removed there in the background, or, with
// keep, into KeptDirName, where it stays. The move is made durable (both
// directories, and the new directory's entry) before the marker goes, so a
// crash in between moves it again. The caller holds the repository lock.
func (s *Syncer) distrustLocked(ctx context.Context, repo localrefs.RepoRef, path, reason string, cause error, keep bool) error {
	d := s.core.RefDurability

	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return d.Forget(ctx, repo, path)
	} else if err != nil {
		return structerr.NewInternal("storage core: stat repository: %w", err)
	}

	parent, err := s.tmpDir(repo.Storage)
	if err != nil {
		return err
	}
	if keep {
		root, err := s.locator.GetStorageByName(ctx, repo.Storage)
		if err != nil {
			return err
		}
		parent = filepath.Join(root, KeptDirName)
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return structerr.NewInternal("storage core: create the directory of kept repositories: %w", err)
		}
	}
	aside, err := os.MkdirTemp(parent, tmpStalePattern)
	if err != nil {
		return structerr.NewInternal("storage core: create stale directory: %w", err)
	}
	if err := os.Rename(path, filepath.Join(aside, "repo.git")); err != nil {
		_ = os.Remove(aside)
		return structerr.NewInternal("storage core: move aside an untrusted repository: %w", err)
	}
	for _, dir := range []string{filepath.Dir(path), aside, parent, filepath.Dir(parent)} {
		if err := syncDir(dir); err != nil {
			return structerr.NewInternal("storage core: make the move of an untrusted repository durable: %w", err)
		}
	}

	s.forgetPending(path)
	if err := d.Forget(ctx, repo, path); err != nil {
		return err
	}
	d.CountUntrusted(reason)

	logger := s.logger.WithField("repository_path", path).WithField("moved_path", aside).WithField("reason", reason)
	if cause != nil {
		logger = logger.WithError(cause)
	}
	if keep {
		logger.Error("storage core: moved aside a local repository whose references may have lost writes; it holds shadow-mode history not imported yet and is kept for an operator; the next request hydrates the repository")
		return nil
	}
	logger.Error("storage core: moved aside a local repository whose references may have lost writes, the next request hydrates it")

	go func() { _ = os.RemoveAll(aside) }()
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
