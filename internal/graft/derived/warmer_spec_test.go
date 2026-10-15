// Black-box spec tests for WP24 (Pool = eager warmer, metrics).
//
// Written from PLAN3.md (WP24 + RECONCILIATION) only. Helpers used from
// Gitaly test packages (testcfg, testhelper, gittest) are assumed to have their
// usual current signatures:
//
//	testcfg.Build(t) config.Cfg
//	testhelper.Context(t) context.Context
//	testhelper.NewLogger(t) *logrus.Entry
//	gittest.CreateRepository(t, ctx, cfg) (*gitalypb.Repository, string)
//	gittest.WriteCommit(t, cfg, repoPath, opts...) git.ObjectID
//
// Import paths assume module gitlab.com/gitlab-org/gitaly/v16; adjust if needed.
// The metrics tests assume Pool implements prometheus.Collector (WP24 says it
// exposes the graft_derived_warm_* metrics).
package derived

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/prometheus/client_golang/prometheus"
)

// specGather registers c in a fresh registry and returns the summed value of
// every gathered counter/gauge keyed by metric family name.
func specGather(t *testing.T, c prometheus.Collector) map[string]float64 {
	t.Helper()

	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("registering pool as prometheus.Collector failed: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics failed: %v", err)
	}

	out := map[string]float64{}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			switch {
			case m.GetCounter() != nil:
				out[mf.GetName()] += m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				out[mf.GetName()] += m.GetGauge().GetValue()
			}
		}
	}
	return out
}

// specOpenFn returns an open func handing every warmer the same Store.
func specOpenFn(logger log.Logger, store *Store) func(context.Context, storage.Repository) (WarmEnv, func(), error) {
	return func(context.Context, storage.Repository) (WarmEnv, func(), error) {
		return WarmEnv{Logger: logger, Store: store}, func() {}, nil
	}
}

// specRepoAndCommits creates a real repository with n distinct commits.
func specRepoAndCommits(t *testing.T, n int) (storage.Repository, []git.ObjectID) {
	t.Helper()

	cfg := testcfg.Build(t)
	ctx := testhelper.Context(t)
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})

	commits := make([]git.ObjectID, 0, n)
	for i := 0; i < n; i++ {
		commits = append(commits, gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithMessage(fmt.Sprintf("spec commit %d", i))))
	}
	return repo, commits
}

// specWaitFor polls cond until it is true or the deadline passes.
func specWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSpecPoolRunsRegisteredWarmersOnEnqueuedCommits(t *testing.T) {
	sfx := specSuffix()
	kind := "spec-pool-" + sfx

	var mu sync.Mutex
	seen := map[git.ObjectID]bool{}
	RegisterWarmer(kind, func(_ context.Context, env WarmEnv, commits []git.ObjectID) error {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range commits {
			seen[c] = true
			if err := env.Store.Kind(kind, 1).Put(c, []byte("computed")); err != nil {
				return err
			}
		}
		return nil
	})

	store := specStore(t)
	logger := testhelper.NewLogger(t)
	pool := NewPool(logger, 2, 100, specOpenFn(logger, store))

	ctx, cancel := context.WithCancel(testhelper.Context(t))
	defer cancel()
	go pool.Run(ctx)
	defer pool.Close()

	repo, commits := specRepoAndCommits(t, 3)
	pool.Enqueue(repo, commits)

	specWaitFor(t, "all enqueued commits to be computed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range commits {
			if !seen[c] {
				return false
			}
		}
		return true
	})

	for _, c := range commits {
		got, ok, err := store.Kind(kind, 1).Get(c)
		if err != nil || !ok || string(got) != "computed" {
			t.Fatalf("commit %s: stored value ok=%v err=%v value=%q, want the warmer's value", c, ok, err, got)
		}
	}
}

