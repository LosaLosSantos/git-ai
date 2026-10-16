package landlane

// Black-box spec tests for WP21 (internal/graft/landlane), written from
// doc/graft/PLAN3.md (WP21 section + RECONCILIATION section) only.
//
// Adapted to the merged API at release time: NewRegistry also returns an
// error, a Config batches only with Enabled set, and Response.BatchPosition is
// zero-based (UserLandResponse.batch_position, which the spec defines as
// 1-based, adds one).

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/prometheus/client_golang/prometheus"
)

const specTimeout = 5 * time.Second

type specOutcome struct {
	idx  int
	resp Response[int]
	err  error
}

func specRegistry(t *testing.T, cfg Config, exec Executor[int, int], obs Observer) *Registry[int, int] {
	t.Helper()
	reg, err := NewRegistry[int, int](cfg, exec, obs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func specKey(branch string) Key {
	return Key{Storage: "default", RelativePath: "@hashed/spec.git", Branch: branch}
}

// specEcho completes every request with Value = req * 10.
func specEcho(_ context.Context, _ Key, reqs []int) ([]Result[int], error) {
	out := make([]Result[int], len(reqs))
	for i, r := range reqs {
		out[i] = Result[int]{Value: r * 10}
	}
	return out, nil
}

type specRecorder struct {
	mu       sync.Mutex
	batches  [][]int
	batchLen map[int]int
	batchPos map[int]int
}

func newSpecRecorder() *specRecorder {
	return &specRecorder{batchLen: map[int]int{}, batchPos: map[int]int{}}
}

func (r *specRecorder) note(reqs []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, append([]int(nil), reqs...))
	for i, q := range reqs {
		r.batchLen[q] = len(reqs)
		r.batchPos[q] = i + 1
	}
}

func specSubmitAsync(reg *Registry[int, int], key Key, req int, out chan<- specOutcome) {
	go func() {
		resp, err := reg.Submit(context.Background(), key, req)
		out <- specOutcome{idx: req, resp: resp, err: err}
	}()
}

func specSubmitSync(t *testing.T, reg *Registry[int, int], key Key, req int) (Response[int], error) {
	t.Helper()
	ch := make(chan specOutcome, 1)
	specSubmitAsync(reg, key, req, ch)
	o := specRecv(t, ch)
	return o.resp, o.err
}

func specRecv(t *testing.T, ch <-chan specOutcome) specOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(specTimeout):
		t.Fatalf("timed out waiting for Submit to return")
	}
	return specOutcome{}
}

func specWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(specTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestSpecFIFOAndMaxSizeSplit: requests queued behind a running batch are
// handed off in FIFO order, batches never exceed MaxSize, and each response
// reports the BatchSize and zero-based BatchPosition of the batch it landed in.
func TestSpecFIFOAndMaxSizeSplit(t *testing.T) {
	rec := newSpecRecorder()
	gate := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int32
	exec := func(ctx context.Context, key Key, reqs []int) ([]Result[int], error) {
		rec.note(reqs)
		if calls.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return specEcho(ctx, key, reqs)
	}
	reg := specRegistry(t, Config{Enabled: true, Window: 0, MaxSize: 3}, exec, nil)
	defer reg.Close()
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	defer open()

	key := specKey("main")
	out := make(chan specOutcome, 8)
	specSubmitAsync(reg, key, 0, out)
	specWait(t, entered, "first batch to reach the executor")
	for i := 1; i < 8; i++ {
		specSubmitAsync(reg, key, i, out)
		time.Sleep(10 * time.Millisecond)
	}
	open()

	got := map[int]specOutcome{}
	for i := 0; i < 8; i++ {
		o := specRecv(t, out)
		got[o.idx] = o
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var flat []int
	for _, b := range rec.batches {
		if len(b) > 3 {
			t.Fatalf("batch %v exceeds MaxSize 3", b)
		}
		flat = append(flat, b...)
	}
	want := []int{0, 1, 2, 3, 4, 5, 6, 7}
	if !reflect.DeepEqual(flat, want) {
		t.Fatalf("executor saw requests in order %v, want FIFO order %v", flat, want)
	}
	for i := 0; i < 8; i++ {
		o := got[i]
		if o.err != nil {
			t.Fatalf("request %d: unexpected error %v", i, o.err)
		}
		if o.resp.Value != i*10 {
			t.Fatalf("request %d: Value = %d, want %d", i, o.resp.Value, i*10)
		}
		if o.resp.BatchSize != rec.batchLen[i] {
			t.Errorf("request %d: BatchSize = %d, executor batch had %d", i, o.resp.BatchSize, rec.batchLen[i])
		}
		if o.resp.BatchPosition+1 != rec.batchPos[i] {
			t.Errorf("request %d: BatchPosition = %d, want zero-based position %d", i, o.resp.BatchPosition, rec.batchPos[i]-1)
		}
	}
}

// TestSpecWindowGroupsRequests: requests arriving within Window of the first
// queued one are executed as a single batch.
func TestSpecWindowGroupsRequests(t *testing.T) {
	rec := newSpecRecorder()
	exec := func(ctx context.Context, key Key, reqs []int) ([]Result[int], error) {
		rec.note(reqs)
		return specEcho(ctx, key, reqs)
	}
	reg := specRegistry(t, Config{Enabled: true, Window: 200 * time.Millisecond, MaxSize: 50}, exec, nil)
	defer reg.Close()

	key := specKey("main")
	out := make(chan specOutcome, 3)
	for i := 0; i < 3; i++ {
		specSubmitAsync(reg, key, i, out)
	}
	for i := 0; i < 3; i++ {
		if o := specRecv(t, out); o.err != nil {
			t.Fatalf("request %d: unexpected error %v", o.idx, o.err)
		}
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.batches) != 1 || len(rec.batches[0]) != 3 {
		t.Fatalf("expected one batch of 3 requests within the window, got %v", rec.batches)
	}
}

// TestSpecKeysRunInParallel: a blocked lane does not stop other keys.
func TestSpecKeysRunInParallel(t *testing.T) {
	slowEntered := make(chan struct{})
	release := make(chan struct{})
	var once, relOnce sync.Once
	exec := func(ctx context.Context, key Key, reqs []int) ([]Result[int], error) {
		if key.Branch == "slow" {
			once.Do(func() { close(slowEntered) })
			<-release
		}
		return specEcho(ctx, key, reqs)
	}
	reg := specRegistry(t, Config{Enabled: true, Window: 0, MaxSize: 4}, exec, nil)
	defer reg.Close()
	defer relOnce.Do(func() { close(release) })

	slow := make(chan specOutcome, 1)
	specSubmitAsync(reg, specKey("slow"), 1, slow)
	specWait(t, slowEntered, "slow lane to block")

	resp, err := specSubmitSync(t, reg, specKey("fast"), 2)
	if err != nil {
		t.Fatalf("fast key blocked by slow key: %v", err)
	}
	if resp.Value != 20 {
		t.Fatalf("fast key Value = %d, want 20", resp.Value)
	}

	relOnce.Do(func() { close(release) })
	if o := specRecv(t, slow); o.err != nil {
		t.Fatalf("slow key request failed: %v", o.err)
	}
}

// TestSpecOneBatchPerKeyAtATime: at most one batch per key executes at a time.
func TestSpecOneBatchPerKeyAtATime(t *testing.T) {
	var active, maxActive atomic.Int32
	exec := func(ctx context.Context, key Key, reqs []int) ([]Result[int], error) {
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		active.Add(-1)
		return specEcho(ctx, key, reqs)
	}
	reg := specRegistry(t, Config{Enabled: true, Window: 0, MaxSize: 2}, exec, nil)
	defer reg.Close()

	key := specKey("main")
	const n = 20
	out := make(chan specOutcome, n)
	for i := 0; i < n; i++ {
		specSubmitAsync(reg, key, i, out)
	}
	for i := 0; i < n; i++ {
		if o := specRecv(t, out); o.err != nil {
			t.Fatalf("request %d: unexpected error %v", o.idx, o.err)
		}
	}
	if m := maxActive.Load(); m != 1 {
		t.Fatalf("max concurrent batches on one key = %d, want 1", m)
	}
}

// TestSpecCancelBeforeHandOffIsRemoved: a submitter cancelled while its item is
// still queued gets ctx.Err() and the item never reaches the executor.
func TestSpecCancelBeforeHandOffIsRemoved(t *testing.T) {
	rec := newSpecRecorder()
	gate := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int32
	exec := func(ctx context.Context, key Key, reqs []int) ([]Result[int], error) {
		rec.note(reqs)
		if calls.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return specEcho(ctx, key, reqs)
	}
	reg := specRegistry(t, Config{Enabled: true, Window: 0, MaxSize: 1}, exec, nil)
	defer reg.Close()
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	defer open()

	key := specKey("main")
	out := make(chan specOutcome, 2)
	specSubmitAsync(reg, key, 0, out)
	specWait(t, entered, "first batch to reach the executor")

	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan specOutcome, 1)
	go func() {
		resp, err := reg.Submit(ctx, key, 1)
		cancelled <- specOutcome{idx: 1, resp: resp, err: err}
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if o := specRecv(t, cancelled); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("cancelled queued Submit returned %v, want context.Canceled", o.err)
	}

	specSubmitAsync(reg, key, 2, out)
	open()
	for i := 0; i < 2; i++ {
		if o := specRecv(t, out); o.err != nil {
			t.Fatalf("request %d: unexpected error %v", o.idx, o.err)
		}
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, b := range rec.batches {
		for _, q := range b {
			if q == 1 {
				t.Fatalf("cancelled-before-hand-off request reached the executor: batches %v", rec.batches)
			}
		}
	}
}

// TestSpecCancelAfterHandOffReturnsPromptly: once handed off, cancellation makes
// Submit return ctx.Err() immediately, and the executor's context is NOT
// cancelled by the submitter.
func TestSpecCancelAfterHandOffReturnsPromptly(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	laneErr := make(chan error, 1)
	var calls atomic.Int32
	exec := func(ctx context.Context, key Key, reqs []int) ([]Result[int], error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			laneErr <- ctx.Err()
		}
		return specEcho(ctx, key, reqs)
	}
	reg := specRegistry(t, Config{Enabled: true, Window: 0, MaxSize: 1}, exec, nil)
	defer reg.Close()
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()

	key := specKey("main")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan specOutcome, 1)
	go func() {
		resp, err := reg.Submit(ctx, key, 7)
		res <- specOutcome{idx: 7, resp: resp, err: err}
	}()
	specWait(t, entered, "hand-off to executor")
	cancel()
	if o := specRecv(t, res); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("Submit after hand-off returned %v, want context.Canceled", o.err)
	}

	open()
	select {
	case err := <-laneErr:
		if err != nil {
			t.Fatalf("executor context was cancelled by a submitter: %v", err)
		}
	case <-time.After(specTimeout):
		t.Fatalf("executor never reported its context state")
	}

	if resp, err := specSubmitSync(t, reg, key, 8); err != nil || resp.Value != 80 {
		t.Fatalf("lane unusable after cancelled hand-off: value=%d err=%v", resp.Value, err)
	}
}

