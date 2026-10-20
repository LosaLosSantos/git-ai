package operations

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// keysOf returns the keys of scope that still have a holder or a waiter, for the tests.
func (r *landTicketRegistry) keysOf(scope string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for k := range r.keys {
		if strings.HasPrefix(k, scope+"\x00") {
			out = append(out, k)
		}
	}
	return out
}

func newTestTickets() *landTicketRegistry {
	return &landTicketRegistry{keys: map[string]*landTicketKey{}}
}

// TestLandTickets_firstInFirstOut: the holder of a key keeps it until it releases it, and the waiters
// get it one after the other in the order they asked.
func TestLandTickets_firstInFirstOut(t *testing.T) {
	t.Parallel()

	r := newTestTickets()
	keys := landTicketKeys("repo", []string{"a"}, false)
	first, err := r.acquire(context.Background(), keys)
	require.NoError(t, err)

	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	tickets := make([]*landTicket, 5)
	for i := range tickets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk, err := r.acquire(context.Background(), keys)
			require.NoError(t, err)
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			tickets[i] = tk
			tk.release()
		}()
		// They ask one after the other.
		require.Eventually(t, func() bool { return r.waiting(keys[0]) == i+1 }, 5*time.Second, time.Millisecond)
	}
	require.True(t, r.blocked(keys, nil), "the holder blocks the others")
	require.False(t, r.blocked(keys, first), "and not itself")
	first.release()
	wg.Wait()
	require.Equal(t, []int{0, 1, 2, 3, 4}, order)
	require.False(t, r.blocked(keys, nil), "nothing is kept of a key without holder or waiter")
	require.Empty(t, r.keys)
}

// TestLandTickets_aWaiterThatGivesUpLeavesTheQueue: the context ends while the land waits; it holds
// nothing, and the next waiter is not stuck behind it.
func TestLandTickets_aWaiterThatGivesUpLeavesTheQueue(t *testing.T) {
	t.Parallel()

	r := newTestTickets()
	keys := landTicketKeys("repo", []string{"a"}, false)
	holder, err := r.acquire(context.Background(), keys)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan error, 1)
	go func() {
		_, err := r.acquire(ctx, keys)
		gone <- err
	}()
	require.Eventually(t, func() bool { return r.waiting(keys[0]) == 1 }, 5*time.Second, time.Millisecond)

	next := make(chan *landTicket, 1)
	go func() {
		tk, _ := r.acquire(context.Background(), keys)
		next <- tk
	}()
	require.Eventually(t, func() bool { return r.waiting(keys[0]) == 2 }, 5*time.Second, time.Millisecond)

	cancel()
	require.ErrorIs(t, <-gone, context.Canceled)
	require.Equal(t, 1, r.waiting(keys[0]))
	holder.release()
	tk := <-next
	require.NotNil(t, tk)
	tk.release()
	require.Empty(t, r.keys)
}

// TestLandTickets_orderedKeysDoNotDeadlock: lands that need the same files in another order take them
// in the same (sorted) order, so none of them waits for the other while holding what it needs.
func TestLandTickets_orderedKeysDoNotDeadlock(t *testing.T) {
	t.Parallel()

	r := newTestTickets()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		paths := []string{"a", "b", "c"}
		if i%2 == 1 {
			paths = []string{"c", "b", "a"}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			tk, err := r.acquire(ctx, landTicketKeys("repo", paths, false))
			require.NoError(t, err)
			time.Sleep(time.Millisecond)
			tk.release()
		}()
	}
	wg.Wait()
	require.Empty(t, r.keys)
}

// TestLandTickets_keys: a ticket is per file, or per branch when a verification is configured (the tree
// that is verified is the whole branch's), and a land with no file of its own takes the branch's.
func TestLandTickets_keys(t *testing.T) {
	t.Parallel()

	require.Equal(t, landTicketKeys("r", []string{"b", "a", "b"}, false), []string{"r\x00a", "r\x00b"})
	require.Equal(t, []string{"r\x00"}, landTicketKeys("r", []string{"a"}, true))
	require.Equal(t, []string{"r\x00"}, landTicketKeys("r", nil, false))
	require.NotEqual(t, landTicketScope("s", "x.git", []byte("main")), landTicketScope("s", "x.git", []byte("dev")))

	r := newTestTickets()
	tk, err := r.acquire(context.Background(), landTicketKeys("r", []string{"a", "b"}, false))
	require.NoError(t, err)
	require.True(t, tk.covers(landTicketKeys("r", []string{"a"}, false)))
	require.False(t, tk.covers(landTicketKeys("r", []string{"a", "c"}, false)))
	require.False(t, r.blocked(landTicketKeys("r", []string{"c"}, false), nil), "another file is free")
	require.True(t, r.blocked(landTicketKeys("r", []string{"c", "b"}, false), nil))
	tk.release()
	tk.release() // a second release does nothing
	require.False(t, (*landTicket)(nil).covers(nil))
}
