package eager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

const (
	zeroOID = "0000000000000000000000000000000000000000"
	oidA    = "1111111111111111111111111111111111111111"
	oidB    = "2222222222222222222222222222222222222222"
	oidC    = "3333333333333333333333333333333333333333"
)

var testRepo = &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"}

// fakeEnqueuer records the commits it is asked to enqueue.
type fakeEnqueuer struct {
	mu      sync.Mutex
	calls   [][]git.ObjectID
	explode bool
}

func (f *fakeEnqueuer) Enqueue(_ storage.Repository, commits []git.ObjectID) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.explode {
		panic("boom")
	}

	f.calls = append(f.calls, append([]git.ObjectID(nil), commits...))
}

func (f *fakeEnqueuer) recorded() [][]git.ObjectID {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([][]git.ObjectID(nil), f.calls...)
}

// fakeLister answers the listing of the new commits from a table keyed by the
// old and new value of an update, and records the listings.
type fakeLister struct {
	mu      sync.Mutex
	answers map[[2]string][]git.ObjectID
	errs    map[[2]string]error
	calls   [][2]string
}

func (l *fakeLister) list(_ context.Context, _ storage.Repository, oldOID, newOID string) ([]git.ObjectID, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := [2]string{oldOID, newOID}
	l.calls = append(l.calls, key)

	if err := l.errs[key]; err != nil {
		return nil, err
	}

	return l.answers[key], nil
}

func (l *fakeLister) listed() [][2]string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([][2]string(nil), l.calls...)
}

// fakeWriter counts the commit-graph writes per repository.
type fakeWriter struct {
	mu     sync.Mutex
	counts int
}

func (w *fakeWriter) write(_ context.Context, _ storage.Repository) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.counts++
	return nil
}

func (w *fakeWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.counts
}

func TestObserveCommitted_EnqueuesNewCommitsOfEveryTarget(t *testing.T) {
	ctx := testhelper.Context(t)
	enqueuer := &fakeEnqueuer{}
	writer := &fakeWriter{}
	lister := &fakeLister{answers: map[[2]string][]git.ObjectID{
		{zeroOID, oidB}: {oidB, oidA},
		{oidA, oidC}:    {oidC, oidB, oidA},
	}}

	observer := NewObserver(ctx, testhelper.NewLogger(t),
		Options{Eager: true, CommitGraph: true, Debounce: 10 * time.Millisecond},
		Deps{Enqueuer: enqueuer, ListNewCommits: lister.list, WriteCommitGraph: writer.write})
	defer observer.Close()

	observer.ObserveCommitted(ctx, testRepo, []changelog.Update{
		{Reference: "refs/heads/main", OldOID: zeroOID, NewOID: oidB},
		{Reference: "refs/heads/feature", OldOID: oidA, NewOID: oidC},
		{Reference: "refs/heads/gone", OldOID: oidA, NewOID: zeroOID},
		{Reference: "refs/heads/main", OldOID: zeroOID, NewOID: oidB},
	})

	require.Eventually(t, func() bool { return len(enqueuer.recorded()) == 1 }, time.Second, time.Millisecond)
	require.Equal(t, [][]git.ObjectID{{oidB, oidA, oidC}}, enqueuer.recorded(),
		"every new commit is enqueued once, not only the tips")
	require.Equal(t, [][2]string{{zeroOID, oidB}, {oidA, oidC}}, lister.listed(),
		"deletions and repeated updates are not listed")
	require.Eventually(t, func() bool { return writer.count() == 1 }, time.Second, time.Millisecond)
}

func TestObserveCommitted_ListingErrorSkipsOnlyThatUpdate(t *testing.T) {
	ctx := testhelper.Context(t)
	enqueuer := &fakeEnqueuer{}
	lister := &fakeLister{
		answers: map[[2]string][]git.ObjectID{{zeroOID, oidC}: {oidC}},
		errs:    map[[2]string]error{{oidA, oidB}: errors.New("bad object")},
	}

	observer := NewObserver(ctx, testhelper.NewLogger(t), Options{Eager: true},
		Deps{Enqueuer: enqueuer, ListNewCommits: lister.list})
	defer observer.Close()

	observer.ObserveCommitted(ctx, testRepo, []changelog.Update{
		{Reference: "refs/heads/broken", OldOID: oidA, NewOID: oidB},
		{Reference: "refs/heads/new", OldOID: zeroOID, NewOID: oidC},
	})

	require.Eventually(t, func() bool { return len(enqueuer.recorded()) == 1 }, time.Second, time.Millisecond)
	require.Equal(t, [][]git.ObjectID{{oidC}}, enqueuer.recorded())
}

