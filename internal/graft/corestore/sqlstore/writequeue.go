package sqlstore

import (
	"context"
	"sync"
)

// writeQueue admits the SQLite write transactions of one Store one at a time,
// in the order the writers arrive (doc/graft/PLAN7.md, WP73b).
//
// SQLite has a single write lock, and a writer that does not get it waits in
// SQLite's busy handler, which keeps no queue: the lock goes to whichever
// waiter happens to retry first after it is released, so an unlucky writer can
// lose again and again while the lock is never idle. Measured on the dense
// commit test (32 writers on one reference, two cores): a mean wait of 0.16 s
// and a longest wait of 3.05 s while a fair queue bounds a wait near 0.3 s;
// under heavier load the tail crossed the busy timeout of 10 s and the write
// failed with "database is locked". The Store knows its own writers, so it
// queues them itself before BEGIN IMMEDIATE: a writer waits behind the writers
// ahead of it, never behind luck, and the end of its context ends the wait at
// once. SQLite admits one writer at a time anyway, so nothing is lost.
//
// The queue only orders the writers of this process. Several processes on one
// SQLite file (several nodes on one host) still meet in SQLite's busy handler,
// which is why multi-node deployments use Postgres (doc/graft/STORAGE.md).
type writeQueue struct {
	mu sync.Mutex
	// busy is true while a writer holds the admission.
	busy bool
	// waiters are the writers waiting for the admission, first come first. The
	// admission is handed to the first one directly (its channel is closed), so
	// a writer that arrives in between never overtakes it.
	waiters []chan struct{}
}

// queuedHook, when set by a test, is called each time a writer joins the queue,
// with the number of writers then waiting, after its position is fixed. It is
// nil in production.
var queuedHook func(waiting int)

// acquire waits for the admission. It returns ctx's error, holding nothing,
// when ctx ends first.
func (q *writeQueue) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	q.mu.Lock()
	if !q.busy {
		q.busy = true
		q.mu.Unlock()
		return nil
	}
	turn := make(chan struct{})
	q.waiters = append(q.waiters, turn)
	waiting := len(q.waiters)
	q.mu.Unlock()

	if queuedHook != nil {
		queuedHook(waiting)
	}

	select {
	case <-turn:
		return nil
	case <-ctx.Done():
	}

	// The context ended. Leave the queue, unless the admission was handed over
	// at the same moment: then pass it on, so that it is never lost.
	q.mu.Lock()
	for i, w := range q.waiters {
		if w == turn {
			q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
			q.mu.Unlock()
			return ctx.Err()
		}
	}
	q.mu.Unlock()
	q.release()
	return ctx.Err()
}

// release hands the admission to the first waiting writer, or frees it.
func (q *writeQueue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.waiters) == 0 {
		q.busy = false
		return
	}
	next := q.waiters[0]
	q.waiters = q.waiters[1:]
	close(next)
}

// waiting returns the number of writers waiting for the admission.
func (q *writeQueue) waiting() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiters)
}
