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

const effectiveMaxSizeGauge = "graft_land_lane_effective_max_size"

// clockExecutor echoes the requests and advances the fake clock by the duration
// it is set to, so that the registry measures that duration for every batch.
type clockExecutor struct {
	clock *fakeClock

	mu   sync.Mutex
	took time.Duration
}

func (e *clockExecutor) set(took time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.took = took
}

func (e *clockExecutor) run(_ context.Context, _ Key, reqs []string) ([]Result[string], error) {
	e.mu.Lock()
	took := e.took
	e.mu.Unlock()

	e.clock.advance(took)

	out := make([]Result[string], len(reqs))
	for i, req := range reqs {
		out[i].Value = "landed-" + req
	}
	return out, nil
}

// keyStateRegistry returns a registry with MaxSize 16 and a target latency of
// 100ms, whose batches take the duration of exec, measured on clock.
func keyStateRegistry(t *testing.T, exec *clockExecutor, obs Observer) *Registry[string, string] {
	t.Helper()

	r := newRegistry(t, Config{Enabled: true, MaxSize: 16, TargetLatency: 100 * time.Millisecond}, exec.run, obs)
	r.now = exec.clock.now
	t.Cleanup(r.Close)

	return r
}

// submitAndRetire lands one request on key and waits until every lane of r is
// retired.
func submitAndRetire(t *testing.T, ctx context.Context, r *Registry[string, string], key Key) {
	t.Helper()

	_, err := r.Submit(ctx, key, "r")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return r.runningLanes() == 0 }, 10*time.Second, time.Millisecond)
}

func keptKeys(r *Registry[string, string]) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.keys)
}

func repoKey(i int) Key {
	return Key{Storage: "default", RelativePath: fmt.Sprintf("repo-%d.git", i), Branch: "main"}
}

// TestIdleKeysAreForgottenAfterKeyStateTTL is the regression test for keys that
// were slowed down once and never used again: their reduced size was kept
// forever, in the registry and in the gauge.
func TestIdleKeysAreForgottenAfterKeyStateTTL(t *testing.T) {
	ctx := testhelper.Context(t)

	exec := &clockExecutor{clock: newFakeClock()}
	exec.set(200 * time.Millisecond)

	obs := NewPrometheusObserver()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(obs))

	r := keyStateRegistry(t, exec, obs)

	// Every key has one slow batch, which halves its size from 16 to 8. All of
	// them land within KeyStateTTL (1000 * 200ms), so all of them are kept.
	const keys = 1000
	for i := 0; i < keys; i++ {
		submitAndRetire(t, ctx, r, repoKey(i))
	}
	require.Equal(t, keys, keptKeys(r))
	require.Equal(t, float64(keys*8), gatherValues(t, reg)[effectiveMaxSizeGauge])

	// Once they have been idle for KeyStateTTL, the next request, on another key,
	// forgets all of them. Only the new key is left.
	exec.clock.advance(KeyStateTTL)
	exec.set(0)
	submitAndRetire(t, ctx, r, Key{Storage: "default", RelativePath: "fresh.git", Branch: "main"})

	require.Equal(t, 1, keptKeys(r))
	require.Equal(t, 16.0, gatherValues(t, reg)[effectiveMaxSizeGauge], "only the fresh key counts, with MaxSize")

	r.mu.Lock()
	for key := range r.keys {
		require.Equal(t, "fresh.git", key.RelativePath)
	}
	r.mu.Unlock()

	// ExpireIdleKeys forgets it as well once it is idle, without any request.
	exec.clock.advance(KeyStateTTL - time.Nanosecond)
	r.ExpireIdleKeys()
	require.Equal(t, 1, keptKeys(r), "a key idle for less than KeyStateTTL is kept")

	exec.clock.advance(time.Nanosecond)
	r.ExpireIdleKeys()
	require.Zero(t, keptKeys(r))
	require.Zero(t, gatherValues(t, reg)[effectiveMaxSizeGauge])
}