// TestSpecCloseFailsQueuedAndRejectsNew: Close waits for the running batch,
// fails queued items with ErrClosed, and later Submit calls return ErrClosed.
func TestSpecCloseFailsQueuedAndRejectsNew(t *testing.T) {
	entered := make(chan struct{})
	gate := make(chan struct{})
	var calls atomic.Int32
	exec := func(ctx context.Context, key Key, reqs []int) ([]Result[int], error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return specEcho(ctx, key, reqs)
	}
	reg := specRegistry(t, Config{Enabled: true, Window: 0, MaxSize: 1}, exec, nil)
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	defer open()

	key := specKey("main")
	out := make(chan specOutcome, 2)
	specSubmitAsync(reg, key, 1, out)
	specWait(t, entered, "running batch")
	specSubmitAsync(reg, key, 2, out)
	time.Sleep(20 * time.Millisecond)

	closed := make(chan struct{})
	go func() {
		reg.Close()
		close(closed)
	}()
	time.Sleep(20 * time.Millisecond)
	open()

	results := map[int]specOutcome{}
	for i := 0; i < 2; i++ {
		o := specRecv(t, out)
		results[o.idx] = o
	}
	if results[1].err != nil {
		t.Fatalf("running batch should complete during Close, got error %v", results[1].err)
	}
	if !errors.Is(results[2].err, ErrClosed) {
		t.Fatalf("queued item after Close returned %v, want ErrClosed", results[2].err)
	}
	select {
	case <-closed:
	case <-time.After(specTimeout):
		t.Fatalf("Close did not return after the running batch finished")
	}
	if _, err := specSubmitSync(t, reg, key, 3); !errors.Is(err, ErrClosed) {
		t.Fatalf("Submit after Close returned %v, want ErrClosed", err)
	}
}

// TestSpecExecutorPanicIsRecovered: a panicking batch fails its items with an
// error mentioning the panic, and the lane keeps serving later requests.
func TestSpecExecutorPanicIsRecovered(t *testing.T) {
	var calls atomic.Int32
	exec := func(ctx context.Context, key Key, reqs []int) ([]Result[int], error) {
		if calls.Add(1) == 1 {
			panic("spec-boom-17")
		}
		return specEcho(ctx, key, reqs)
	}
	reg := specRegistry(t, Config{Enabled: true, Window: 0, MaxSize: 4}, exec, nil)
	defer reg.Close()

	key := specKey("main")
	_, err := specSubmitSync(t, reg, key, 1)
	if err == nil || !strings.Contains(err.Error(), "spec-boom-17") {
		t.Fatalf("request in panicking batch got %v, want an error wrapping the panic value", err)
	}
	resp, err := specSubmitSync(t, reg, key, 2)
	if err != nil || resp.Value != 20 {
		t.Fatalf("lane did not survive panic: value=%d err=%v", resp.Value, err)
	}
}

