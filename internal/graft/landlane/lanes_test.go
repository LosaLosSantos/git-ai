package landlane

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

var testKey = Key{Storage: "default", RelativePath: "repo.git", Branch: "main"}

// recorder remembers the requests of every executed batch.
type recorder struct {
	mu      sync.Mutex
	batches [][]string
}

func (r *recorder) record(reqs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.batches = append(r.batches, append([]string(nil), reqs...))
}

func (r *recorder) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([][]string(nil), r.batches...)
}

// echoExecutor lands every request and records the batches. If gate is not nil,
// every execution signals entered and then waits until gate is closed.
func echoExecutor(rec *recorder, gate, entered chan struct{}) Executor[string, string] {
	return func(_ context.Context, _ Key, reqs []string) ([]Result[string], error) {
		if entered != nil {
			entered <- struct{}{}
		}
		if gate != nil {
			<-gate
		}

		rec.record(reqs)

		out := make([]Result[string], len(reqs))
		for i, req := range reqs {
			out[i].Value = "landed-" + req
		}

		return out, nil
	}
}

type countingObserver struct {
	mu      sync.Mutex
	batches int
	waited  int
}

func (o *countingObserver) BatchExecuted(Key, int, time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.batches++
}

func (o *countingObserver) RequestWaited(Key, time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.waited++
}

func (o *countingObserver) QueueDepth(Key, int) {}

func (o *countingObserver) MaxSizeChanged(Key, int) {}

func newRegistry(t *testing.T, cfg Config, exec Executor[string, string], obs Observer) *Registry[string, string] {
	t.Helper()

	r, err := NewRegistry(cfg, exec, obs)
	require.NoError(t, err)

	return r
}

type result struct {
	resp Response[string]
	err  error
}

func submitAsync(ctx context.Context, r *Registry[string, string], key Key, req string) <-chan result {
	ch := make(chan result, 1)
	go func() {
		resp, err := r.Submit(ctx, key, req)
		ch <- result{resp: resp, err: err}
	}()

	return ch
}

func requireResult(t *testing.T, ch <-chan result) result {
	t.Helper()

	select {
	case res := <-ch:
		return res
	case <-time.After(10 * time.Second):
		require.FailNow(t, "timed out waiting for Submit to return")
	}

	return result{}
}

// queueLen returns the number of queued requests of testKey.
func queueLen(r *Registry[string, string]) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	l := r.lanes[testKey]
	if l == nil {
		return 0
	}

	return len(l.queue)
}

// unbatched returns a Config with batching enabled and no collection window.
func unbatched(maxSize int) Config {
	return Config{Enabled: true, Window: 0, MaxSize: maxSize}
}

func TestBatchCollectsRequestsWithinWindow(t *testing.T) {
	ctx := testhelper.Context(t)
	rec := &recorder{}
	obs := &countingObserver{}
	r := newRegistry(t, Config{Enabled: true, Window: 2 * time.Second, MaxSize: 50}, echoExecutor(rec, nil, nil), obs)

	var chans []<-chan result
	for i := range 10 {
		chans = append(chans, submitAsync(ctx, r, testKey, fmt.Sprintf("req-%d", i)))
	}

	positions := map[int]bool{}
	for _, ch := range chans {
		res := requireResult(t, ch)
		require.NoError(t, res.err)
		require.Equal(t, 10, res.resp.BatchSize)
		require.False(t, positions[res.resp.BatchPosition], "duplicate batch position")
		positions[res.resp.BatchPosition] = true
	}
	require.Len(t, positions, 10)

	require.Len(t, rec.snapshot(), 1)
	require.Len(t, rec.snapshot()[0], 10)

	obs.mu.Lock()
	defer obs.mu.Unlock()
	require.Equal(t, 1, obs.batches)
	require.Equal(t, 10, obs.waited)
}

