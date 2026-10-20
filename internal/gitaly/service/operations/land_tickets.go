package operations

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The tickets of the lands that lose a round (PLAN10 15.8).
//
// A land with conflicts is resolved out of the lane, and a file's verdict is valid only for the exact
// sides of the file, so every landing that changes a hot file invalidates the verdicts of all the
// lands that wait for it. Without an order they all resolve again after every landing: N lands on one
// file cost about N*N/2 resolver calls, which fill the resolve slots, and which land first is nobody's
// choice, so a land can lose every round until resolve_rounds ends it.
//
// A land that has lost a round therefore takes a ticket: a first-in first-out queue per file (per
// branch when a verification is configured, since the tree that is verified is the whole branch's). The
// holder is the only land that calls the resolver for those files after losing a round; the others wait
// without a request. The holder keeps the ticket until it lands, its deadline ends, or it fails: nothing
// takes it away. The lane hands back, to wait in the queue, any other land with a verdict on a ticketed
// file (landApplyVerdicts), so that the holder's verdict is the one that lands. The queue is in memory
// and per process, like the lanes: against another node of the same branch (authoritative storage) the
// lands go on as they did, in rounds, without the guarantee.

// landTicketWait is the time a land waited for the tickets of its files.
var landTicketWait = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "graft_land_ticket_wait_seconds",
	Help:    "Time a land that lost a round of its conflict resolution waited for its turn on its files.",
	Buckets: prometheus.ExponentialBuckets(0.005, 4, 9),
})

// landResolutionRounds is the number of rounds the lands with conflicts used.
var landResolutionRounds = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "graft_land_resolution_rounds",
	Help:    "Rounds (resolve, then the lane) a land with conflicts needed.",
	Buckets: []float64{1, 2, 3, 4, 6, 8, 12, 16, 24, 32, 64},
})

// landTicketRegistry is the tickets of the process.
type landTicketRegistry struct {
	mu   sync.Mutex
	keys map[string]*landTicketKey
}

// landTicketKey is one file (or branch) and its queue.
type landTicketKey struct {
	holder  *landTicket
	waiters []*landTicketWaiter
}

type landTicketWaiter struct {
	ticket *landTicket
	ready  chan struct{}
}

// landTicket is the right to resolve a set of files of a branch before the other lands that lost a round.
type landTicket struct {
	reg  *landTicketRegistry
	keys []string // sorted: the order they are taken in, so that two lands cannot wait for each other

	mu       sync.Mutex
	acquired []string // the keys held, in the order taken
	released bool
	// priority says that the holder resolves its files out of the lane after losing a round: until it ends, the
	// lane hands back the inline resolutions of the other lands on them, which would change the files again and
	// make its verdict stale at every round (priorityBlocked).
	priority bool
}

// landTickets are the tickets of the process, shared by all the servers of it.
var landTickets = &landTicketRegistry{keys: map[string]*landTicketKey{}}

// landTicketKeys names the tickets of the files of a branch: one per path, or the one of the branch when
// branchWide. scope is the repository and the branch.
func landTicketKeys(scope string, paths []string, branchWide bool) []string {
	if branchWide || len(paths) == 0 {
		return []string{scope + "\x00"}
	}
	seen := make(map[string]bool, len(paths))
	keys := make([]string, 0, len(paths))
	for _, p := range paths {
		k := scope + "\x00" + p
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// landTicketScope is what the tickets of a land are about: its repository and its branch.
func landTicketScope(storage, relativePath string, branch []byte) string {
	return strings.Join([]string{storage, relativePath, string(branch)}, "\x00")
}

// acquire waits for the tickets of keys, first in first out on each, and takes them in sorted order. It
// returns when it holds all of them, or with ctx's error holding none.
func (r *landTicketRegistry) acquire(ctx context.Context, keys []string) (*landTicket, error) {
	start := time.Now()
	t := &landTicket{reg: r, keys: keys}
	for _, key := range keys {
		if err := r.take(ctx, t, key); err != nil {
			t.release()
			landTicketWait.Observe(time.Since(start).Seconds())
			return nil, err
		}
	}
	landTicketWait.Observe(time.Since(start).Seconds())
	return t, nil
}

// take makes t the holder of key, waiting for the holders and the waiters before it.
func (r *landTicketRegistry) take(ctx context.Context, t *landTicket, key string) error {
	r.mu.Lock()
	k := r.keys[key]
	if k == nil {
		k = &landTicketKey{}
		r.keys[key] = k
	}
	if k.holder == nil && len(k.waiters) == 0 {
		k.holder = t
		r.mu.Unlock()
		t.noteAcquired(key)
		return nil
	}
	w := &landTicketWaiter{ticket: t, ready: make(chan struct{})}
	k.waiters = append(k.waiters, w)
	r.mu.Unlock()

	select {
	case <-w.ready:
		t.noteAcquired(key)
		return nil
	case <-ctx.Done():
		r.mu.Lock()
		for i, x := range k.waiters {
			if x == w {
				// Still queued: leave it.
				k.waiters = append(k.waiters[:i], k.waiters[i+1:]...)
				r.mu.Unlock()
				return ctx.Err()
			}
		}
		r.mu.Unlock()
		// The key was handed over while the context ended: it is held, and the caller releases it.
		t.noteAcquired(key)
		return ctx.Err()
	}
}

func (t *landTicket) noteAcquired(key string) {
	t.mu.Lock()
	t.acquired = append(t.acquired, key)
	t.mu.Unlock()
}

// release gives every key back, to the next waiter of each; a second call does nothing.
func (t *landTicket) release() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.released {
		t.mu.Unlock()
		return
	}
	t.released = true
	acquired := t.acquired
	t.mu.Unlock()

	r := t.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(acquired) - 1; i >= 0; i-- {
		k := r.keys[acquired[i]]
		if k == nil || k.holder != t {
			continue
		}
		if len(k.waiters) == 0 {
			delete(r.keys, acquired[i])
			continue
		}
		next := k.waiters[0]
		k.waiters = k.waiters[1:]
		k.holder = next.ticket
		close(next.ready)
	}
}

// covers reports whether t holds a ticket for every one of keys: a released ticket holds none.
func (t *landTicket) covers(keys []string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.released {
		return false
	}
	held := make(map[string]bool, len(t.acquired))
	for _, k := range t.acquired {
		held[k] = true
	}
	for _, k := range keys {
		if !held[k] {
			return false
		}
	}
	return true
}

// setPriority makes the ticket one that the inline resolutions of the other lands give way to.
func (t *landTicket) setPriority() {
	t.mu.Lock()
	t.priority = true
	t.mu.Unlock()
}

func (t *landTicket) hasPriority() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.priority && !t.released
}

// priorityBlocked reports whether another land holds, with priority, a ticket for one of keys: the land that
// asks must not change those files in the lane until the holder is done.
func (r *landTicketRegistry) priorityBlocked(keys []string, me *landTicket) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		if k := r.keys[key]; k != nil && k.holder != nil && k.holder != me && k.holder.hasPriority() {
			return true
		}
	}
	return false
}

// blocked reports whether another land holds a ticket for one of keys: the land that asks, which is me
// when it holds tickets (nil otherwise), must wait for its turn.
func (r *landTicketRegistry) blocked(keys []string, me *landTicket) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		if k := r.keys[key]; k != nil && k.holder != nil && k.holder != me {
			return true
		}
	}
	return false
}

// waiting returns the number of lands that wait for key, for the tests.
func (r *landTicketRegistry) waiting(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if k := r.keys[key]; k != nil {
		return len(k.waiters)
	}
	return 0
}
