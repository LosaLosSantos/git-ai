package derived

// Black-box spec tests for the eager warm pool and the warmer registry that
// WP26 depends on (PLAN3.md, RECONCILIATION, WP24 section):
//
//   func RegisterWarmer(name string, fn WarmFunc)
//   func Warmers() []string // sorted
//   type WarmFunc func(ctx context.Context, env WarmEnv, commits []git.ObjectID) error
//   type WarmEnv struct { Logger log.Logger; Repo *localrepo.Repo; Reader catfile.ObjectContentReader; Store *Store }
//   NewPool(logger, workers, queueSize, open func(ctx, repo storage.Repository) (WarmEnv, func(), error))
//   (*Pool).Enqueue(repo storage.Repository, commits []git.ObjectID) // non-blocking, counts drops
//   (*Pool).Run(ctx) / (*Pool).Close()
//   metrics: graft_derived_warm_queue_depth, graft_derived_warm_dropped_total,
//            graft_derived_warm_computed_total, graft_derived_warm_errors_total
//
// Only exported identifiers from the spec and Gitaly test helpers are used.

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
)

// specPoolRepo is the repository value handed to the pool. The pool only
// forwards it to the open function and must not require a real repository.
func specPoolRepo() storage.Repository {
	return &gitalypb.Repository{StorageName: "default", RelativePath: "spec-wp26.git"}
}

func specNoopOpen(ctx context.Context, repo storage.Repository) (WarmEnv, func(), error) {
	return WarmEnv{}, func() {}, nil
}

func specCommit(c byte) git.ObjectID {
	b := make([]byte, 40)
	for i := range b {
		b[i] = c
	}
	return git.ObjectID(b)
}

// TestSpecPoolRunsEveryRegisteredWarmer: a registered warmer appears in the
// sorted Warmers() list, and after Enqueue the pool runs warmers on the
// enqueued commits (every enqueued commit reaches the warmer).
func TestSpecPoolRunsEveryRegisteredWarmer(t *testing.T) {
	// Warmer names are lowercase letters and dashes; registerToyWarmer removes
	// the warmer when the test ends so that it does not run in other tests.
	name := "spec-wp-recorder-" + specSuffix()

	var mu sync.Mutex
	var got []git.ObjectID
	registerToyWarmer(t, name, func(ctx context.Context, env WarmEnv, commits []git.ObjectID) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, commits...)
		return nil
	})

	names := Warmers()
	if !sort.StringsAreSorted(names) {
		t.Fatalf("Warmers() must be sorted, got %v", names)
	}
	found := false
	for _, n := range names {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("Warmers() = %v, expected it to contain registered warmer %q", names, name)
	}

	pool := NewPool(testhelper.NewLogger(t), 1, 8, specNoopOpen)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer pool.Close()
	go pool.Run(ctx)

	want := []git.ObjectID{specCommit('1'), specCommit('2')}
	pool.Enqueue(specPoolRepo(), want)

	snapshot := func() []git.ObjectID {
		mu.Lock()
		defer mu.Unlock()
		return append([]git.ObjectID(nil), got...)
	}
	deadline := time.After(10 * time.Second)
	for {
		have := make(map[git.ObjectID]bool)
		for _, c := range snapshot() {
			have[c] = true
		}
		all := true
		for _, w := range want {
			if !have[w] {
				all = false
			}
		}
		if all {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("warmer did not receive all enqueued commits within 10s; got %v, want %v", snapshot(), want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestSpecPoolEnqueueNeverBlocksWhenQueueFull: with a single worker stuck in
// its open function and a queue of size 1, Enqueue never blocks; excess
// commits are dropped and counted in graft_derived_warm_dropped_total.
func TestSpecPoolEnqueueNeverBlocksWhenQueueFull(t *testing.T) {
	release := make(chan struct{})
	blockingOpen := func(ctx context.Context, repo storage.Repository) (WarmEnv, func(), error) {
		<-release
		return WarmEnv{}, func() {}, nil
	}

	pool := NewPool(testhelper.NewLogger(t), 1, 1, blockingOpen)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer pool.Close()
	defer close(release) // runs before pool.Close so a blocked worker can finish
	go pool.Run(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			pool.Enqueue(specPoolRepo(), []git.ObjectID{specCommit(byte('a' + i%26))})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue blocked while the queue was full; it must be non-blocking")
	}

	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(pool); err != nil {
		t.Fatalf("pool must be registrable as a prometheus collector: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering pool metrics: %v", err)
	}
	var dropped float64
	foundMetric := false
	for _, mf := range mfs {
		if mf.GetName() != "graft_derived_warm_dropped_total" {
			continue
		}
		foundMetric = true
		for _, m := range mf.GetMetric() {
			dropped += m.GetCounter().GetValue()
		}
	}
	if !foundMetric {
		t.Fatal("metric graft_derived_warm_dropped_total not exported")
	}
	if dropped <= 0 {
		t.Fatalf("expected dropped commits to be counted when the queue is full, got %v", dropped)
	}
}

// TestSpecPoolRunReturnsOnCancel: Run(ctx) returns once its context is
// cancelled, and Close afterwards does not hang.
func TestSpecPoolRunReturnsOnCancel(t *testing.T) {
	pool := NewPool(testhelper.NewLogger(t), 1, 4, specNoopOpen)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run(ctx) did not return within 5s after the context was cancelled")
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		pool.Close()
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() hung after Run returned")
	}
}
