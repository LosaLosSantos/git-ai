package derived

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/prometheus/client_golang/prometheus"
)

// Prometheus metric names of the eager warmers.
const (
	metricWarmQueueDepth = "graft_derived_warm_queue_depth"
	metricWarmDropped    = "graft_derived_warm_dropped_total"
	metricWarmComputed   = "graft_derived_warm_computed_total"
	metricWarmErrors     = "graft_derived_warm_errors_total"
)

// OpenWarmEnvFunc prepares the environment of the warmers for one repository. The
// returned cleanup function is called after every warmer ran on a batch.
type OpenWarmEnvFunc func(ctx context.Context, repo storage.Repository) (WarmEnv, func(), error)

// warmJob is one accepted Enqueue call: the commits of one repository.
type warmJob struct {
	repo    storage.Repository
	commits []git.ObjectID
}

// Pool runs the registered eager warmers in the background. Commits are offered
// without blocking, so a caller never waits for derived data, and a full queue sheds
// work instead of growing without bound: a dropped commit is computed lazily by the
// next reader that needs it.
//
// Pool implements prometheus.Collector with the graft_derived_warm_* metrics.
type Pool struct {
	logger  log.Logger
	workers int
	open    OpenWarmEnvFunc
	queue   chan warmJob

	// stop is closed by Close. Run returns once it is closed.
	stop chan struct{}

	// mu guards closed and the registration of running Run calls, so that Close
	// never waits for a Run that starts afterwards.
	mu      sync.Mutex
	closed  bool
	running sync.WaitGroup

	dropped  atomic.Int64
	computed atomic.Int64
	failed   atomic.Int64
}

// NewPool returns a pool with the given number of workers and queue size in
// batches. Both must be positive. open is called once for every batch of commits.
func NewPool(logger log.Logger, workers, queueSize int, open OpenWarmEnvFunc) *Pool {
	if workers <= 0 {
		panic(fmt.Sprintf("derived pool needs at least one worker, got %d", workers))
	}
	if queueSize <= 0 {
		panic(fmt.Sprintf("derived pool needs a queue size of at least one, got %d", queueSize))
	}

	return &Pool{
		logger:  logger,
		workers: workers,
		open:    open,
		queue:   make(chan warmJob, queueSize),
		stop:    make(chan struct{}),
	}
}

// Enqueue offers commits of repo to the warmers without blocking. When the queue is
// full, or the pool is closed, the commits are dropped and counted.
func (p *Pool) Enqueue(repo storage.Repository, commits []git.ObjectID) {
	if len(commits) == 0 {
		return
	}

	select {
	case <-p.stop:
		p.dropped.Add(int64(len(commits)))
		return
	default:
	}

	select {
	case p.queue <- warmJob{repo: repo, commits: slices.Clone(commits)}:
	default:
		p.dropped.Add(int64(len(commits)))
	}
}

// Run starts the workers and returns once ctx is done or the pool is closed, and
// every worker has stopped. Run called on a closed pool returns immediately.
func (p *Pool) Run(ctx context.Context) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.running.Add(1)
	p.mu.Unlock()
	defer p.running.Done()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-p.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < p.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.work(ctx)
		}()
	}
	wg.Wait()
}

// Close stops the pool. Workers finish the batch they are computing, with their
// context cancelled, and then stop. Batches still queued are dropped and counted.
// Close waits for a running Run to return and is safe to call more than once.
func (p *Pool) Close() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.stop)
	}
	p.mu.Unlock()

	p.running.Wait()

	for {
		select {
		case job := <-p.queue:
			p.dropped.Add(int64(len(job.commits)))
		default:
			return
		}
	}
}

// work processes queued batches until ctx is done.
func (p *Pool) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-p.queue:
			p.process(ctx, job)
		}
	}
}

// process runs every registered warmer on the commits of job. A failing warmer is
// logged and counted, and it does not stop the other warmers.
func (p *Pool) process(ctx context.Context, job warmJob) {
	env, cleanup, err := p.open(ctx, job.repo)
	if err != nil {
		p.failed.Add(1)
		p.logger.WithError(err).WithField("commits", len(job.commits)).Warn("derived: opening warm environment failed")
		return
	}
	if cleanup != nil {
		defer cleanup()
	}
	if env.Logger == nil {
		env.Logger = p.logger
	}

	for _, name := range Warmers() {
		if ctx.Err() != nil {
			return
		}

		fn, ok := lookupWarmer(name)
		if !ok {
			continue
		}

		if err := runWarmer(ctx, fn, env, job.commits); err != nil {
			p.failed.Add(1)
			p.logger.WithError(err).WithField("warmer", name).WithField("commits", len(job.commits)).Warn("derived: eager computation failed")
			continue
		}
		p.computed.Add(int64(len(job.commits)))
	}
}

// runWarmer calls fn and turns a panic into an error, so that a faulty warmer cannot
// take the server down.
func runWarmer(ctx context.Context, fn WarmFunc, env WarmEnv, commits []git.ObjectID) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("warmer panicked: %v", r)
		}
	}()

	return fn(ctx, env, commits)
}

var (
	warmQueueDepthDesc = prometheus.NewDesc(metricWarmQueueDepth,
		"Number of batches of commits waiting for the eager derived-data warmers.", nil, nil)
	warmDroppedDesc = prometheus.NewDesc(metricWarmDropped,
		"Number of commits the eager derived-data warmers dropped because the queue was full or closed.", nil, nil)
	warmComputedDesc = prometheus.NewDesc(metricWarmComputed,
		"Number of derived values computed by the eager warmers, one per warmer and commit.", nil, nil)
	warmErrorsDesc = prometheus.NewDesc(metricWarmErrors,
		"Number of failed eager warmer runs, including failures to open the warm environment.", nil, nil)
)

// Describe implements prometheus.Collector.
func (p *Pool) Describe(ch chan<- *prometheus.Desc) {
	ch <- warmQueueDepthDesc
	ch <- warmDroppedDesc
	ch <- warmComputedDesc
	ch <- warmErrorsDesc
}

// Collect implements prometheus.Collector.
func (p *Pool) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(warmQueueDepthDesc, prometheus.GaugeValue, float64(len(p.queue)))
	ch <- prometheus.MustNewConstMetric(warmDroppedDesc, prometheus.CounterValue, float64(p.dropped.Load()))
	ch <- prometheus.MustNewConstMetric(warmComputedDesc, prometheus.CounterValue, float64(p.computed.Load()))
	ch <- prometheus.MustNewConstMetric(warmErrorsDesc, prometheus.CounterValue, float64(p.failed.Load()))
}
