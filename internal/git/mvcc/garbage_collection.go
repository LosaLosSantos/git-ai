package mvcc

import (
	"context"
	"time"
)

// acquireBackoff is how long to wait between attempts at locking a repository for garbage
// collection.
const acquireBackoff = 10 * time.Millisecond

// minEntryAge is the floor under the configured minimum age of anything garbage collection removes.
// An entry that is merely unreferenced may still be being written, as artifacts and manifests are
// both fetched into a temporary file in the directory they land in.
const minEntryAge = 2 * maxArtifactFetchTimeout

// acquire locks the repository at repoPath for a garbage collection run, waiting up to wait for it.
// It reports how long it waited, and if it failed, whether the repository stayed busy or Gitaly is
// shutting down.
func acquire(ctx context.Context, repoPath string, wait time.Duration) (lock *CollectionLock, waited time.Duration, skipped, cancelled bool) {
	if ctx.Err() != nil {
		return nil, 0, false, true
	}

	start := time.Now()

	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	lock = lockForCollection(waitCtx, repoPath)
	waited = time.Since(start)

	if lock == nil {
		cancelled = ctx.Err() != nil
		skipped = !cancelled
	}

	return lock, waited, skipped, cancelled
}

// lockForCollection locks the repository for garbage collection, retrying until the context runs
// out. It never waits on the lock itself, as a queued exclusive acquisition would make arriving
// writes wait too.
func lockForCollection(ctx context.Context, repoPath string) *CollectionLock {
	ticker := time.NewTicker(acquireBackoff)
	defer ticker.Stop()

	for {
		// The attempt comes before the wait, so that a run always gets one try at the
		// lock even when it is given no time to wait for it.
		if lock := TryLockRepositoryForCollection(repoPath); lock != nil {
			return lock
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