func TestMaxSizeClosesBatchEarly(t *testing.T) {
	ctx := testhelper.Context(t)
	rec := &recorder{}
	r := newRegistry(t, Config{Enabled: true, Window: time.Hour, MaxSize: 3}, echoExecutor(rec, nil, nil), nil)

	var chans []<-chan result
	for i := range 3 {
		chans = append(chans, submitAsync(ctx, r, testKey, fmt.Sprintf("req-%d", i)))
	}

	for _, ch := range chans {
		res := requireResult(t, ch)
		require.NoError(t, res.err)
		require.Equal(t, 3, res.resp.BatchSize)
	}

	require.Len(t, rec.snapshot(), 1)
}

func TestQueuedRequestsAreSplitByMaxSize(t *testing.T) {
	ctx := testhelper.Context(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 10)
	rec := &recorder{}
	r := newRegistry(t, unbatched(2), echoExecutor(rec, gate, entered), nil)

	first := submitAsync(ctx, r, testKey, "r0")
	<-entered

	var chans []<-chan result
	for i := 1; i <= 5; i++ {
		chans = append(chans, submitAsync(ctx, r, testKey, fmt.Sprintf("r%d", i)))
		require.Eventually(t, func() bool { return queueLen(r) == i }, 10*time.Second, time.Millisecond)
	}

	close(gate)

	require.NoError(t, requireResult(t, first).err)
	for _, ch := range chans {
		require.NoError(t, requireResult(t, ch).err)
	}

	require.Equal(t, [][]string{{"r0"}, {"r1", "r2"}, {"r3", "r4"}, {"r5"}}, rec.snapshot())
}

func TestRequestsAreExecutedInFIFOOrder(t *testing.T) {
	ctx := testhelper.Context(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 10)
	rec := &recorder{}
	r := newRegistry(t, unbatched(50), echoExecutor(rec, gate, entered), nil)

	first := submitAsync(ctx, r, testKey, "r0")
	<-entered

	var chans []<-chan result
	for i := 1; i <= 5; i++ {
		chans = append(chans, submitAsync(ctx, r, testKey, fmt.Sprintf("r%d", i)))
		require.Eventually(t, func() bool { return queueLen(r) == i }, 10*time.Second, time.Millisecond)
	}

	close(gate)

	require.NoError(t, requireResult(t, first).err)
	for _, ch := range chans {
		require.NoError(t, requireResult(t, ch).err)
	}

	require.Equal(t, [][]string{{"r0"}, {"r1", "r2", "r3", "r4", "r5"}}, rec.snapshot())
}

func TestCancelledQueuedRequestIsDropped(t *testing.T) {
	ctx := testhelper.Context(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 10)
	rec := &recorder{}
	r := newRegistry(t, unbatched(50), echoExecutor(rec, gate, entered), nil)

	first := submitAsync(ctx, r, testKey, "r0")
	<-entered

	cancelCtx, cancel := context.WithCancel(ctx)
	cancelled := submitAsync(cancelCtx, r, testKey, "r1")
	require.Eventually(t, func() bool { return queueLen(r) == 1 }, 10*time.Second, time.Millisecond)

	cancel()
	res := requireResult(t, cancelled)
	require.ErrorIs(t, res.err, context.Canceled)
	require.Equal(t, 0, queueLen(r))

	close(gate)
	require.NoError(t, requireResult(t, first).err)

	second := submitAsync(ctx, r, testKey, "r2")
	require.NoError(t, requireResult(t, second).err)

	require.Equal(t, [][]string{{"r0"}, {"r2"}}, rec.snapshot())
}