// TestSpecExecutorForgetsItemsCompletesWithErrNotCompleted: items the executor
// leaves incomplete fail with ErrNotCompleted and do not hang.
func TestSpecExecutorForgetsItemsCompletesWithErrNotCompleted(t *testing.T) {
	exec := func(_ context.Context, _ Key, _ []int) ([]Result[int], error) {
		return nil, nil
	}
	reg := specRegistry(t, Config{Enabled: true, Window: 0, MaxSize: 4}, exec, nil)
	defer reg.Close()

	key := specKey("main")
	for i := 0; i < 2; i++ {
		if _, err := specSubmitSync(t, reg, key, i); !errors.Is(err, ErrNotCompleted) {
			t.Fatalf("forgotten item %d returned %v, want ErrNotCompleted", i, err)
		}
	}
}

// TestSpecNoGoroutineLeakWhenIdle: lane goroutines exist only while their key
// has work, so after going idle the goroutine count returns to its baseline.
func TestSpecNoGoroutineLeakWhenIdle(t *testing.T) {
	reg := specRegistry(t, Config{Enabled: true, Window: 0, MaxSize: 4}, specEcho, nil)
	defer reg.Close()
	before := runtime.NumGoroutine()

	for k := 0; k < 50; k++ {
		key := Key{Storage: "default", RelativePath: fmt.Sprintf("@hashed/r%d.git", k), Branch: "main"}
		for j := 0; j < 3; j++ {
			if _, err := specSubmitSync(t, reg, key, j); err != nil {
				t.Fatalf("key %d request %d: %v", k, j, err)
			}
		}
	}

	deadline := time.Now().Add(specTimeout)
	for runtime.NumGoroutine() > before+2 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines did not return to baseline when idle: before=%d now=%d",
				before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSpecPrometheusMetricsRegistered: PrometheusObserver is a prometheus
// Collector exposing the three promised metric names.
func TestSpecPrometheusMetricsRegistered(t *testing.T) {
	obs := new(PrometheusObserver)
	reg := specRegistry(t, Config{Enabled: true, Window: 0, MaxSize: 4}, specEcho, obs)
	defer reg.Close()

	promReg := prometheus.NewPedanticRegistry()
	if err := promReg.Register(obs); err != nil {
		t.Fatalf("PrometheusObserver not registrable as a prometheus.Collector: %v", err)
	}
	if _, err := specSubmitSync(t, reg, specKey("main"), 1); err != nil {
		t.Fatalf("submit: %v", err)
	}

	families, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
	}
	for _, want := range []string{
		"graft_land_lane_queue_depth",
		"graft_land_batch_size",
		"graft_land_lane_wait_seconds",
	} {
		if !names[want] {
			t.Errorf("metric %q not exposed; got families %v", want, names)
		}
	}
}

// TestSpecConfigFromGraft: ConfigFromGraft applies the documented defaults
// (10ms window, max size 50), parses the window and rejects invalid values.
func TestSpecConfigFromGraft(t *testing.T) {
	cfg, err := ConfigFromGraft(config.GraftLandBatch{})
	if err != nil {
		t.Fatalf("ConfigFromGraft(zero) error: %v", err)
	}
	if cfg.Window != 10*time.Millisecond || cfg.MaxSize != 50 {
		t.Errorf("defaults: got Window=%v MaxSize=%d, want 10ms and 50", cfg.Window, cfg.MaxSize)
	}

	cfg, err = ConfigFromGraft(config.GraftLandBatch{Window: "25ms", MaxSize: 7})
	if err != nil {
		t.Fatalf("ConfigFromGraft(25ms,7) error: %v", err)
	}
	if cfg.Window != 25*time.Millisecond || cfg.MaxSize != 7 {
		t.Errorf("explicit values: got Window=%v MaxSize=%d, want 25ms and 7", cfg.Window, cfg.MaxSize)
	}

	if _, err := ConfigFromGraft(config.GraftLandBatch{Window: "not-a-duration"}); err == nil {
		t.Errorf("ConfigFromGraft with an unparsable window returned no error")
	}
}
