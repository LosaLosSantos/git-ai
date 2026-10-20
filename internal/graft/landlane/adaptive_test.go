package landlane

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// fakeClock only moves when the test advances it. The registry measures the
// duration of the executor calls with it, so the duration of every batch is
// the one the executor declares.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_000_000, 0)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

// sizeObserver records the effective max sizes reported for the lanes.
type sizeObserver struct {
	NopObserver

	mu    sync.Mutex
	sizes []int
}

func (o *sizeObserver) MaxSizeChanged(_ Key, size int) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.sizes = append(o.sizes, size)
}

func (o *sizeObserver) snapshot() []int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return append([]int(nil), o.sizes...)
}

func TestNextMaxSize(t *testing.T) {
	const target = 100 * time.Millisecond

	for _, tc := range []struct {
		desc    string
		current int
		maxSize int
		took    time.Duration
		target  time.Duration
		want    int
	}{
		{desc: "a slow batch halves the size", current: 16, maxSize: 16, took: 200 * time.Millisecond, target: target, want: 8},
		{desc: "the size is never halved below one", current: 1, maxSize: 16, took: 200 * time.Millisecond, target: target, want: 1},
		{desc: "a fast batch grows the size by a quarter", current: 8, maxSize: 16, took: 10 * time.Millisecond, target: target, want: 10},
		{desc: "growth is at least one", current: 2, maxSize: 16, took: 0, target: target, want: 3},
		{desc: "growth is capped at the maximum", current: 15, maxSize: 16, took: 0, target: target, want: 16},
		{desc: "a batch between half the target and the target keeps the size", current: 8, maxSize: 16, took: 70 * time.Millisecond, target: target, want: 8},
		{desc: "a batch exactly at the target keeps the size", current: 8, maxSize: 16, took: target, target: target, want: 8},
		{desc: "a batch exactly at half the target keeps the size", current: 8, maxSize: 16, took: target / 2, target: target, want: 8},
		{desc: "a zero target never adapts", current: 8, maxSize: 16, took: time.Hour, target: 0, want: 8},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.want, nextMaxSize(tc.current, tc.maxSize, tc.took, tc.target))
		})
	}
}

// adaptiveRun submits requests to a lane whose first batch is held by the
// executor until every request is queued. Every executor call advances the fake
// clock by the next duration of durations (zero after the list ends). It returns
// the batch sizes the executor saw and the effective max sizes the observer saw
// once the lane was retired.
func adaptiveRun(t *testing.T, cfg Config, requests int, durations []time.Duration) ([]int, []int) {
	t.Helper()

	ctx := testhelper.Context(t)
	clock := newFakeClock()
	obs := &sizeObserver{}
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)

	var mu sync.Mutex
	var batches []int
	calls := 0
	exec := func(_ context.Context, _ Key, reqs []string) ([]Result[string], error) {
		mu.Lock()
		calls++
		call := calls
		batches = append(batches, len(reqs))
		mu.Unlock()

		if call == 1 {
			entered <- struct{}{}
			<-gate
		}

		var took time.Duration
		if call <= len(durations) {
			took = durations[call-1]
		}
		clock.advance(took)

		out := make([]Result[string], len(reqs))
		for i, req := range reqs {
			out[i].Value = "landed-" + req
		}
		return out, nil
	}

	cfg.Clock = clock.now
	r := newRegistry(t, cfg, exec, obs)

	chans := []<-chan result{submitAsync(ctx, r, testKey, "r0")}
	<-entered
	for i := 1; i < requests; i++ {
		chans = append(chans, submitAsync(ctx, r, testKey, fmt.Sprintf("r%d", i)))
		require.Eventually(t, func() bool { return queueLen(r) == i }, 10*time.Second, time.Millisecond)
	}

	close(gate)
	for _, ch := range chans {
		require.NoError(t, requireResult(t, ch).err)
	}
	require.Eventually(t, func() bool { return r.runningLanes() == 0 }, 10*time.Second, time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	return batches, obs.snapshot()
}

func TestAdaptiveSizeHalvesOnSlowBatchesAndGrowsOnFastOnes(t *testing.T) {
	cfg := Config{Enabled: true, Window: 0, MaxSize: 16, TargetLatency: 100 * time.Millisecond}

	// The first batch holds one request and is slow, which halves the size of
	// the lane from 16 to 8. The second batch is slow as well: 4. Then the batches
	// are fast, and the size grows back by a quarter (at least one) per batch,
	// until the queue is drained.
	durations := []time.Duration{
		200 * time.Millisecond, 200 * time.Millisecond,
		0, 0, 0, 0, 0, 0, 0,
	}

	batches, sizes := adaptiveRun(t, cfg, 40, durations)

	require.Equal(t, []int{1, 8, 4, 5, 6, 7, 8, 1}, batches, "batches are limited by the effective max size")
	require.Equal(t, []int{16, 8, 4, 5, 6, 7, 8, 10, 12, 12}, sizes,
		"the lane starts at MaxSize, adapts after each batch and reports the size the key keeps when it is retired")
}

func TestAdaptiveSizeNeverDropsBelowOne(t *testing.T) {
	cfg := Config{Enabled: true, Window: 0, MaxSize: 4, TargetLatency: 100 * time.Millisecond}

	// Every batch is slow, so the size shrinks from 4 to 2 and then to 1. It never
	// goes below one, so the later batches hold one request each.
	slow := make([]time.Duration, 12)
	for i := range slow {
		slow[i] = 200 * time.Millisecond
	}

	batches, sizes := adaptiveRun(t, cfg, 12, slow)

	require.Equal(t, []int{1, 2, 1, 1, 1, 1, 1, 1, 1, 1, 1}, batches)
	require.Equal(t, []int{4, 2, 1, 1}, sizes, "the size of one is kept when the lane is retired")
}

func TestAdaptiveSizeDisabledWithZeroTarget(t *testing.T) {
	cfg := Config{Enabled: true, Window: 0, MaxSize: 4, TargetLatency: 0}

	batches, sizes := adaptiveRun(t, cfg, 10, []time.Duration{time.Hour, time.Hour, time.Hour})

	require.Equal(t, []int{1, 4, 4, 1}, batches, "every batch uses MaxSize")
	require.Equal(t, []int{4, 4}, sizes, "no size change is reported: the retired lane keeps MaxSize for the key")
}

func TestPrometheusObserverSumsEffectiveMaxSizeOverLanes(t *testing.T) {
	obs := NewPrometheusObserver()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(obs))

	obs.MaxSizeChanged(Key{Branch: "a"}, 50)
	obs.MaxSizeChanged(Key{Branch: "b"}, 25)
	require.Equal(t, 75.0, gatherValues(t, reg)["graft_land_lane_effective_max_size"])

	obs.MaxSizeChanged(Key{Branch: "a"}, 12)
	require.Equal(t, 37.0, gatherValues(t, reg)["graft_land_lane_effective_max_size"])

	obs.MaxSizeChanged(Key{Branch: "a"}, 0)
	require.Equal(t, 25.0, gatherValues(t, reg)["graft_land_lane_effective_max_size"])
}