func TestCancelDuringExecutionReturnsImmediatelyAndStillLands(t *testing.T) {
	ctx := testhelper.Context(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 10)
	ctxErrs := make(chan error, 1)
	rec := &recorder{}

	exec := func(execCtx context.Context, _ Key, reqs []string) ([]Result[string], error) {
		entered <- struct{}{}
		<-gate

		ctxErrs <- execCtx.Err()
		rec.record(reqs)

		return []Result[string]{{Value: "landed"}}, nil
	}
	r := newRegistry(t, unbatched(50), exec, nil)

	cancelCtx, cancel := context.WithCancel(ctx)
	cancelled := submitAsync(cancelCtx, r, testKey, "r0")
	<-entered

	cancel()
	res := requireResult(t, cancelled)
	require.Equal(t, context.Canceled, res.err)

	close(gate)
	require.NoError(t, <-ctxErrs, "the batch context must not be cancelled by the submitter")
	require.Eventually(t, func() bool { return len(rec.snapshot()) == 1 }, 10*time.Second, time.Millisecond)
	require.Equal(t, [][]string{{"r0"}}, rec.snapshot())
}

func TestPerRequestAndBatchErrors(t *testing.T) {
	ctx := testhelper.Context(t)
	boom := errors.New("boom")

	t.Run("per request error", func(t *testing.T) {
		exec := func(_ context.Context, _ Key, reqs []string) ([]Result[string], error) {
			out := make([]Result[string], len(reqs))
			for i, req := range reqs {
				if req == "bad" {
					out[i].Err = boom
				} else {
					out[i].Value = "landed-" + req
				}
			}
			return out, nil
		}
		r := newRegistry(t, Config{Enabled: true, Window: 2 * time.Second, MaxSize: 50}, exec, nil)

		good := submitAsync(ctx, r, testKey, "ok")
		bad := submitAsync(ctx, r, testKey, "bad")

		goodRes := requireResult(t, good)
		require.NoError(t, goodRes.err)
		require.Equal(t, "landed-ok", goodRes.resp.Value)
		require.Equal(t, 2, goodRes.resp.BatchSize)

		badRes := requireResult(t, bad)
		require.ErrorIs(t, badRes.err, boom)
		require.Equal(t, 2, badRes.resp.BatchSize)
	})

	t.Run("batch error fails every request", func(t *testing.T) {
		exec := func(context.Context, Key, []string) ([]Result[string], error) {
			return nil, boom
		}
		r := newRegistry(t, unbatched(50), exec, nil)

		_, err := r.Submit(ctx, testKey, "r0")
		require.ErrorIs(t, err, boom)
	})

	t.Run("executor forgets requests", func(t *testing.T) {
		exec := func(context.Context, Key, []string) ([]Result[string], error) {
			return nil, nil
		}
		r := newRegistry(t, unbatched(50), exec, nil)

		_, err := r.Submit(ctx, testKey, "r0")
		require.ErrorIs(t, err, ErrNotCompleted)
	})
}

func TestPanicFailsEveryRequestOfTheBatch(t *testing.T) {
	ctx := testhelper.Context(t)
	boom := errors.New("boom")
	calls := 0
	exec := func(_ context.Context, _ Key, reqs []string) ([]Result[string], error) {
		calls++
		if calls == 1 {
			panic(boom)
		}
		out := make([]Result[string], len(reqs))
		for i, req := range reqs {
			out[i].Value = "landed-" + req
		}
		return out, nil
	}
	r := newRegistry(t, Config{Enabled: true, Window: 2 * time.Second, MaxSize: 50}, exec, nil)

	var chans []<-chan result
	for i := range 3 {
		chans = append(chans, submitAsync(ctx, r, testKey, fmt.Sprintf("r%d", i)))
	}

	for _, ch := range chans {
		res := requireResult(t, ch)
		require.ErrorContains(t, res.err, "panicked")
		require.ErrorIs(t, res.err, boom, "the panic value must stay wrapped")
		require.Equal(t, 3, res.resp.BatchSize)
	}

	resp, err := r.Submit(ctx, testKey, "next")
	require.NoError(t, err, "the lane must keep working after a panic")
	require.Equal(t, "landed-next", resp.Value)
}

func TestPanicWithNonErrorValue(t *testing.T) {
	ctx := testhelper.Context(t)
	exec := func(context.Context, Key, []string) ([]Result[string], error) {
		panic("kaboom")
	}
	r := newRegistry(t, unbatched(50), exec, nil)

	_, err := r.Submit(ctx, testKey, "r0")
	require.ErrorContains(t, err, "panicked: kaboom")
}

