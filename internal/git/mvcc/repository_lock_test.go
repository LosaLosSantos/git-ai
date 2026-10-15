package mvcc

import (
	"context"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// lockRepositoryForWriteAsync starts a write in the background and returns a function that waits
// for it to have taken the repository and releases it again. Waiting is what reports a failure,
// since a failed assertion cannot be reported from another goroutine: it would stop that goroutine
// and the test would block waiting for a write that never arrives.
func lockRepositoryForWriteAsync(tb testing.TB, ctx context.Context, repoPath string) func() {
	tb.Helper()

	type write struct {
		unlock func()
		err    error
	}

	// Buffered, so that the goroutine can finish even if the test gives up before waiting.
	writing := make(chan write, 1)
	go func() {
		unlock, err := LockRepositoryForWrite(ctx, repoPath)
		writing <- write{unlock: unlock, err: err}
	}()

	return func() {
		tb.Helper()

		write := <-writing
		require.NoError(tb, write.err)
		write.unlock()
	}
}

func TestRepositoryLock(t *testing.T) {
	t.Parallel()

	t.Run("garbage collection locks an idle repository", func(t *testing.T) {
		t.Parallel()

		lock := TryLockRepositoryForCollection(t.TempDir())
		require.NotNil(t, lock)
		require.False(t, lock.Evicted())
		lock.Unlock()
	})

	t.Run("garbage collection is refused while a write is in progress", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repoPath := t.TempDir()

		unlockWrite, err := LockRepositoryForWrite(ctx, repoPath)
		require.NoError(t, err)
		require.Nil(t, TryLockRepositoryForCollection(repoPath))

		unlockWrite()

		lock := TryLockRepositoryForCollection(repoPath)
		require.NotNil(t, lock)
		lock.Unlock()
	})

	t.Run("writes do not exclude each other", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repoPath := t.TempDir()

		first, err := LockRepositoryForWrite(ctx, repoPath)
		require.NoError(t, err)
		second, err := LockRepositoryForWrite(ctx, repoPath)
		require.NoError(t, err)

		first()
		require.Nil(t, TryLockRepositoryForCollection(repoPath), "the second write still holds the repository")

		second()

		lock := TryLockRepositoryForCollection(repoPath)
		require.NotNil(t, lock)
		lock.Unlock()
	})

	t.Run("an arriving write evicts garbage collection", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repoPath := t.TempDir()

		lock := TryLockRepositoryForCollection(repoPath)
		require.NotNil(t, lock)

		unlockWrite := lockRepositoryForWriteAsync(t, ctx, repoPath)

		require.Eventually(t, lock.Evicted, time.Minute, time.Millisecond,
			"a write waiting for the repository must be visible to garbage collection")

		// The write only gets through once garbage collection hands the repository back.
		lock.Unlock()
		unlockWrite()
	})

	t.Run("garbage collection is not evicted before its grace period is up", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repoPath := t.TempDir()

		lock := TryLockRepositoryForCollection(repoPath)
		require.NotNil(t, lock)

		unlockWrite := lockRepositoryForWriteAsync(t, ctx, repoPath)

		// Wait for the write to be queued. Eviction deliberately does not report it yet, so
		// the counter is what tells us it is there.
		require.Eventually(t, func() bool {
			repositoryLocks.mu.Lock()
			defer repositoryLocks.mu.Unlock()

			return repositoryLocks.locks[repoPath].writersWaiting.Load() > 0
		}, time.Minute, time.Millisecond)

		// Waiting for the write to queue may itself have taken longer than the grace period on a
		// loaded machine, so the acquisition time is pinned rather than waited out. That
		// keeps both assertions about the grace period rather than about the clock.
		lock.acquiredAt = time.Now()
		require.False(t, lock.Evicted(),
			"a run must get its grace period before it is asked to give the repository back")

		lock.acquiredAt = time.Now().Add(-evictionGracePeriod)
		require.True(t, lock.Evicted(),
			"the write must be reported once the grace period is up")

		lock.Unlock()
		unlockWrite()
	})

	t.Run("a write gives up when its context is cancelled", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repoPath := t.TempDir()

		// Collection holds the repository, and what it is in the middle of may block in
		// the kernel for seconds, so a write must not be stuck behind it.
		lock := TryLockRepositoryForCollection(repoPath)
		require.NotNil(t, lock)
		defer lock.Unlock()

		writeCtx, cancel := context.WithCancel(ctx)
		cancelled := make(chan error)
		go func() {
			_, err := LockRepositoryForWrite(writeCtx, repoPath)
			cancelled <- err
		}()

		cancel()
		require.ErrorIs(t, <-cancelled, context.Canceled)

		requireLockRegistered(t, repoPath, true, "garbage collection still holds it")
	})

	t.Run("locks are dropped once unused", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repoPath := t.TempDir()

		unlockWrite, err := LockRepositoryForWrite(ctx, repoPath)
		require.NoError(t, err)
		requireLockRegistered(t, repoPath, true)

		unlockWrite()
		requireLockRegistered(t, repoPath, false)

		lock := TryLockRepositoryForCollection(repoPath)
		require.NotNil(t, lock)
		requireLockRegistered(t, repoPath, true)

		lock.Unlock()
		requireLockRegistered(t, repoPath, false)
	})

	t.Run("a refused garbage collection does not leave a reference behind", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repoPath := t.TempDir()

		unlockWrite, err := LockRepositoryForWrite(ctx, repoPath)
		require.NoError(t, err)
		require.Nil(t, TryLockRepositoryForCollection(repoPath))
		unlockWrite()

		requireLockRegistered(t, repoPath, false)
	})

	t.Run("unlocking twice is a no-op", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repoPath := t.TempDir()

		unlockWrite, err := LockRepositoryForWrite(ctx, repoPath)
		require.NoError(t, err)
		unlockWrite()
		unlockWrite()

		lock := TryLockRepositoryForCollection(repoPath)
		require.NotNil(t, lock)
		lock.Unlock()
		lock.Unlock()

		requireLockRegistered(t, repoPath, false)
	})
}

func requireLockRegistered(tb testing.TB, repoPath string, expected bool, msgAndArgs ...any) {
	tb.Helper()

	repositoryLocks.mu.Lock()
	defer repositoryLocks.mu.Unlock()

	_, ok := repositoryLocks.locks[repoPath]
	require.Equal(tb, expected, ok, msgAndArgs...)
}
