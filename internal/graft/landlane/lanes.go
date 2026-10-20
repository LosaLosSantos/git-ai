// Package landlane serializes and batches land requests per lane.
//
// A Registry holds one lane per Key (in Graft: storage, repository and
// branch). Requests submitted to a lane enter a FIFO queue. A runner goroutine
// of the lane collects requests for up to Config.Window after the first queued
// request, or until the effective max size of the lane is queued, whichever
// comes first, and hands the whole batch to the Executor in one call. The
// effective max size starts at Config.MaxSize and adapts to
// Config.TargetLatency after each batch. Batches of one lane never run
// concurrently. The package knows nothing about Git: the Executor
// lands the batch.
package landlane

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// ErrClosed is returned by Submit after Close, and to the requests that were
// still queued when Close was called.
var ErrClosed = errors.New("land lane registry is closed")

// ErrNotCompleted is returned to a request of a batch for which the Executor
// returned no result, while it returned results for other requests of the same
// batch.
var ErrNotCompleted = errors.New("land lane executor returned no result for the request")

// Key identifies a lane. In Graft a lane exists per storage, repository and
// branch.
type Key struct {
	Storage      string
	RelativePath string
	Branch       string
}

// Config controls batching. Use ConfigFromGraft to build it from the
// [graft.land.batch] configuration.
//
// The zero value is a disabled configuration, because Enabled defaults to
// false. A Config literal that only sets Window and MaxSize does not batch: set
// Enabled to true as well.
type Config struct {
	// Enabled turns batching on. When disabled every request is executed on
	// its own and immediately, without waiting for other requests. Unlike
	// batched execution, disabled execution does not serialize the requests of
	// one key: the caller must not rely on it for mutual exclusion.
	Enabled bool
	// Window is how long a lane waits for more requests after its first queued
	// request. Zero means no wait.
	Window time.Duration
	// MaxSize is the maximum number of requests in one batch. It is also the
	// effective max size of a lane that has never been slowed down.
	MaxSize int
	// TargetLatency is the batch duration a lane aims for. A lane halves its
	// effective max size after a batch slower than TargetLatency, and grows it
	// after a batch faster than half of TargetLatency, up to MaxSize. Zero
	// disables the adaptation: every lane always uses MaxSize.
	TargetLatency time.Duration
	// Clock is the time that measures the batches, whose durations drive the
	// adaptation, and the idle time of the keys (KeyStateTTL). Nil means
	// time.Now. The window always runs on the time of the system. Tests set it
	// to decide how long the batches take.
	Clock func() time.Time
}

// ConfigFromGraft builds the batching configuration from
// [graft.land.batch], applying the defaults of the configuration.
func ConfigFromGraft(c config.GraftLandBatch) (Config, error) {
	window, err := c.WindowOrDefault()
	if err != nil {
		return Config{}, err
	}
	targetLatency, err := c.TargetLatencyOrDefault()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Enabled:       !c.Disabled,
		Window:        window,
		MaxSize:       c.MaxSizeOrDefault(),
		TargetLatency: targetLatency,
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if c.Window < 0 {
		return errors.New("window must not be negative")
	}
	if c.MaxSize < 1 {
		return errors.New("max_size must be at least 1")
	}
	if c.TargetLatency < 0 {
		return errors.New("target latency must not be negative")
	}
	return nil
}

// nextMaxSize returns the effective max size of a lane after a batch that took
// took, given the current effective size, the configured maximum and the target
// latency. A batch slower than target halves the size (but not below 1). A batch
// faster than target/2 grows it by a quarter (at least 1), up to maxSize. Any
// other batch, and a zero target, leave the size unchanged.
func nextMaxSize(current, maxSize int, took, target time.Duration) int {
	switch {
	case target <= 0:
		return current
	case took > target:
		return max(1, current/2)
	case took < target/2:
		return min(maxSize, current+max(1, current/4))
	default:
		return current
	}
}

// Result is the outcome of one request of a batch.
type Result[Res any] struct {
	Value Res
	Err   error
}

