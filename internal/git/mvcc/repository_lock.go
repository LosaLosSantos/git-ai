package mvcc

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// evictionGracePeriod is how long a garbage collection run may hold the repository before it starts
// giving way to writes waiting for it.
const evictionGracePeriod = 25 * time.Millisecond

// repositoryLock keeps garbage collection from running while a repository is being written to.
type repositoryLock struct {
	// sem is held with weight one by each write and in full by garbage collection, in order to
	// make the two mutually exclusive. A semaphore rather than a sync.RWMutex because a
	// semaphore respects context cancellation.
	sem *semaphore.Weighted
	// writersWaiting counts the writes blocked on sem. A semaphore does not expose whether
	// anyone is waiting for it, and garbage collection needs to know so that it can hand the
	// repository back rather than make a write wait for it to finish.
	writersWaiting atomic.Int64
	// commitOrder is held by a commit from before it advances the manifest pointer until it has
	// run the functions registered with AfterCommit, so that those run in commit order.
	commitOrder sync.Mutex
	// references counts the callers currently using this lock. It is only ever touched while
	// holding the registry's own lock, which is what allows the registry to drop the lock once
	// nothing refers to it anymore.
	references int
}

// repositoryLockRegistry hands out one lock per repository, creating locks on demand and dropping
// them once they are unused. A map that is never pruned would instead grow with every repository
// the process has served.
type repositoryLockRegistry struct {
	mu    sync.Mutex
	locks map[string]*repositoryLock
}

// repositoryLocks is global for the Gitaly process, which assumes that a node only ever runs a
// single Gitaly process.
var repositoryLocks = repositoryLockRegistry{locks: map[string]*repositoryLock{}}

// acquire returns the lock for the repository at repoPath together with a function that gives up
// the reference to it.
//
// The reference must be given up after the lock has been released, never before. Dropping it first
// can take a lock out of the registry while it is still held. The next caller for that repository
// then gets a new lock rather than this one, and two callers holding different locks exclude
// nothing.
func (r *repositoryLockRegistry) acquire(repoPath string) (*repositoryLock, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	lock, ok := r.locks[repoPath]
	if !ok {
		lock = &repositoryLock{sem: semaphore.NewWeighted(math.MaxInt64)}
		r.locks[repoPath] = lock
	}

	lock.references++

	return lock, sync.OnceFunc(func() {
		r.mu.Lock()
		defer r.mu.Unlock()

		lock.references--
		if lock.references == 0 {
			delete(r.locks, repoPath)
		}
	})
}

// LockRepositoryForWrite locks the repository at the given absolute path against garbage collection
// for the duration of a write, and returns a function that unlocks it again.
//
// Writes do not exclude each other, as they are already serialised by the compare-and-swap on the
// manifest pointer.
//
// Reads do not take the lock at all: a read references a manifest that the retention policy keeps
// alive, and so is not affected by what garbage collection removes. A read against a manifest old enough
// to have been pruned is outside that contract and may fail, which is what the retention window
// has to be sized for.
//
// Taking the lock also asks a garbage collection run that is already in progress to stop, so that a write
// does not wait for one to finish. It still has to wait for whatever the run is in the middle of,
// though, and a single unlink can block in the kernel for seconds, so the wait is cancellable.
//
// The returned function is safe to call more than once.
func LockRepositoryForWrite(ctx context.Context, repoPath string) (func(), error) {
	lock, dropReference := repositoryLocks.acquire(repoPath)

	lock.writersWaiting.Add(1)
	err := lock.sem.Acquire(ctx, 1)
	lock.writersWaiting.Add(-1)

	if err != nil {
		dropReference()
		return nil, fmt.Errorf("locking repository for write: %w", err)
	}

	return sync.OnceFunc(func() {
		lock.sem.Release(1)
		dropReference()
	}), nil
}

// CollectionLock is the exclusive hold on a repository that garbage collection deletes under.
type CollectionLock struct {
	lock       *repositoryLock
	acquiredAt time.Time
	unlock     func()
}

// TryLockRepositoryForCollection locks the repository at the given absolute path for garbage
// collection, or returns nil if it is being written to.
//
// It must not become a blocking acquisition. A queued full acquisition makes the writes that
// arrive behind it wait too, so waiting for the lock would let one long-running write stall every
// write that arrives while we wait. Collection is opportunistic instead: it gives up immediately
// and the next run picks the work back up.
func TryLockRepositoryForCollection(repoPath string) *CollectionLock {
	lock, dropReference := repositoryLocks.acquire(repoPath)

	if !lock.sem.TryAcquire(math.MaxInt64) {
		dropReference()
		return nil
	}

	return &CollectionLock{
		lock:       lock,
		acquiredAt: time.Now(),
		unlock: sync.OnceFunc(func() {
			lock.sem.Release(math.MaxInt64)
			dropReference()
		}),
	}
}

// Evicted reports whether a write is waiting for the repository. Garbage collection is expected to
// stop what it is doing and unlock when it is, so that the write does not have to wait for a whole
// run to finish.
//
// It stays false for the first evictionGracePeriod after the lock was taken, so that a run always
// gets far enough to be worth having taken it.
func (l *CollectionLock) Evicted() bool {
	if time.Since(l.acquiredAt) < evictionGracePeriod {
		return false
	}

	return l.lock.writersWaiting.Load() > 0
}

// HeldFor returns how long the lock has been held.
func (l *CollectionLock) HeldFor() time.Duration {
	return time.Since(l.acquiredAt)
}

// Unlock releases the lock. It is safe to call more than once.
func (l *CollectionLock) Unlock() {
	l.unlock()
}