func TestObserveCommitted_DeletionsOnlyScheduleCommitGraph(t *testing.T) {
	ctx := testhelper.Context(t)
	enqueuer := &fakeEnqueuer{}
	writer := &fakeWriter{}
	lister := &fakeLister{}

	observer := NewObserver(ctx, testhelper.NewLogger(t),
		Options{Eager: true, CommitGraph: true, Debounce: 5 * time.Millisecond},
		Deps{Enqueuer: enqueuer, ListNewCommits: lister.list, WriteCommitGraph: writer.write})
	defer observer.Close()

	observer.ObserveCommitted(ctx, testRepo, []changelog.Update{
		{Reference: "refs/heads/main", OldOID: oidA, NewOID: zeroOID},
	})

	require.Eventually(t, func() bool { return writer.count() == 1 }, time.Second, time.Millisecond)
	require.Empty(t, lister.listed())
	require.Empty(t, enqueuer.recorded())
}

func TestObserveCommitted_NoUpdatesDoesNothing(t *testing.T) {
	ctx := testhelper.Context(t)
	enqueuer := &fakeEnqueuer{}
	writer := &fakeWriter{}
	lister := &fakeLister{}

	observer := NewObserver(ctx, testhelper.NewLogger(t),
		Options{Eager: true, CommitGraph: true, Debounce: 5 * time.Millisecond},
		Deps{Enqueuer: enqueuer, ListNewCommits: lister.list, WriteCommitGraph: writer.write})
	defer observer.Close()

	observer.ObserveCommitted(ctx, testRepo, nil)

	time.Sleep(30 * time.Millisecond)
	require.Empty(t, lister.listed())
	require.Empty(t, enqueuer.recorded())
	require.Zero(t, writer.count())
}

func TestObserveCommitted_DisabledTriggers(t *testing.T) {
	ctx := testhelper.Context(t)
	updates := []changelog.Update{{Reference: "refs/heads/main", OldOID: oidA, NewOID: oidB}}

	t.Run("eager off", func(t *testing.T) {
		enqueuer := &fakeEnqueuer{}
		lister := &fakeLister{}

		observer := NewObserver(ctx, testhelper.NewLogger(t), Options{Eager: false},
			Deps{Enqueuer: enqueuer, ListNewCommits: lister.list})
		defer observer.Close()

		observer.ObserveCommitted(ctx, testRepo, updates)
		require.Empty(t, lister.listed())
		require.Empty(t, enqueuer.recorded())
	})

	t.Run("no enqueuer", func(t *testing.T) {
		writer := &fakeWriter{}
		lister := &fakeLister{}

		observer := NewObserver(ctx, testhelper.NewLogger(t),
			Options{Eager: true, CommitGraph: true, Debounce: 5 * time.Millisecond},
			Deps{ListNewCommits: lister.list, WriteCommitGraph: writer.write})
		defer observer.Close()

		observer.ObserveCommitted(ctx, testRepo, updates)
		require.Eventually(t, func() bool { return writer.count() == 1 }, time.Second, time.Millisecond)
		require.Empty(t, lister.listed())
	})

	t.Run("commit-graph off", func(t *testing.T) {
		enqueuer := &fakeEnqueuer{}
		lister := &fakeLister{answers: map[[2]string][]git.ObjectID{{oidA, oidB}: {oidB}}}
		writer := &fakeWriter{}

		observer := NewObserver(ctx, testhelper.NewLogger(t),
			Options{Eager: true, CommitGraph: false, Debounce: 5 * time.Millisecond},
			Deps{Enqueuer: enqueuer, ListNewCommits: lister.list, WriteCommitGraph: writer.write})
		defer observer.Close()

		observer.ObserveCommitted(ctx, testRepo, updates)
		require.Eventually(t, func() bool { return len(enqueuer.recorded()) == 1 }, time.Second, time.Millisecond)

		time.Sleep(30 * time.Millisecond)
		require.Zero(t, writer.count())
	})
}

func TestObserveCommitted_PanicIsRecovered(t *testing.T) {
	ctx := testhelper.Context(t)
	lister := &fakeLister{answers: map[[2]string][]git.ObjectID{{oidA, oidB}: {oidB}}}

	observer := NewObserver(ctx, testhelper.NewLogger(t), Options{Eager: true},
		Deps{Enqueuer: &fakeEnqueuer{explode: true}, ListNewCommits: lister.list})

	require.NotPanics(t, func() {
		observer.ObserveCommitted(ctx, testRepo, []changelog.Update{{Reference: "refs/heads/main", OldOID: oidA, NewOID: oidB}})
	})

	// Close waits for the listing, so a leaked panic would take the test down.
	observer.Close()
}

func TestObserveCommitted_ClosedObserverDoesNothing(t *testing.T) {
	ctx := testhelper.Context(t)
	enqueuer := &fakeEnqueuer{}
	writer := &fakeWriter{}
	lister := &fakeLister{}

	observer := NewObserver(ctx, testhelper.NewLogger(t),
		Options{Eager: true, CommitGraph: true, Debounce: 5 * time.Millisecond},
		Deps{Enqueuer: enqueuer, ListNewCommits: lister.list, WriteCommitGraph: writer.write})
	observer.Close()
	observer.Close()

	observer.ObserveCommitted(ctx, testRepo, []changelog.Update{{Reference: "refs/heads/main", OldOID: oidA, NewOID: oidB}})

	time.Sleep(20 * time.Millisecond)
	require.Empty(t, lister.listed())
	require.Empty(t, enqueuer.recorded())
	require.Zero(t, writer.count())
}