// Executor lands a batch of requests. It returns the results in the same order
// as the requests. An error returned by the Executor fails every request of the
// batch. Requests for which no result is returned, while others got one, fail
// with ErrNotCompleted. Returning more results than requests is an error.
//
// The context is not cancelled when the submitter of a request cancels its
// request. It carries the values (not the deadline or cancellation) of the
// first request of the batch, which is an arbitrary submitter. The Executor
// must take per-request identity and settings from the requests themselves,
// never from the context.
type Executor[Req, Res any] func(ctx context.Context, key Key, reqs []Req) ([]Result[Res], error)

// Response is returned to the submitter of a request.
type Response[Res any] struct {
	// Value is the value the Executor returned for the request.
	Value Res
	// BatchSize is the number of requests executed together with this one.
	// It is 1 when batching is disabled.
	BatchSize int
	// BatchPosition is the zero-based position of the request in its batch.
	BatchPosition int
	// Wait is the time between the submission and the start of the execution.
	Wait time.Duration
}

// Observer receives batching events. Its methods are called while the
// registry holds its lock for QueueDepth, so they must not block.
type Observer interface {
	// BatchExecuted is called after each executed batch.
	BatchExecuted(key Key, size int, took time.Duration)
	// RequestWaited is called for each request that entered an execution.
	RequestWaited(key Key, wait time.Duration)
	// QueueDepth is called whenever the number of queued requests of a lane
	// changes.
	QueueDepth(key Key, depth int)
	// MaxSizeChanged is called when a lane comes into existence, whenever its
	// effective max size changes, and when it is retired, with the size that the
	// key keeps (see Registry). It is called with 0 when the registry forgets the
	// key: after the key has had no lane for KeyStateTTL, and on Close.
	MaxSizeChanged(key Key, size int)
}

// NopObserver is an Observer that ignores all events.
type NopObserver struct{}

// BatchExecuted implements Observer.
func (NopObserver) BatchExecuted(Key, int, time.Duration) {}

// RequestWaited implements Observer.
func (NopObserver) RequestWaited(Key, time.Duration) {}

// QueueDepth implements Observer.
func (NopObserver) QueueDepth(Key, int) {}

// MaxSizeChanged implements Observer.
func (NopObserver) MaxSizeChanged(Key, int) {}

// Registry runs requests of many lanes. The zero value is not usable, use
// NewRegistry.
//
// A single mutex guards all lanes. The critical sections only manipulate
// queues, so this does not limit throughput in practice, and it keeps the
// lane lifecycle simple: a lane is removed from the registry in the same
// critical section in which its queue is found empty.
type Registry[Req, Res any] struct {
	cfg  Config
	exec Executor[Req, Res]
	obs  Observer
	// now measures the duration of the executor calls, which drives the
	// adaptation of the lanes: Config.Clock, or time.Now. Tests of the package
	// replace it with a fake clock.
	now func() time.Time

	// running counts the goroutines that execute batches: lane runners and
	// unbatched executions. Close waits for them.
	running sync.WaitGroup
	// done is closed by Close to wake up lane runners that wait for a window.
	done chan struct{}

	mu     sync.Mutex
	closed bool
	lanes  map[Key]*lane[Req, Res]
	// keys holds the effective max size of the keys whose last lane was retired
	// less than KeyStateTTL ago, whatever that size is. It outlives the lanes: a
	// lane is retired as soon as its queue is empty, and the next lane of the same
	// key starts from the kept size. A key that has had no lane for KeyStateTTL is
	// forgotten (see expireLocked), so the map only holds recently used keys, and
	// the next lane of a forgotten key starts at Config.MaxSize. Guarded by mu.
	keys map[Key]keyState
	// nextExpiry is the earliest time at which an entry of keys expires, or the
	// zero time if no entry can expire (every kept key has a lane). Guarded by mu.
	nextExpiry time.Time
}

// KeyStateTTL is how long a Registry keeps the effective max size of a key
// after its last lane was retired. Within that time a new lane of the key starts
// from the kept size. After it, the key is forgotten: its next lane starts at
// Config.MaxSize and it no longer counts in graft_land_lane_effective_max_size.
const KeyStateTTL = 5 * time.Minute

// keyState is what a Registry keeps of a key between its lanes.
type keyState struct {
	// size is the effective max size of the last lane of the key.
	size int
	// idleSince is when the last lane of the key was retired.
	idleSince time.Time
}