func TestSpecPoolWarmEnvCarriesStore(t *testing.T) {
	sfx := specSuffix()
	kind := "spec-env-" + sfx

	store := specStore(t)
	var mu sync.Mutex
	var gotStore *Store
	RegisterWarmer(kind, func(_ context.Context, env WarmEnv, _ []git.ObjectID) error {
		mu.Lock()
		defer mu.Unlock()
		gotStore = env.Store
		return nil
	})

	logger := testhelper.NewLogger(t)
	pool := NewPool(logger, 1, 10, specOpenFn(logger, store))
	ctx, cancel := context.WithCancel(testhelper.Context(t))
	defer cancel()
	go pool.Run(ctx)
	defer pool.Close()

	repo, commits := specRepoAndCommits(t, 1)
	pool.Enqueue(repo, commits)

	specWaitFor(t, "warmer to receive WarmEnv", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gotStore != nil
	})

	mu.Lock()
	defer mu.Unlock()
	if gotStore != store {
		t.Fatalf("WarmEnv.Store given to the warmer is not the Store the pool was opened with")
	}
}

func TestSpecPoolEnqueueNeverBlocksWhenFull(t *testing.T) {
	logger := testhelper.NewLogger(t)
	// No Run: nothing drains the queue, so it fills up immediately.
	pool := NewPool(logger, 1, 1, specOpenFn(logger, specStore(t)))
	defer pool.Close()

	repo, commits := specRepoAndCommits(t, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			pool.Enqueue(repo, commits)
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Enqueue blocked on a full queue; it must be non-blocking")
	}

	if dropped := specGather(t, pool)["graft_derived_warm_dropped_total"]; dropped <= 0 {
		t.Fatalf("graft_derived_warm_dropped_total = %v after overflowing the queue, want > 0", dropped)
	}
}

func TestSpecPoolMetricNamesExposed(t *testing.T) {
	logger := testhelper.NewLogger(t)
	pool := NewPool(logger, 1, 4, specOpenFn(logger, specStore(t)))
	defer pool.Close()

	got := specGather(t, pool)
	for _, name := range []string{
		"graft_derived_warm_queue_depth",
		"graft_derived_warm_dropped_total",
		"graft_derived_warm_computed_total",
		"graft_derived_warm_errors_total",
	} {
		if _, ok := got[name]; !ok {
			t.Fatalf("metric %q is not exposed by the warmer pool; gathered: %v", name, got)
		}
	}
}

func TestSpecPoolCountsComputedAndErrors(t *testing.T) {
	sfx := specSuffix()
	okKind := "spec-ok-" + sfx
	badKind := "spec-bad-" + sfx

	RegisterWarmer(okKind, func(context.Context, WarmEnv, []git.ObjectID) error { return nil })
	RegisterWarmer(badKind, func(context.Context, WarmEnv, []git.ObjectID) error {
		return fmt.Errorf("spec: deliberate warmer failure")
	})

	logger := testhelper.NewLogger(t)
	pool := NewPool(logger, 2, 100, specOpenFn(logger, specStore(t)))
	ctx, cancel := context.WithCancel(testhelper.Context(t))
	defer cancel()
	go pool.Run(ctx)
	defer pool.Close()

	repo, commits := specRepoAndCommits(t, 2)
	pool.Enqueue(repo, commits)

	specWaitFor(t, "computed and errors counters to move", func() bool {
		m := specGather(t, pool)
		return m["graft_derived_warm_computed_total"] >= 1 && m["graft_derived_warm_errors_total"] >= 1
	})
}

func TestSpecPoolCloseStopsCleanly(t *testing.T) {
	logger := testhelper.NewLogger(t)
	pool := NewPool(logger, 2, 10, specOpenFn(logger, specStore(t)))

	ctx, cancel := context.WithCancel(testhelper.Context(t))
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		pool.Run(ctx)
	}()

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		pool.Close()
	}()

	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatalf("Close did not return while the pool was running")
	}

	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not stop after Close")
	}
}