func TestDisabledBatchingExecutesEachRequestAlone(t *testing.T) {
	ctx := testhelper.Context(t)
	rec := &recorder{}
	r := newRegistry(t, Config{Enabled: false, Window: time.Hour, MaxSize: 50}, echoExecutor(rec, nil, nil), nil)

	for i := range 3 {
		resp, err := r.Submit(ctx, testKey, fmt.Sprintf("r%d", i))
		require.NoError(t, err)
		require.Equal(t, 1, resp.BatchSize)
		require.Equal(t, 0, resp.BatchPosition)
	}

	require.Equal(t, [][]string{{"r0"}, {"r1"}, {"r2"}}, rec.snapshot())
}

func TestDifferentLanesAreIndependent(t *testing.T) {
	ctx := testhelper.Context(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 10)

	exec := func(_ context.Context, key Key, reqs []string) ([]Result[string], error) {
		if key.Branch == "blocked" {
			entered <- struct{}{}
			<-gate
		}

		out := make([]Result[string], len(reqs))
		for i, req := range reqs {
			out[i].Value = "landed-" + req
		}
		return out, nil
	}
	r := newRegistry(t, unbatched(50), exec, nil)

	blockedKey := Key{Storage: "default", RelativePath: "repo.git", Branch: "blocked"}
	blocked := submitAsync(ctx, r, blockedKey, "r0")
	<-entered

	other := submitAsync(ctx, r, Key{Storage: "default", RelativePath: "repo.git", Branch: "other"}, "r1")
	require.NoError(t, requireResult(t, other).err)

	close(gate)
	require.NoError(t, requireResult(t, blocked).err)
}

func TestCloseFailsQueuedRequestsAndWaitsForRunningBatch(t *testing.T) {
	ctx := testhelper.Context(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 10)
	rec := &recorder{}
	r := newRegistry(t, unbatched(50), echoExecutor(rec, gate, entered), nil)

	first := submitAsync(ctx, r, testKey, "r0")
	<-entered

	second := submitAsync(ctx, r, testKey, "r1")
	require.Eventually(t, func() bool { return queueLen(r) == 1 }, 10*time.Second, time.Millisecond)

	closed := make(chan struct{})
	go func() {
		r.Close()
		close(closed)
	}()

	require.ErrorIs(t, requireResult(t, second).err, ErrClosed)

	select {
	case <-closed:
		require.FailNow(t, "Close returned while a batch was still running")
	default:
	}

	close(gate)

	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "Close did not return after the running batch completed")
	}

	require.NoError(t, requireResult(t, first).err)
	require.Equal(t, [][]string{{"r0"}}, rec.snapshot())

	_, err := r.Submit(ctx, testKey, "r2")
	require.ErrorIs(t, err, ErrClosed)

	r.Close()
}

func TestSubmitAfterCloseReturnsErrClosed(t *testing.T) {
	ctx := testhelper.Context(t)

	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			rec := &recorder{}
			r := newRegistry(t, Config{Enabled: enabled, Window: 0, MaxSize: 50}, echoExecutor(rec, nil, nil), nil)

			r.Close()

			_, err := r.Submit(ctx, testKey, "r0")
			require.ErrorIs(t, err, ErrClosed)
			require.Empty(t, rec.snapshot(), "a closed registry must not execute requests")
		})
	}
}

func TestIdleLaneIsRetired(t *testing.T) {
	ctx := testhelper.Context(t)
	r := newRegistry(t, unbatched(50), echoExecutor(&recorder{}, nil, nil), nil)

	require.NoError(t, requireResult(t, submitAsync(ctx, r, testKey, "r0")).err)
	require.Eventually(t, func() bool { return r.runningLanes() == 0 }, 10*time.Second, time.Millisecond)
}

