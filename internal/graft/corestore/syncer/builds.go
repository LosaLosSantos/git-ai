package syncer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// buildLockName is the file in a build directory on which the process that
// runs the build holds an exclusive flock until it removed the directory. The
// kernel releases it when the process dies, so a process cleaning up tells the
// builds of a process that died from those of one that runs (the old process
// of a graceful upgrade).
const buildLockName = "build.lock"

// lockBuild creates the lock file of the build directory dir and takes its
// flock. The file locked must still be the one at the path: CleanBuilds may
// have found the lock file free before the flock, as for a process that died,
// and removed the directory; the build then fails. afterOpen, when not nil (a
// test), is called between the open and the flock.
func lockBuild(dir string, afterOpen func(path string)) (*os.File, error) {
	path := filepath.Join(dir, buildLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if afterOpen != nil {
		afterOpen(path)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock: %w", err)
	}
	locked, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock: %w", err)
	}
	current, err := os.Stat(path)
	if err != nil || !os.SameFile(locked, current) {
		_ = f.Close()
		return nil, fmt.Errorf("lock: the build directory was removed before its lock was taken (%v)", err)
	}
	return f, nil
}

// unlockedBuildAge is how old a build directory without a lock file must be
// before CleanBuilds removes it: one whose process died between creating the
// directory and its lock file, or one of a version before the lock files
// (v0.2), which may still run during a graceful upgrade. No hydration or seed
// lives longer than the larger of a seed's deadline W and sync_timeout, so an
// older directory has no process.
func (s *Syncer) unlockedBuildAge() time.Duration {
	return max(s.core.WriteDeadline(), s.syncTimeout())
}

// CleanBuilds removes the build directories (hydrate-*) of hydrations and
// seeds that no process runs any more: their process died (a crash, a kill)
// before it removed them, and nothing else does. A directory whose lock file
// another process holds is in use and kept. A directory gone meanwhile was
// removed by its process. A directory that cannot be checked or removed is
// kept, logged and counted (graft_storage_build_cleanup_failures_total): the
// cleanup never stops the server. gitaly serve calls it at startup, before
// the listeners open (final minor 3).
func (s *Syncer) CleanBuilds(ctx context.Context, storages []string) {
	removed := 0
	fail := func(dir string, err error) {
		s.buildCleanupFailures.Inc()
		s.logger.WithError(err).WithField("directory", dir).WarnContext(ctx, "storage core: a build directory could not be cleaned up; it is kept")
	}
	for _, storage := range storages {
		tmpDir, err := s.locator.TempDir(storage)
		if err != nil {
			fail(storage, err)
			continue
		}
		entries, err := os.ReadDir(tmpDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			fail(tmpDir, err)
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), strings.TrimSuffix(tmpHydratePatt, "*")) {
				continue
			}
			dir := filepath.Join(tmpDir, entry.Name())
			ok, err := s.cleanBuild(dir)
			if err != nil {
				fail(dir, err)
				continue
			}
			if ok {
				removed++
			}
		}
	}
	if removed > 0 {
		s.logger.WithField("directories", removed).InfoContext(ctx, "storage core: removed the build directories of hydrations that no process runs")
	}
}

// cleanBuild removes the build directory dir if no process runs its build,
// and reports whether it did. A directory that is gone is not an error.
func (s *Syncer) cleanBuild(dir string) (bool, error) {
	stale, err := s.staleBuild(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil || !stale {
		return false, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	return true, nil
}

// staleBuild reports whether no process runs the build in dir: its lock file
// can be locked, or it has none and dir is older than unlockedBuildAge. A
// lock taken here is released when the directory is gone or kept.
func (s *Syncer) staleBuild(dir string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(dir, buildLockName), os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		info, err := os.Stat(dir)
		if err != nil {
			return false, err
		}
		return time.Since(info.ModTime()) >= s.unlockedBuildAge(), nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// StopSeeds cancels the running seed jobs and waits for them to end; no seed
// starts afterwards. gitaly serve calls it after its servers stopped, before
// the last durable step of the local references (final minor 3).
func (s *Syncer) StopSeeds() {
	s.mu.Lock()
	s.seedsStopped = true
	s.mu.Unlock()
	s.stopSeeds()
	s.seedsRunning.Wait()
}