func TestKeptSizeIsUsedWithinKeyStateTTLAndForgottenAfter(t *testing.T) {
	ctx := testhelper.Context(t)

	exec := &clockExecutor{clock: newFakeClock()}
	obs := &sizeObserver{}
	r := keyStateRegistry(t, exec, obs)

	// A slow batch halves the size from 16 to 8, and the key keeps it.
	exec.set(200 * time.Millisecond)
	submitAndRetire(t, ctx, r, testKey)
	require.Equal(t, []int{16, 8, 8}, obs.snapshot())

	// Just before KeyStateTTL the next lane starts from the kept size, and a fast
	// batch grows it.
	exec.clock.advance(KeyStateTTL - time.Nanosecond)
	exec.set(0)
	submitAndRetire(t, ctx, r, testKey)
	require.Equal(t, []int{16, 8, 8, 8, 10, 10}, obs.snapshot())

	// After KeyStateTTL the key is forgotten, and its next lane starts at MaxSize.
	exec.clock.advance(KeyStateTTL)
	submitAndRetire(t, ctx, r, testKey)
	require.Equal(t, []int{16, 8, 8, 8, 10, 10, 0, 16, 16}, obs.snapshot())
}

func TestKeyWithALaneIsNotForgotten(t *testing.T) {
	ctx := testhelper.Context(t)

	clock := newFakeClock()
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	blocked := Key{Storage: "default", RelativePath: "blocked.git", Branch: "main"}

	exec := func(_ context.Context, key Key, reqs []string) ([]Result[string], error) {
		if key == blocked {
			clock.advance(200 * time.Millisecond)
			if len(reqs) == 1 && reqs[0] == "hold" {
				entered <- struct{}{}
				<-gate
			}
		}

		out := make([]Result[string], len(reqs))
		for i, req := range reqs {
			out[i].Value = "landed-" + req
		}
		return out, nil
	}

	obs := NewPrometheusObserver()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(obs))

	r := newRegistry(t, Config{Enabled: true, MaxSize: 16, TargetLatency: 100 * time.Millisecond}, exec, obs)
	r.now = clock.now
	t.Cleanup(r.Close)

	// The blocked key is slowed down to 8 and kept.
	submitAndRetire(t, ctx, r, blocked)

	// Its next lane runs for longer than KeyStateTTL. A request on another key
	// expires the idle keys meanwhile, but not the key whose lane is running.
	held := submitAsync(ctx, r, blocked, "hold")
	<-entered
	clock.advance(2 * KeyStateTTL)
	_, err := r.Submit(ctx, testKey, "r")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, live := r.lanes[testKey]
		return !live
	}, 10*time.Second, time.Millisecond)
	require.Equal(t, 2, keptKeys(r), "the key with a lane is kept")
	require.Equal(t, 8.0+16.0, gatherValues(t, reg)[effectiveMaxSizeGauge],
		"the lane counts with its size, the other key with MaxSize")

	// When its lane is retired, the key is idle from that moment on: it is kept
	// for another KeyStateTTL.
	close(gate)
	require.NoError(t, requireResult(t, held).err)
	require.Eventually(t, func() bool { return r.runningLanes() == 0 }, 10*time.Second, time.Millisecond)

	r.ExpireIdleKeys()
	r.mu.Lock()
	state, ok := r.keys[blocked]
	r.mu.Unlock()
	require.True(t, ok, "the key is kept after its lane was retired")
	require.Equal(t, 4, state.size, "the slow held batch halved the size again")
}

func TestCloseForgetsEveryKey(t *testing.T) {
	ctx := testhelper.Context(t)

	exec := &clockExecutor{clock: newFakeClock()}
	exec.set(200 * time.Millisecond)

	obs := NewPrometheusObserver()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(obs))

	r := keyStateRegistry(t, exec, obs)
	for i := 0; i < 3; i++ {
		submitAndRetire(t, ctx, r, repoKey(i))
	}
	require.Equal(t, 24.0, gatherValues(t, reg)[effectiveMaxSizeGauge])

	r.Close()
	require.Zero(t, keptKeys(r))
	require.Zero(t, gatherValues(t, reg)[effectiveMaxSizeGauge])
}

func TestSharedCollectForgetsIdleKeys(t *testing.T) {
	ctx := testhelper.Context(t)

	exec := &clockExecutor{clock: newFakeClock()}
	exec.set(200 * time.Millisecond)

	shared := NewShared(Config{Enabled: true, MaxSize: 16, TargetLatency: 100 * time.Millisecond})
	defer shared.Close()

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(shared))

	r, err := SharedRegistry(shared, exec.run)
	require.NoError(t, err)
	r.now = exec.clock.now

	for i := 0; i < 3; i++ {
		submitAndRetire(t, ctx, r, repoKey(i))
	}
	require.Equal(t, 24.0, gatherValues(t, reg)[effectiveMaxSizeGauge])

	// No request arrives any more. The scrape itself forgets the idle keys.
	exec.clock.advance(KeyStateTTL)
	require.Zero(t, gatherValues(t, reg)[effectiveMaxSizeGauge])
	require.Zero(t, keptKeys(r))
}