func TestNoGoroutineLeakAfterIdle(t *testing.T) {
	ctx := testhelper.Context(t)
	r := newRegistry(t, unbatched(50), echoExecutor(&recorder{}, nil, nil), nil)

	before := runtime.NumGoroutine()

	var chans []<-chan result
	for i := range 20 {
		key := Key{Storage: "default", RelativePath: "repo.git", Branch: fmt.Sprintf("branch-%d", i)}
		chans = append(chans, submitAsync(ctx, r, key, fmt.Sprintf("r%d", i)))
	}
	for _, ch := range chans {
		require.NoError(t, requireResult(t, ch).err)
	}

	require.Eventually(t, func() bool { return r.runningLanes() == 0 }, 10*time.Second, time.Millisecond)

	// require.Eventually runs its condition in a goroutine of its own, so the
	// goroutine count is polled by hand here.
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.LessOrEqual(t, runtime.NumGoroutine(), before, "lane goroutines must exit once their lanes are idle")
}

func TestNewRegistryRejectsInvalidConfig(t *testing.T) {
	exec := echoExecutor(&recorder{}, nil, nil)

	_, err := NewRegistry(Config{Enabled: true, Window: -time.Second, MaxSize: 1}, exec, nil)
	require.ErrorContains(t, err, "window must not be negative")

	_, err = NewRegistry(Config{Enabled: true, Window: 0, MaxSize: 0}, exec, nil)
	require.ErrorContains(t, err, "max_size must be at least 1")

	_, err = NewRegistry[string, string](unbatched(1), nil, nil)
	require.Error(t, err)
}

func TestPartialResultsCompleteOnlyTheMissingRequests(t *testing.T) {
	ctx := testhelper.Context(t)

	// The executor returns a result for the first request of the batch only.
	exec := func(_ context.Context, _ Key, reqs []string) ([]Result[string], error) {
		return []Result[string]{{Value: "landed-" + reqs[0]}}, nil
	}
	r := newRegistry(t, Config{Enabled: true, Window: 2 * time.Second, MaxSize: 2}, exec, nil)

	first := submitAsync(ctx, r, testKey, "r0")
	require.Eventually(t, func() bool { return queueLen(r) == 1 }, 10*time.Second, time.Millisecond)
	second := submitAsync(ctx, r, testKey, "r1")

	results := []result{requireResult(t, first), requireResult(t, second)}

	var landed, notCompleted int
	for _, res := range results {
		require.Equal(t, 2, res.resp.BatchSize)

		switch {
		case res.err == nil:
			require.Equal(t, 0, res.resp.BatchPosition, "only the first request of the batch has a result")
			require.Equal(t, "landed-r0", res.resp.Value, "the result must belong to its own request")
			landed++
		case errors.Is(res.err, ErrNotCompleted):
			require.Equal(t, 1, res.resp.BatchPosition)
			notCompleted++
		default:
			require.FailNow(t, "unexpected error", res.err.Error())
		}
	}

	require.Equal(t, 1, landed)
	require.Equal(t, 1, notCompleted)
}

func TestTooManyResultsFailTheWholeBatch(t *testing.T) {
	ctx := testhelper.Context(t)

	exec := func(_ context.Context, _ Key, reqs []string) ([]Result[string], error) {
		return make([]Result[string], len(reqs)+2), nil
	}
	r := newRegistry(t, unbatched(50), exec, nil)

	_, err := r.Submit(ctx, testKey, "r0")
	require.ErrorContains(t, err, "executor returned 3 results for 1 requests")
	require.NotErrorIs(t, err, ErrNotCompleted)
}

