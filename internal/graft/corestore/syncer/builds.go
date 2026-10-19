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

// unlockedBuildAge is how old a build directory without its lock file must be
// before CleanBuilds removes it: one whose process died between creating the
// directory and its lock file, which takes microseconds.
const unlockedBuildAge = time.Minute

// lockBuild creates the lock file of the build directory dir and takes its
// flock.
func lockBuild(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, buildLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock: %w", err)
	}
	return f, nil
}

// CleanBuilds removes the build directories (hydrate-*) of hydrations and
// seeds that no process runs any more: their process died (a crash, a kill)
// before it removed them, and nothing else does. A directory whose lock file
// another process holds is in use and kept. gitaly serve calls it at startup,
// before the listeners open (final minor 3).
func (s *Syncer) CleanBuilds(ctx context.Context, storages []string) error {
	removed := 0
	for _, storage := range storages {
		tmpDir, err := s.locator.TempDir(storage)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(tmpDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("storage core: list the temporary directory of %s: %w", storage, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), strings.TrimSuffix(tmpHydratePatt, "*")) {
				continue
			}
			dir := filepath.Join(tmpDir, entry.Name())
			stale, err := staleBuild(dir)
			if err != nil {
				return fmt.Errorf("storage core: check the build directory %s: %w", dir, err)
			}
			if !stale {
				continue
			}
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("storage core: remove the build directory %s: %w", dir, err)
			}
			removed++
		}
	}
	if removed > 0 {
		s.logger.WithField("directories", removed).InfoContext(ctx, "storage core: removed the build directories of hydrations that no process runs")
	}
	return nil
}

// staleBuild reports whether no process runs the build in dir: its lock file
// can be locked, or it has none and dir is older than unlockedBuildAge. A
// lock taken here is released when the directory is gone or kept.
func staleBuild(dir string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(dir, buildLockName), os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		info, err := os.Stat(dir)
		if err != nil {
			return false, err
		}
		return time.Since(info.ModTime()) >= unlockedBuildAge, nil
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
