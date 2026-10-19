package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
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

		cache, err := s.storageCoreCache(ctx, rec.Repo, rec.Path)
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

// storageCoreCache reports whether the local repository at path is a cache of
// the storage core that a hydration can rebuild: the database holds it with
// changesets, and it has a watermark.
func (s *Syncer) storageCoreCache(ctx context.Context, repo localrefs.RepoRef, path string) (bool, error) {
	r, err := s.core.Meta.GetRepository(ctx, corestore.RepoKey{Storage: repo.Storage, RelativePath: repo.RelativePath})
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.HeadSeq == 0 {
		return false, nil
	}

	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	_, ok, err := gitops.ReadWatermark(path)
	if err != nil {
		// A watermark that cannot be read is no proof that the storage core wrote
		// the repository: it is kept, as untracked() keeps it.
		return false, nil
	}
	return ok, nil
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

	cache, cacheErr := s.storageCoreCache(ctx, untrusted.Repo, path)
	if cacheErr != nil {
		d.CountStepError()
		logger.WithField("cache_check_error", cacheErr.Error()).
			Error("storage core: the references of a repository may have lost writes; whether it can be hydrated again is not known yet, retried at the next step")
		return
	}
	if !cache {
		logger.Error("storage core: the references of a repository that is not a cache of the storage core may have lost writes; it is kept in place")
		return
	}
	if err := s.distrustLocked(ctx, untrusted.Repo, path, localrefs.ReasonSyncFailed, untrusted); err != nil {
		logger.WithField("move_error", err.Error()).
			Error("storage core: could not move aside a repository whose references may have lost writes")
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
// (both directories, and the new directory's entry) before the marker goes, so
// a crash in between moves it again. The caller holds the repository lock.
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
	for _, dir := range []string{filepath.Dir(path), stale, tmpDir} {
		if err := syncDir(dir); err != nil {
			return structerr.NewInternal("storage core: make the move of an untrusted repository durable: %w", err)
		}
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