func TestBatchWindowIsMeasuredFromTheFirstQueuedRequest(t *testing.T) {
	ctx := testhelper.Context(t)
	const window = 300 * time.Millisecond

	gate := make(chan struct{})
	entered := make(chan struct{}, 10)
	starts := make(chan time.Time, 10)
	var mu sync.Mutex
	calls := 0

	exec := func(_ context.Context, _ Key, reqs []string) ([]Result[string], error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()

		if first {
			entered <- struct{}{}
			<-gate
		}

		starts <- time.Now()

		out := make([]Result[string], len(reqs))
		for i, req := range reqs {
			out[i].Value = "landed-" + req
		}
		return out, nil
	}
	r := newRegistry(t, Config{Enabled: true, Window: window, MaxSize: 50}, exec, nil)

	first := submitAsync(ctx, r, testKey, "r0")
	<-entered

	// The second request is queued while the first batch runs, and it has
	// already waited longer than Window when the first batch completes. Its
	// batch must not wait for another full Window.
	second := submitAsync(ctx, r, testKey, "r1")
	require.Eventually(t, func() bool { return queueLen(r) == 1 }, 10*time.Second, time.Millisecond)
	time.Sleep(2 * window)

	close(gate)

	require.NoError(t, requireResult(t, first).err)
	require.NoError(t, requireResult(t, second).err)

	end := <-starts
	start := <-starts
	require.Less(t, start.Sub(end), window/2, "back-to-back batches must not leave an idle window between them")
}

func TestDisabledBatchingCancelAfterHandOffReturnsImmediately(t *testing.T) {
	ctx := testhelper.Context(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	exec := func(context.Context, Key, []string) ([]Result[string], error) {
		entered <- struct{}{}
		<-gate
		return []Result[string]{{Value: "landed"}}, nil
	}
	r := newRegistry(t, Config{Enabled: false, MaxSize: 1}, exec, nil)

	cancelCtx, cancel := context.WithCancel(ctx)
	cancelled := submitAsync(cancelCtx, r, testKey, "r0")
	<-entered

	cancel()
	require.ErrorIs(t, requireResult(t, cancelled).err, context.Canceled)

	close(gate)
}

func TestZeroConfigDoesNotBatch(t *testing.T) {
	ctx := testhelper.Context(t)
	r := newRegistry(t, Config{Window: time.Hour, MaxSize: 50}, echoExecutor(&recorder{}, nil, nil), nil)

	// A Config without Enabled is disabled, so the request runs at once instead
	// of waiting an hour for a batch.
	resp, err := r.Submit(ctx, testKey, "r0")
	require.NoError(t, err)
	require.Equal(t, 1, resp.BatchSize)
}

func TestConfigFromGraft(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := ConfigFromGraft(config.GraftLandBatch{})
		require.NoError(t, err)
		require.Equal(t, Config{
			Enabled:       true,
			Window:        config.DefaultGraftLandBatchWindow,
			MaxSize:       config.DefaultGraftLandBatchMaxSize,
			TargetLatency: config.DefaultGraftLandBatchTargetLatency,
		}, cfg)
	})

	t.Run("explicit values", func(t *testing.T) {
		cfg, err := ConfigFromGraft(config.GraftLandBatch{Window: "25ms", MaxSize: 7, TargetLatency: "2s"})
		require.NoError(t, err)
		require.Equal(t, Config{Enabled: true, Window: 25 * time.Millisecond, MaxSize: 7, TargetLatency: 2 * time.Second}, cfg)
	})

	t.Run("adaptation disabled", func(t *testing.T) {
		cfg, err := ConfigFromGraft(config.GraftLandBatch{TargetLatency: "0s"})
		require.NoError(t, err)
		require.Equal(t, time.Duration(0), cfg.TargetLatency)
	})

	t.Run("invalid target latency", func(t *testing.T) {
		_, err := ConfigFromGraft(config.GraftLandBatch{TargetLatency: "fast"})
		require.ErrorContains(t, err, "invalid target_latency")
	})

	t.Run("disabled", func(t *testing.T) {
		cfg, err := ConfigFromGraft(config.GraftLandBatch{Disabled: true, Window: "0s"})
		require.NoError(t, err)
		require.False(t, cfg.Enabled)
		require.Equal(t, time.Duration(0), cfg.Window)
	})

	t.Run("invalid window", func(t *testing.T) {
		_, err := ConfigFromGraft(config.GraftLandBatch{Window: "soon"})
		require.ErrorContains(t, err, "invalid window")
	})
}