// NewRegistry creates a registry. A nil observer is replaced by NopObserver.
func NewRegistry[Req, Res any](cfg Config, exec Executor[Req, Res], obs Observer) (*Registry[Req, Res], error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("land lane config: %w", err)
	}
	if exec == nil {
		return nil, errors.New("land lane: nil executor")
	}
	if obs == nil {
		obs = NopObserver{}
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}

	return &Registry[Req, Res]{
		cfg:   cfg,
		exec:  exec,
		obs:   obs,
		now:   now,
		done:  make(chan struct{}),
		lanes: map[Key]*lane[Req, Res]{},
		keys:  map[Key]keyState{},
	}, nil
}

// Submit queues the request on the lane of key and waits until its batch has
// been executed. It returns the response of the request, or an error if the
// Executor failed the request or ctx was done first. Submit returns ErrClosed
// once the registry is closed.
//
// A request whose context is done before its execution starts is removed from
// the queue and Submit returns ctx.Err(). If the context is done while the
// batch runs, Submit returns ctx.Err() immediately, but the batch still
// completes: the change may still land.
func (r *Registry[Req, Res]) Submit(ctx context.Context, key Key, req Req) (Response[Res], error) {
	if err := ctx.Err(); err != nil {
		return Response[Res]{}, err
	}

	if !r.cfg.Enabled {
		return r.executeAlone(ctx, key, req)
	}

	e := &entry[Req, Res]{
		ctx:      ctx,
		req:      req,
		enqueued: time.Now(),
		done:     make(chan delivery[Res], 1),
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Response[Res]{}, ErrClosed
	}

	r.expireLocked()

	l := r.lanes[key]
	if l == nil {
		l = &lane[Req, Res]{kick: make(chan struct{}, 1), maxSize: r.effectiveMaxSizeLocked(key)}
		r.lanes[key] = l
		r.obs.MaxSizeChanged(key, l.maxSize)
	}
	l.queue = append(l.queue, e)
	if len(l.queue) >= l.maxSize {
		select {
		case l.kick <- struct{}{}:
		default:
		}
	}
	if !l.running {
		l.running = true
		r.running.Add(1)
		go r.run(key, l)
	}
	r.obs.QueueDepth(key, len(l.queue))
	r.mu.Unlock()

	select {
	case d := <-e.done:
		return d.resp, d.err
	case <-ctx.Done():
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	switch e.state {
	case stateQueued:
		l.remove(e)
		r.obs.QueueDepth(key, len(l.queue))
		return Response[Res]{}, ctx.Err()
	case stateExecuting:
		return Response[Res]{}, ctx.Err()
	default:
		// The batch finished or the request was dropped concurrently with
		// the cancellation. The outcome is already in the channel.
		d := <-e.done
		return d.resp, d.err
	}
}

// Close stops the registry. Submit returns ErrClosed from now on, the requests
// still queued fail with ErrClosed, and Close waits until the running batches
// have completed. The registry forgets every key: the observer gets size 0 for
// each of them.
func (r *Registry[Req, Res]) Close() {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.done)

		for key, l := range r.lanes {
			for _, e := range l.queue {
				e.state = stateDone
				e.done <- delivery[Res]{err: ErrClosed}
			}
			l.queue = nil
			r.obs.QueueDepth(key, 0)
			r.obs.MaxSizeChanged(key, 0)
		}

		for key := range r.keys {
			r.obs.MaxSizeChanged(key, 0)
		}
		clear(r.keys)
		r.nextExpiry = time.Time{}
	}
	r.mu.Unlock()

	r.running.Wait()
}

// ExpireIdleKeys forgets the keys that have had no lane for KeyStateTTL, as
// Submit does. Collectors call it before they report the metrics, so that the
// effective max size of a key that is no longer used drops out of the gauge
// even when no request arrives.
func (r *Registry[Req, Res]) ExpireIdleKeys() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.expireLocked()
}

