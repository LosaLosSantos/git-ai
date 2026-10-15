package landlane

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// closedLoopRound submits n requests of testKey at once and waits until all of
// them have their response and the lane has been retired. Like the agents of the
// bench, every round waits for its own requests before the next one starts, so
// the queue of the lane is empty after each batch.
func closedLoopRound(t *testing.T, ctx context.Context, r *Registry[string, string], round, n int) {
	t.Helper()

	chans := make([]<-chan result, 0, n)
	for i := 0; i < n; i++ {
		chans = append(chans, submitAsync(ctx, r, testKey, fmt.Sprintf("round%d-r%d", round, i)))
	}
	for _, ch := range chans {
		require.NoError(t, requireResult(t, ch).err)
	}
	require.Eventually(t, func() bool { return r.runningLanes() == 0 }, 10*time.Second, time.Millisecond)
}

// recordingExecutor returns an executor that records the batch sizes, takes
// duration to run and echoes the requests.
func recordingExecutor(duration time.Duration, batches *[]int, mu *sync.Mutex) Executor[string, string] {
	return func(_ context.Context, _ Key, reqs []string) ([]Result[string], error) {
		mu.Lock()
		*batches = append(*batches, len(reqs))
		mu.Unlock()

		time.Sleep(duration)

		out := make([]Result[string], len(reqs))
		for i, req := range reqs {
			out[i].Value = "landed-" + req
		}
		return out, nil
	}
}

func TestClosedLoopLaneKeepsItsReducedSizeAcrossRetirement(t *testing.T) {
	ctx := testhelper.Context(t)

	var mu sync.Mutex
	var batches []int
	// Every batch takes 80ms, well above the target of 20ms, so every batch halves
	// the effective max size of the key. The window is long: a batch is cut by
	// the effective max size, never by the window.
	exec := recordingExecutor(80*time.Millisecond, &batches, &mu)

	obs := NewPrometheusObserver()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(obs))

	r := newRegistry(t, Config{Enabled: true, Window: 5 * time.Second, MaxSize: 4, TargetLatency: 20 * time.Millisecond}, exec, obs)

	// Round 1 starts at MaxSize 4 and ends at 2. Round 2 starts at 2 (the lane is
	// new, but the size of the key is kept) and ends at 1. Round 3 stays at 1.
	for round, wantGauge := range []float64{2, 1, 1} {
		closedLoopRound(t, ctx, r, round+1, 4)

		require.Equal(t, wantGauge, gatherValues(t, reg)["graft_land_lane_effective_max_size"],
			"the gauge counts the size kept for the key after round %d", round+1)
	}

	mu.Lock()
	defer mu.Unlock()

	require.Equal(t, []int{4, 2, 1, 1, 1, 1, 1, 1}, batches,
		"every round is limited by the size kept from the rounds before, not by MaxSize")
}

func TestClosedLoopKeyThatGrowsBackToMaxSizeStillCounts(t *testing.T) {
	ctx := testhelper.Context(t)

	var mu sync.Mutex
	var batches []int

	obs := NewPrometheusObserver()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(obs))

	// The first round is slow and halves the size from 2 to 1. The second round is
	// fast and grows it back to MaxSize. The gauge follows the size of the key up:
	// a key that recovered does not drop out of it.
	slow := recordingExecutor(200*time.Millisecond, &batches, &mu)
	fast := recordingExecutor(0, &batches, &mu)

	var exec switchableExecutor
	exec.set(slow)
	r := newRegistry(t, Config{Enabled: true, Window: 5 * time.Second, MaxSize: 2, TargetLatency: 50 * time.Millisecond}, exec.run, obs)

	closedLoopRound(t, ctx, r, 1, 2)
	require.Equal(t, 1.0, gatherValues(t, reg)["graft_land_lane_effective_max_size"],
		"the size of one is kept after the slow round")

	exec.set(fast)
	closedLoopRound(t, ctx, r, 2, 2)
	require.Equal(t, 2.0, gatherValues(t, reg)["graft_land_lane_effective_max_size"],
		"a key that grew back to MaxSize counts with MaxSize")

	r.mu.Lock()
	defer r.mu.Unlock()
	require.Len(t, r.keys, 1)
	require.Equal(t, 2, r.keys[testKey].size)
}

// switchableExecutor switches the executor of a registry between rounds.
type switchableExecutor struct {
	mu   sync.Mutex
	exec Executor[string, string]
}

func (e *switchableExecutor) set(exec Executor[string, string]) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.exec = exec
}

func (e *switchableExecutor) run(ctx context.Context, key Key, reqs []string) ([]Result[string], error) {
	e.mu.Lock()
	exec := e.exec
	e.mu.Unlock()

	return exec(ctx, key, reqs)
}
