package mvcc

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// cacheLen reports the number of entries currently held by the keyedRunner.
func cacheLen[K comparable](c *KeyedRunnerCache[K]) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// waitForConsumers blocks until the entry for key has exactly want consumers. It
// is used to deterministically wait for followers to attach to a leader's
// in-flight work before letting the work complete.
func waitForConsumers[K comparable](t *testing.T, c *KeyedRunnerCache[K], key K, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		item, ok := c.items[key]
		return ok && item.consumers == want
	}, time.Second, time.Millisecond)
}

func TestKeyedRunnerCache_RunsWorkOnceAndEvictsEntry(t *testing.T) {
	t.Parallel()

	c := NewKeyedRunnerCache[string]()

	var runCount int32
	do := func(context.Context) error {
		atomic.AddInt32(&runCount, 1)
		return nil
	}

	require.NoError(t, c.Run(context.Background(), "key", do))
	require.Equal(t, int32(1), atomic.LoadInt32(&runCount))

	// The caller's reference is dropped when Run returns, so the completed entry
	// is evicted without the caller having to do anything.
	require.Equal(t, 0, cacheLen(c))
}

func TestKeyedRunnerCache_CoalescesConcurrentCallers(t *testing.T) {
	t.Parallel()

	c := NewKeyedRunnerCache[string]()

	started := make(chan struct{}, 1)
	proceed := make(chan struct{})

	// This value should be set to 1, as we expect `do` to
	// be run only once no matter how many callers
	var runCount int32
	do := func(context.Context) error {
		atomic.AddInt32(&runCount, 1)
		started <- struct{}{}
		<-proceed
		return nil
	}

	// One caller becomes the leader and starts the work; the rest attach to it.
	const callers = 6
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.Run(context.Background(), "key", do)
		}(i)
	}

	// The work started, and every caller attached to the single entry.
	<-started
	waitForConsumers(t, c, "key", callers)
	require.Equal(t, 1, cacheLen(c))

	// Let the work finish; every caller observes success.
	close(proceed)

	// This code block here is a way to wait for the WaitGroup to unblock
	// while timing out after 1 second just in case, to avoid hanging the
	// tests unnecessarily.
	{
		done := make(chan struct{})
		go func() {
			defer close(done)
			wg.Wait()
		}()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for callers")
		}
	}

	// Validate the `op` only run once
	require.Equal(t, int32(1), atomic.LoadInt32(&runCount))

	// Validate the results of every caller
	for i := range callers {
		require.NoError(t, errs[i], "caller %d", i)
	}

	// Every caller dropped its reference on return, so the entry is gone.
	require.Equal(t, 0, cacheLen(c))
}

func TestKeyedRunnerCache_WorkContinuesAfterCallerCanceled(t *testing.T) {
	t.Parallel()

	c := NewKeyedRunnerCache[string]()

	started := make(chan struct{}, 1)
	proceed := make(chan struct{})
	finished := make(chan struct{})

	var runCount int32
	do := func(ctx context.Context) error {
		atomic.AddInt32(&runCount, 1)
		started <- struct{}{}
		<-proceed
		close(finished)
		return nil
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())

	// The leader (first caller for the key) runs an operation in the cache
	leaderCh := make(chan error, 1)
	go func() {
		leaderCh <- c.Run(leaderCtx, "key", do)
	}()

	// Wait for the `do` to start
	<-started

	// A follower with a live context attaches to the in-flight work.
	followerCh := make(chan error, 1)
	go func() {
		followerCh <- c.Run(context.Background(), "key", do)
	}()

	// Here we wait to have 2 consumers of the item:
	// 1. The leader
	// 2. The follower
	waitForConsumers(t, c, "key", 2)

	// Cancel the leader. It stops waiting and observes its context error, but the
	// background work keeps running.
	cancelLeader()
	require.ErrorIs(t, <-leaderCh, ErrTimeoutWaitingOnFetch)

	// The leader dropped its reference when Run returned, but the follower still
	// holds one so the entry stays in the cache.
	waitForConsumers(t, c, "key", 1)
	require.Equal(t, 1, cacheLen(c))

	// The follower is still waiting: the work has not been abandoned.
	select {
	case <-followerCh:
		t.Fatal("follower returned before the work finished")
	case <-time.After(100 * time.Millisecond):
		// This case should trigger, because the followerCh should not return yet
		// since the work for `op` is not done yet.
	}

	// Let the work finish.  This unblocks `op` and runs it to completion.
	close(proceed)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("background work did not finish after the leader was canceled")
	}

	// ...and the follower observes the successful result.
	require.NoError(t, <-followerCh)

	require.Equal(t, int32(1), atomic.LoadInt32(&runCount))
	require.Equal(t, 0, cacheLen(c))
}