func TestObserveCommitted_NilObserver(t *testing.T) {
	ctx := testhelper.Context(t)

	var observer *Observer
	require.NotPanics(t, func() {
		observer.ObserveCommitted(ctx, testRepo, []changelog.Update{{Reference: "refs/heads/main", OldOID: oidA, NewOID: oidB}})
		observer.Close()
	})
}

// TestObserveCommitted_ListingsAreBounded commits many transactions at once
// while every listing blocks. Only ListWorkers listings may run at a time, the
// queue holds ListQueueSize transactions and the rest are dropped and counted,
// without ObserveCommitted ever blocking.
func TestObserveCommitted_ListingsAreBounded(t *testing.T) {
	ctx := testhelper.Context(t)
	enqueuer := &fakeEnqueuer{}

	const (
		workers      = 2
		queueSize    = 5
		transactions = 300
	)

	var running, maxRunning, listed atomic.Int32
	release := make(chan struct{})
	lister := func(ctx context.Context, _ storage.Repository, _, newOID string) ([]git.ObjectID, error) {
		now := running.Add(1)
		defer running.Add(-1)
		for {
			highest := maxRunning.Load()
			if now <= highest || maxRunning.CompareAndSwap(highest, now) {
				break
			}
		}

		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		listed.Add(1)
		return []git.ObjectID{git.ObjectID(newOID)}, nil
	}

	observer := NewObserver(ctx, testhelper.NewLogger(t),
		Options{Eager: true, ListWorkers: workers, ListQueueSize: queueSize},
		Deps{Enqueuer: enqueuer, ListNewCommits: lister})
	defer observer.Close()

	for i := 0; i < transactions; i++ {
		observer.ObserveCommitted(ctx, testRepo, []changelog.Update{
			// The new OID is never zero, which would be a deletion that lists nothing.
			{Reference: fmt.Sprintf("refs/heads/b%d", i), OldOID: zeroOID, NewOID: fmt.Sprintf("%040d", i+1)},
		})
	}

	require.Eventually(t, func() bool { return running.Load() == workers }, 5*time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, int32(workers), maxRunning.Load(), "listings beyond the workers do not start")

	dropped := observer.droppedListings.Load()
	require.GreaterOrEqual(t, dropped, int64(transactions-workers-queueSize))
	require.LessOrEqual(t, dropped, int64(transactions-queueSize))

	testhelper.RequirePromMetrics(t, observer, fmt.Sprintf(`# HELP graft_eager_dropped_listings_total Number of committed reference transactions whose new commits were not listed for the eager derived-data warmers because the listing queue was full.
# TYPE graft_eager_dropped_listings_total counter
graft_eager_dropped_listings_total %d
`, dropped))

	close(release)

	accepted := transactions - int(dropped)
	require.Eventually(t, func() bool { return len(enqueuer.recorded()) == accepted }, 5*time.Second, time.Millisecond,
		"every accepted transaction is listed and enqueued")
	require.LessOrEqual(t, maxRunning.Load(), int32(workers))
	require.Equal(t, int32(accepted), listed.Load())
}

func TestObserveCommitted_CloseStopsBlockedListings(t *testing.T) {
	ctx := testhelper.Context(t)

	started := make(chan struct{}, 1)
	lister := func(ctx context.Context, _ storage.Repository, _, _ string) ([]git.ObjectID, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	observer := NewObserver(ctx, testhelper.NewLogger(t),
		Options{Eager: true, ListWorkers: 1, ListQueueSize: 4},
		Deps{Enqueuer: &fakeEnqueuer{}, ListNewCommits: lister})

	for i := 0; i < 3; i++ {
		observer.ObserveCommitted(ctx, testRepo, []changelog.Update{
			{Reference: "refs/heads/main", OldOID: oidA, NewOID: oidB},
		})
	}
	<-started

	// Close cancels the running listing, drops the queued ones and returns.
	observer.Close()
	require.Len(t, started, 0, "no queued listing starts after Close")
}

func TestUpdatedTargets(t *testing.T) {
	require.Nil(t, updatedTargets(nil))
	require.Equal(t, []updateTarget{
		{reference: "refs/heads/a", oldOID: zeroOID, newOID: oidA},
		{reference: "refs/heads/b", oldOID: oidA, newOID: oidB},
		{reference: "refs/tags/v1", oldOID: zeroOID, newOID: oidA},
	}, updatedTargets([]changelog.Update{
		{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA},
		{Reference: "refs/heads/gone", OldOID: oidC, NewOID: zeroOID},
		{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA},
		{Reference: "refs/heads/b", OldOID: oidA, NewOID: oidB},
		{Reference: "refs/tags/v1", OldOID: zeroOID, NewOID: oidA},
	}))
}