// expireLocked forgets the keys whose last lane was retired at least
// KeyStateTTL ago and reports size 0 for them. It scans the kept keys only once
// the earliest of them has expired. The registry lock must be held.
func (r *Registry[Req, Res]) expireLocked() {
	if r.nextExpiry.IsZero() {
		return
	}

	now := r.now()
	if now.Before(r.nextExpiry) {
		return
	}

	var next time.Time
	for key, state := range r.keys {
		if _, live := r.lanes[key]; live {
			// The key is in use. Its state is renewed when the lane is retired.
			continue
		}

		expiry := state.idleSince.Add(KeyStateTTL)
		if !now.Before(expiry) {
			delete(r.keys, key)
			r.obs.MaxSizeChanged(key, 0)
			continue
		}

		if next.IsZero() || expiry.Before(next) {
			next = expiry
		}
	}

	r.nextExpiry = next
}

// retireLocked removes lane l of key from the registry and keeps its effective
// max size for the key. After Close the key is not kept. The registry lock must
// be held.
func (r *Registry[Req, Res]) retireLocked(key Key, l *lane[Req, Res]) {
	delete(r.lanes, key)
	l.running = false

	if r.closed {
		r.obs.MaxSizeChanged(key, 0)
		return
	}

	idleSince := r.now()
	r.keys[key] = keyState{size: l.maxSize, idleSince: idleSince}
	if expiry := idleSince.Add(KeyStateTTL); r.nextExpiry.IsZero() || expiry.Before(r.nextExpiry) {
		r.nextExpiry = expiry
	}
	r.obs.MaxSizeChanged(key, l.maxSize)

	r.expireLocked()
}

// runningLanes returns the number of lanes that currently hold queued or
// executing requests. It is for tests.
func (r *Registry[Req, Res]) runningLanes() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.lanes)
}

// executeAlone executes a single request without batching. Like a batched
// request, it returns ctx.Err() as soon as ctx is done, while the execution
// completes in the background.
func (r *Registry[Req, Res]) executeAlone(ctx context.Context, key Key, req Req) (Response[Res], error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Response[Res]{}, ErrClosed
	}
	r.running.Add(1)
	r.mu.Unlock()

	type outcome struct {
		resp Response[Res]
		err  error
	}

	done := make(chan outcome, 1)
	go func() {
		defer r.running.Done()

		resp, err := r.runAlone(ctx, key, req)
		done <- outcome{resp: resp, err: err}
	}()

	select {
	case o := <-done:
		return o.resp, o.err
	case <-ctx.Done():
		return Response[Res]{}, ctx.Err()
	}
}

// runAlone executes a single request. The registry must have accounted for the
// execution in running.
func (r *Registry[Req, Res]) runAlone(ctx context.Context, key Key, req Req) (Response[Res], error) {
	start := time.Now()
	results, err := r.call(context.WithoutCancel(ctx), key, []Req{req})
	took := time.Since(start)
	r.obs.BatchExecuted(key, 1, took)
	r.obs.RequestWaited(key, 0)

	resp := Response[Res]{BatchSize: 1}
	resp.Value, err = resultOf(results, err, 0)
	return resp, err
}

// run executes the batches of a lane until its queue is empty. It removes the
// lane from the registry before it returns.
func (r *Registry[Req, Res]) run(key Key, l *lane[Req, Res]) {
	defer r.running.Done()

	for {
		r.mu.Lock()
		r.collectLocked(l)

		if len(l.queue) == 0 {
			r.retireLocked(key, l)
			r.mu.Unlock()
			return
		}

		batch := r.takeLocked(key, l)
		r.mu.Unlock()

		if len(batch) > 0 {
			r.execute(key, l, batch)
		}
	}
}

// collectLocked waits until the next batch of the lane is ready: when Window
// has passed since the first queued request, or when MaxSize requests are
// queued, or when the registry is closed. A batch that has already waited for
// Window is ready at once, so back-to-back batches of a busy lane have no idle
// time between them. The registry lock must be held. It is released while
// waiting and held again on return.
func (r *Registry[Req, Res]) collectLocked(l *lane[Req, Res]) {
	for n := len(l.queue); n > 0 && n < l.maxSize; n = len(l.queue) {
		wait := time.Until(l.queue[0].enqueued.Add(r.cfg.Window))
		if wait <= 0 {
			return
		}

		// Drop a stale kick so that only a batch filling up from now on ends
		// the wait early.
		select {
		case <-l.kick:
		default:
		}

		r.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-l.kick:
		case <-r.done:
		}
		timer.Stop()

		r.mu.Lock()
	}
}