func TestKeyedRunnerCache_DifferentKeysDoNotCoalesce(t *testing.T) {
	t.Parallel()

	c := NewKeyedRunnerCache[string]()

	started := make(chan struct{}, 2)
	proceed := make(chan struct{})

	var runCount int32
	do := func(context.Context) error {
		atomic.AddInt32(&runCount, 1)
		started <- struct{}{}
		<-proceed
		return nil
	}

	// Each key gets its own execution of the work.
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, key := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.Run(context.Background(), key, do)
		}()
	}

	// Both operations are in flight at the same time, each with its own entry.
	<-started
	<-started
	require.Equal(t, 2, cacheLen(c))

	close(proceed)
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, int32(2), atomic.LoadInt32(&runCount))
	require.Equal(t, 0, cacheLen(c))
}

func TestKeyedRunnerCache_RunAfterCompletionRunsAgain(t *testing.T) {
	t.Parallel()

	c := NewKeyedRunnerCache[string]()
	var runCount int32
	do := func(context.Context) error {
		atomic.AddInt32(&runCount, 1)
		return nil
	}

	require.NoError(t, c.Run(context.Background(), "key", do))
	require.Equal(t, 0, cacheLen(c))

	// The entry is gone, so running for the same key again triggers a fresh
	// execution.
	require.NoError(t, c.Run(context.Background(), "key", do))
	require.Equal(t, int32(2), atomic.LoadInt32(&runCount))
	require.Equal(t, 0, cacheLen(c))
}

func TestKeyedRunnerCache_RecoversPanicFromWork(t *testing.T) {
	t.Parallel()

	c := NewKeyedRunnerCache[string]()

	started := make(chan struct{}, 1)
	proceed := make(chan struct{})

	var runCount int32
	do := func(context.Context) error {
		atomic.AddInt32(&runCount, 1)
		started <- struct{}{}
		<-proceed
		panic("boom")
	}

	// The leader runs the panicking work, a follower attaches to it. Both must
	// observe the recovered panic as an error rather than crashing the process.
	leaderCh := make(chan error, 1)
	go func() {
		leaderCh <- c.Run(context.Background(), "key", do)
	}()

	<-started

	followerCh := make(chan error, 1)
	go func() {
		followerCh <- c.Run(context.Background(), "key", do)
	}()

	waitForConsumers(t, c, "key", 2)

	close(proceed)

	expectedErr := "KeyedRunnerCache.run: panic recovered: boom"
	require.EqualError(t, <-leaderCh, expectedErr)
	require.EqualError(t, <-followerCh, expectedErr)

	// The work ran only once, and the failed entry was discarded so a later call
	// for the same key starts fresh.
	require.Equal(t, int32(1), atomic.LoadInt32(&runCount))
	require.Equal(t, 0, cacheLen(c))
}

func TestKeyedRunnerCache_Drain(t *testing.T) {
	t.Parallel()

	t.Run("empty cache", func(t *testing.T) {
		t.Parallel()

		c := NewKeyedRunnerCache[string]()

		// Draining an empty cache is a no-op, and the cache stays usable.
		c.Drain()
		require.Equal(t, 0, cacheLen(c))

		require.NoError(t, c.Run(context.Background(), "key", func(context.Context) error {
			return nil
		}))
	})

	t.Run("cancels in-flight work and evicts entries", func(t *testing.T) {
		t.Parallel()

		c := NewKeyedRunnerCache[string]()

		keys := []string{"a", "b", "c"}

		started := make(chan struct{}, len(keys))

		var runCount int32
		// The work only returns once its own context is canceled, so the only way
		// for the callers below to return is for Drain to cancel it.
		do := func(ctx context.Context) error {
			atomic.AddInt32(&runCount, 1)
			started <- struct{}{}

			// The context gets canceled with calling Drain()
			<-ctx.Done()
			return ctx.Err()
		}

		// Execute one operation per key in keys
		errs := make([]error, len(keys))
		var wg sync.WaitGroup
		for i, key := range keys {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = c.Run(context.Background(), key, do)
			}()
		}

		// Every key has its work in flight and its own entry in the cache.
		for range keys {
			<-started
		}
		require.Equal(t, len(keys), cacheLen(c))

		// This should cancel all in-flight operations
		c.Drain()

		// The entries are gone right away, without waiting for the canceled work
		// to unwind.
		require.Equal(t, 0, cacheLen(c))

		wg.Wait()

		// Each caller observes the cancellation of its operation's context. Note
		// that this is the operation's error, not ErrTimeoutWaitingOnFetch: the
		// callers' own contexts are still live.
		for i, key := range keys {
			require.ErrorIs(t, errs[i], context.Canceled, "caller for key %q", key)
			require.NotErrorIs(t, errs[i], ErrTimeoutWaitingOnFetch, "caller for key %q", key)
		}
		require.Equal(t, int32(len(keys)), atomic.LoadInt32(&runCount))

		// The drained entries were discarded, so the cache is still usable and a
		// call for a drained key triggers a fresh execution.
		require.NoError(t, c.Run(context.Background(), keys[0], func(context.Context) error {
			return nil
		}))
		require.Equal(t, 0, cacheLen(c))
	})
}
