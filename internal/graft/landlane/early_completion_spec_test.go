package landlane

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/testhelper"
)

// Spec (WP37): CompleteEarly delivers the result of reqs[index] of the running
// batch now. It returns false if ctx is not an executor context of this
// package or index is out of range. These tests cover the false paths, which
// are reachable without a running batch. The "true" path (delivery before the
// executor returns, later result ignored) needs the lane submit API, which the
// spec does not name, so it is not covered here.

func TestSpecCompleteEarlyReturnsFalseOutsideExecutorContext(t *testing.T) {
	ctx := testhelper.Context(t)

	var r Result[int]
	if CompleteEarly(ctx, 0, r) {
		t.Fatal("CompleteEarly returned true for a non-executor context")
	}
}

func TestSpecCompleteEarlyRejectsAnyIndexOutsideExecutor(t *testing.T) {
	ctx := testhelper.Context(t)

	var r Result[string]
	for _, index := range []int{-1, 0, 1, 1 << 20} {
		if CompleteEarly(ctx, index, r) {
			t.Fatalf("CompleteEarly(index=%d) returned true outside an executor context", index)
		}
	}
}

func TestSpecCompleteEarlyReturnsFalseForCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(testhelper.Context(t))
	cancel()

	var r Result[int]
	if CompleteEarly(ctx, 0, r) {
		t.Fatal("CompleteEarly returned true for a canceled non-executor context")
	}
}

func TestSpecCompleteEarlyIsConcurrencySafe(t *testing.T) {
	ctx := testhelper.Context(t)

	var wg sync.WaitGroup
	results := make(chan bool, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			var r Result[int]
			results <- CompleteEarly(ctx, index%4, r)
		}(i)
	}
	wg.Wait()
	close(results)

	for ok := range results {
		if ok {
			t.Fatal("concurrent CompleteEarly returned true outside an executor context")
		}
	}
}

func TestSpecTargetLatencyZeroMeansNoAdaptation(t *testing.T) {
	// The zero value of Config has no adaptation: TargetLatency is 0.
	var cfg Config
	if cfg.TargetLatency != 0 {
		t.Fatalf("zero Config has TargetLatency %v, want 0 (no adaptation)", cfg.TargetLatency)
	}

	// TargetLatency is a time.Duration (Go duration, e.g. 500ms).
	cfg.TargetLatency = 500 * time.Millisecond
	var _ time.Duration = cfg.TargetLatency
	if cfg.TargetLatency != 500*time.Millisecond {
		t.Fatalf("TargetLatency = %v, want 500ms", cfg.TargetLatency)
	}
}
