package mvcc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrTimeoutWaitingOnFetch is returned when the request's context timeout.
// This is to indicate that a `Run` operation returned not because of an error
// with the operation, but rather because the operation is taking longer then
// what the request is allowed to wait for.
var ErrTimeoutWaitingOnFetch error = errors.New("keyed-runner-cache: timeout waiting on fetch")

// maxArtifactFetchTimeout is the longest an artifact fetch may run for. Fetches are detached from
// the caller's context, so this bounds how long one may still be writing into the MVCC directory
// after the RPC that started it has returned.
const maxArtifactFetchTimeout = 3 * time.Hour

// KeyedRunnerCache is a component that runs operations keyed on a type K.
// For each item in the KeyedRunnerCache, an operation is only run once.
// The first caller to register an operation for a key will trigger
// the execution of the operation. Subsequent concurrent calls for the
// same key will not have their operation run. They will instead block
// on the execution of the initial one.
// This component allows two features:
// 1. Coalescing requests for the same key, to avoid duplication
// 2. Keep the operation running even if the caller's context is canceled.
type KeyedRunnerCache[K comparable] struct {
	// mu guards access the items in the map.
	mu sync.Mutex

	// items are the items in the cache.
	items map[K]*runnerCacheEntry
}

type runnerCacheEntry struct {
	// done signal when the operation related to this entry is completed
	done chan struct{}

	// err is the result of the operation.
	err error

	// consumers is the number of caller waiting for the result of this entry
	consumers int

	// completed indicates if the operation related to this entry is complete or not.
	completed bool

	// cancelFn can be called to cancel the operation related to that entry
	cancelFn context.CancelFunc
}

// NewKeyedRunnerCache creates an empty KeyedRunnerCache instance.
func NewKeyedRunnerCache[K comparable]() *KeyedRunnerCache[K] {
	return &KeyedRunnerCache[K]{
		items: make(map[K]*runnerCacheEntry),
	}
}

// Run executes op for the given key, coalescing concurrent calls so that op runs
// only once per key. It returns the result of the operation, or the caller's
// context error if the caller gives up before the operation completes. The
// entry is evicted once the operation has completed and the last caller
// returned.
func (c *KeyedRunnerCache[K]) Run(ctx context.Context, key K, op func(context.Context) error) error {
	c.mu.Lock()
	item, ok := c.items[key]
	// Only run `op()` if no current item for this key is in the cache
	if !ok {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maxArtifactFetchTimeout)

		// At this point the item is not in the map, so this is the first call for
		// this key. Create the entry and add it into the map so that subsequent
		// caller get the same time. This is what allows them to block until
		// the operation completes.
		item = &runnerCacheEntry{
			done:     make(chan struct{}),
			cancelFn: cancel,
		}
		c.items[key] = item

		// This caller is the leader: start the work in the background under a
		// detached context so it outlives the caller that initiated it.
		go func() {
			defer cancel()
			c.run(cctx, key, item, op)
		}()
	}
	item.consumers++
	c.mu.Unlock()

	// Drop this caller's reference as soon as it stops waiting, no matter whether
	// it observed the result or gave up on its context.
	defer c.release(key, item)

	select {
	case <-ctx.Done():
		// The caller's context has been canceled. We return the context error
		// but the operation keeps running in the background.
		return fmt.Errorf("%w: %w", ErrTimeoutWaitingOnFetch, ctx.Err())
	case <-item.done:
		return item.err
	}
}

// Drain removes all entries in the cache, canceling the operations
// related to those entries in the process.
func (c *KeyedRunnerCache[K]) Drain() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k, v := range c.items {
		v.cancelFn()
		delete(c.items, k)
	}
}

// run executes do, records its result on item, and then unblocks every waiter by
// closing item.done.
func (c *KeyedRunnerCache[K]) run(ctx context.Context, key K, item *runnerCacheEntry, do func(context.Context) error) {
	// `do` is a user-provided function, and may panic. To guard against this, we wrap its
	// execution inside a new function, with a defer statement that recovers in case of a
	// panic. That allows us to capture the recover error, if any, while also executing the
	// rest of the code below.
	executeDo := func() (err error) {
		defer func() {
			if recErr := recover(); recErr != nil {
				err = fmt.Errorf("KeyedRunnerCache.run: panic recovered: %v", recErr)
			}
		}()
		return do(ctx)
	}

	err := executeDo()

	c.mu.Lock()
	item.err = err
	item.completed = true
	if err != nil {
		// Discard the failed entry so that subsequent callers start a fresh
		// execution rather than reusing the failure. Callers already attached
		// still have a reference to the item so it is safe to delete.
		if c.items[key] == item {
			delete(c.items, key)
		}
	} else {
		c.evictIfDone(key, item)
	}
	c.mu.Unlock()

	// This unblocks all caller who are blocked waiting on this channel
	close(item.done)
}

// release drops one reference on the given cache entry. It is called once per
// caller of Run. The entry is removed once the work has finished and the last
// reference is dropped.
func (c *KeyedRunnerCache[K]) release(key K, item *runnerCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	item.consumers--
	c.evictIfDone(key, item)
}

// evictIfDone removes item from the map if its work has finished and it has no
// remaining references. The caller must hold c.mu.
func (c *KeyedRunnerCache[K]) evictIfDone(key K, item *runnerCacheEntry) {
	if item.consumers == 0 && item.completed && c.items[key] == item {
		delete(c.items, key)
	}
}