// takeLocked removes the next batch from the queue of the lane. Requests whose
// context is already done are dropped and answered with the context error. The
// registry lock must be held.
func (r *Registry[Req, Res]) takeLocked(key Key, l *lane[Req, Res]) []*entry[Req, Res] {
	var batch []*entry[Req, Res]

	for len(batch) < l.maxSize && len(l.queue) > 0 {
		e := l.queue[0]
		l.queue = l.queue[1:]

		if err := e.ctx.Err(); err != nil {
			e.state = stateDone
			e.done <- delivery[Res]{err: err}
			continue
		}

		e.state = stateExecuting
		batch = append(batch, e)
	}

	r.obs.QueueDepth(key, len(l.queue))

	return batch
}

// execute runs one batch of lane l and delivers the outcome to every request of
// it, except the requests that the Executor completed early (CompleteEarly).
func (r *Registry[Req, Res]) execute(key Key, l *lane[Req, Res], batch []*entry[Req, Res]) {
	reqs := make([]Req, len(batch))
	for i, e := range batch {
		reqs[i] = e.req
	}

	start := time.Now()
	began := r.now()
	state := &executing[Req, Res]{reg: r, entries: batch, start: start}
	// The batch is not owned by any single submitter. It keeps the values of
	// the first request, but not its cancellation. The context also carries the
	// batch, so that the Executor can complete requests early (CompleteEarly).
	ctx := context.WithValue(context.WithoutCancel(batch[0].ctx), completionKey{}, state)
	results, err := r.call(ctx, key, reqs)
	took := r.now().Sub(began)

	// The observer is notified before the results are delivered, so that the
	// metrics of a batch are visible once its submitters return.
	r.obs.BatchExecuted(key, len(batch), took)
	for _, e := range batch {
		r.obs.RequestWaited(key, start.Sub(e.enqueued))
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.adaptLocked(key, l, took)

	for i, e := range batch {
		if e.state == stateDone {
			// Completed early. The Executor's result for this request is ignored.
			continue
		}

		value, reqErr := resultOf(results, err, i)

		d := delivery[Res]{
			resp: Response[Res]{
				Value:         value,
				BatchSize:     len(batch),
				BatchPosition: i,
				Wait:          start.Sub(e.enqueued),
			},
			err: reqErr,
		}

		e.state = stateDone
		e.done <- d // buffered, never blocks
	}
}

// effectiveMaxSizeLocked returns the effective max size that a lane of key
// starts with: the size kept for the key, or Config.MaxSize. The registry lock
// must be held.
func (r *Registry[Req, Res]) effectiveMaxSizeLocked(key Key) int {
	if state, ok := r.keys[key]; ok {
		return state.size
	}

	return r.cfg.MaxSize
}

// adaptLocked updates the effective max size of lane l after a batch that took
// took. The key keeps the size when the lane is retired (retireLocked). The
// registry lock must be held.
func (r *Registry[Req, Res]) adaptLocked(key Key, l *lane[Req, Res], took time.Duration) {
	size := nextMaxSize(l.maxSize, r.cfg.MaxSize, took, r.cfg.TargetLatency)
	if size == l.maxSize {
		return
	}

	l.maxSize = size
	r.obs.MaxSizeChanged(key, size)
}

// completionKey is the context key of the batch that an Executor call runs.
type completionKey struct{}

// earlyCompleter completes requests of an executing batch. The type parameter is
// the result type of the Registry, so that CompleteEarly can find the batch of
// its own Registry only.
type earlyCompleter[Res any] interface {
	completeEarly(index int, r Result[Res]) bool
}

// executing is the state of one batch while the Executor runs it. It is carried
// by the context of the Executor call.
type executing[Req, Res any] struct {
	reg     *Registry[Req, Res]
	entries []*entry[Req, Res]
	start   time.Time
}

// completeEarly delivers the result of the request at index now, unless the
// request has already been completed. It takes the registry lock, which execute
// also holds while it delivers the outcome of the batch.
func (b *executing[Req, Res]) completeEarly(index int, r Result[Res]) bool {
	b.reg.mu.Lock()
	defer b.reg.mu.Unlock()

	if index < 0 || index >= len(b.entries) {
		return false
	}

	e := b.entries[index]
	if e.state != stateExecuting {
		return false
	}

	e.state = stateDone
	e.done <- delivery[Res]{
		resp: Response[Res]{
			Value:         r.Value,
			BatchSize:     len(b.entries),
			BatchPosition: index,
			Wait:          b.start.Sub(e.enqueued),
		},
		err: r.Err,
	}

	return true
}

// CompleteEarly delivers the result of reqs[index] of the running batch now,
// before the Executor returns: the submitter of the request returns with
// Response{Value: r.Value, BatchSize, BatchPosition: index, Wait} and the error
// r.Err. The Executor's later result for that index is ignored. Use it for the
// requests whose outcome is final before the batch's remaining work, so that
// they do not wait for it.
//
// It returns false if ctx is not the context of an Executor call of this
// package with the same Res, if index is out of range, or if the request has
// already been completed, by an earlier CompleteEarly or by the batch. It is
// safe for concurrent calls. Requests of a disabled Registry run alone and are
// not completed early: CompleteEarly returns false for them.
func CompleteEarly[Res any](ctx context.Context, index int, r Result[Res]) bool {
	c, ok := ctx.Value(completionKey{}).(earlyCompleter[Res])
	if !ok {
		return false
	}

	return c.completeEarly(index, r)
}

// call invokes the Executor. A panic is turned into an error, so that a bug in
// one batch fails its requests instead of crashing the process from the lane
// goroutine.
func (r *Registry[Req, Res]) call(ctx context.Context, key Key, reqs []Req) (results []Result[Res], err error) {
	defer func() {
		if p := recover(); p != nil {
			results, err = nil, panicError(p)
		}
	}()

	results, err = r.exec(ctx, key, reqs)
	if err == nil && len(results) > len(reqs) {
		err = fmt.Errorf("executor returned %d results for %d requests", len(results), len(reqs))
	}

	return results, err
}

// resultOf returns the outcome of the request at index i of a batch, given the
// results and the error of its execution. A batch error fails every request.
// A request without a result gets ErrNotCompleted.
func resultOf[Res any](results []Result[Res], err error, i int) (Res, error) {
	var zero Res

	switch {
	case err != nil:
		return zero, err
	case i < len(results):
		return results[i].Value, results[i].Err
	default:
		return zero, ErrNotCompleted
	}
}

// panicError wraps a recovered panic value. An error value stays reachable
// through errors.Is and errors.As.
func panicError(p any) error {
	if err, ok := p.(error); ok {
		return fmt.Errorf("batch executor panicked: %w", err)
	}

	return fmt.Errorf("batch executor panicked: %v", p)
}

type delivery[Res any] struct {
	resp Response[Res]
	err  error
}

type entryState int

const (
	// stateQueued means the request waits in the queue of its lane.
	stateQueued entryState = iota
	// stateExecuting means the request is part of a running batch.
	stateExecuting
	// stateDone means the outcome has been sent to the done channel.
	stateDone
)

type entry[Req, Res any] struct {
	ctx      context.Context
	req      Req
	enqueued time.Time
	state    entryState
	done     chan delivery[Res]
}

type lane[Req, Res any] struct {
	// queue holds the queued requests in FIFO order. Guarded by Registry.mu.
	queue []*entry[Req, Res]
	// running is true while a runner goroutine owns the lane. Guarded by
	// Registry.mu.
	running bool
	// kick signals that the queue reached maxSize while a batch is being
	// collected.
	kick chan struct{}
	// maxSize is the effective max size of the lane: the largest number of
	// requests in its batches. It starts at the size kept for the key, or at
	// Config.MaxSize, and is adapted after each batch (see
	// Config.TargetLatency). Guarded by Registry.mu.
	maxSize int
}

// remove deletes e from the queue. The registry lock must be held.
func (l *lane[Req, Res]) remove(e *entry[Req, Res]) {
	for i, queued := range l.queue {
		if queued == e {
			l.queue = append(l.queue[:i], l.queue[i+1:]...)
			return
		}
	}
}
