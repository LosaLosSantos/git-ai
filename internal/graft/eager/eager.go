// Package eager runs the work that Graft does right after references are
// committed. Every committed reference update of a repository reaches the
// Observer from the reference-transaction hook. The Observer enqueues derived
// data for the new commits that the updates made reachable and schedules an
// incremental commit-graph write for the repository. Lands go through the same
// hook, so they trigger both without a separate path. The commit-graph write is
// debounced per repository, so a burst of lands writes it once.
//
// Nothing here ever fails or blocks the reference update: the hook only
// schedules the work, and errors are logged.
package eager

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// DefaultDebounce is the quiet period after the last committed update of a
	// repository before its commit-graph is written. Writes of one repository
	// are therefore at least this far apart.
	DefaultDebounce = 5 * time.Second
	// DefaultMaxWait bounds how long a commit-graph write can be postponed by
	// a steady stream of updates.
	DefaultMaxWait = 30 * time.Second
	// MaxNewCommitsPerUpdate bounds the commits listed for one reference update.
	MaxNewCommitsPerUpdate = 1000
	// DefaultListWorkers is the number of listings of new commits that run at
	// the same time.
	DefaultListWorkers = 2
	// DefaultListQueueSize is the number of committed transactions whose new
	// commits wait for a listing worker.
	DefaultListQueueSize = 1000

	// listTimeout bounds the listing of the new commits of one update.
	listTimeout = time.Minute
)

var droppedListingsDesc = prometheus.NewDesc("graft_eager_dropped_listings_total",
	"Number of committed reference transactions whose new commits were not listed for the eager "+
		"derived-data warmers because the listing queue was full.", nil, nil)

// Enqueuer schedules derived data for commits of a repository. The derived
// Pool implements it. Enqueue never blocks.
type Enqueuer interface {
	Enqueue(repo storage.Repository, commits []git.ObjectID)
}

// NewCommitsFunc lists the commits that moving the reference from oldOID to
// newOID made reachable: those of newOID that oldOID does not reach, at most
// MaxNewCommitsPerUpdate of them. A zero oldOID lists the history of newOID.
type NewCommitsFunc func(ctx context.Context, repo storage.Repository, oldOID, newOID string) ([]git.ObjectID, error)

// CommitGraphWriter writes the commit-graph of the repository.
type CommitGraphWriter func(ctx context.Context, repo storage.Repository) error

// Options configures an Observer.
type Options struct {
	// Eager enables enqueueing of derived data for the new commits of the
	// updated references, see [graft.derived] eager.
	Eager bool
	// CommitGraph enables the debounced commit-graph write, see
	// [graft.derived] commit_graph. Off by default: a zero Options writes no
	// commit-graph outside housekeeping.
	CommitGraph bool
	// Debounce is the quiet period before a commit-graph write. Zero means
	// DefaultDebounce.
	Debounce time.Duration
	// MaxWait bounds the postponement of a commit-graph write. Zero means
	// DefaultMaxWait.
	MaxWait time.Duration
	// ListWorkers is the number of listings of new commits, each a git
	// rev-list, that run at the same time. Zero means DefaultListWorkers.
	ListWorkers int
	// ListQueueSize is the number of committed transactions that wait for a
	// listing worker. When the queue is full, the transaction is dropped and
	// counted. Zero means DefaultListQueueSize.
	ListQueueSize int
}

// Deps are the collaborators of an Observer. A nil collaborator disables the
// trigger that needs it.
type Deps struct {
	Enqueuer         Enqueuer
	ListNewCommits   NewCommitsFunc
	WriteCommitGraph CommitGraphWriter
}

// Observer receives the committed reference updates of repositories.
//
// The new commits are listed by a fixed set of workers from a bounded queue, so
// a burst of committed transactions never runs more than ListWorkers git
// processes at a time. A transaction that finds the queue full is dropped and
// counted: its commits are computed lazily by the next reader that needs them.
//
// Observer implements prometheus.Collector with graft_eager_dropped_listings_total.
type Observer struct {
	logger         log.Logger
	eager          bool
	enqueuer       Enqueuer
	listNewCommits NewCommitsFunc
	scheduler      *Scheduler

	// listQueue holds the transactions whose new commits wait for a listing
	// worker.
	listQueue chan listJob
	// droppedListings counts the transactions dropped because listQueue was full.
	droppedListings atomic.Int64

	// ctx is cancelled by Close. It is the parent of the background listings.
	ctx    context.Context
	cancel context.CancelFunc

	// mu guards closed. wg tracks the listing workers.
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// listJob is the listing of the new commits of one committed transaction.
type listJob struct {
	repo    storage.Repository
	targets []updateTarget
}

// NewObserver returns an Observer. The Observer owns its scheduler and its
// listing workers, so the caller must Close it.
func NewObserver(ctx context.Context, logger log.Logger, opts Options, deps Deps) *Observer {
	ctx, cancel := context.WithCancel(ctx)

	o := &Observer{
		logger:         logger,
		eager:          opts.Eager && deps.Enqueuer != nil && deps.ListNewCommits != nil,
		enqueuer:       deps.Enqueuer,
		listNewCommits: deps.ListNewCommits,
		ctx:            ctx,
		cancel:         cancel,
	}

	if o.eager {
		workers := opts.ListWorkers
		if workers <= 0 {
			workers = DefaultListWorkers
		}
		queueSize := opts.ListQueueSize
		if queueSize <= 0 {
			queueSize = DefaultListQueueSize
		}

		o.listQueue = make(chan listJob, queueSize)
		for i := 0; i < workers; i++ {
			o.wg.Add(1)
			go o.listWorker()
		}
	}

	if opts.CommitGraph && deps.WriteCommitGraph != nil {
		o.scheduler = NewScheduler(ctx, logger, deps.WriteCommitGraph, opts.Debounce, opts.MaxWait)
	}

	return o
}

// ObserveCommitted is called with the reference updates that were committed to
// repo. It never blocks on Git and never fails the update: the new commits are
// listed and enqueued in the background, and a panic is recovered and logged.
// Deleted references do not enqueue derived data, but they still schedule a
// commit-graph write because they change reachability.
func (o *Observer) ObserveCommitted(ctx context.Context, repo storage.Repository, updates []changelog.Update) {
	if o == nil || len(updates) == 0 {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			o.logger.WithField("panic", fmt.Sprint(r)).ErrorContext(ctx, "eager work after committed updates panicked")
		}
	}()

	if o.eager {
		o.enqueueNewCommits(ctx, repo, updates)
	}

	if o.scheduler != nil {
		o.scheduler.Schedule(repo)
	}
}

// Close stops the background work. Listings in progress are cancelled, queued
// listings and pending commit-graph writes are dropped, since derived data is
// computed lazily and the commit-graph is only an index that Git rebuilds
// lazily, and Close waits for the work in progress. Close is idempotent.
func (o *Observer) Close() {
	if o == nil {
		return
	}

	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed = true
	o.mu.Unlock()

	o.cancel()
	o.wg.Wait()

	if o.scheduler != nil {
		o.scheduler.Close()
	}
}

// enqueueNewCommits queues the listing of the new commits of the updated
// references for the listing workers. It never blocks: when the queue is full,
// the transaction is dropped and counted.
func (o *Observer) enqueueNewCommits(ctx context.Context, repo storage.Repository, updates []changelog.Update) {
	targets := updatedTargets(updates)
	if len(targets) == 0 {
		return
	}

	o.mu.Lock()
	closed := o.closed
	o.mu.Unlock()
	if closed {
		return
	}

	select {
	case o.listQueue <- listJob{repo: repo, targets: targets}:
	default:
		o.droppedListings.Add(1)
		o.logger.WithField("storage", repo.GetStorageName()).WithField("relative_path", repo.GetRelativePath()).
			DebugContext(ctx, "eager: listing queue full, new commits are not warmed")
	}
}

// listWorker lists and enqueues the new commits of queued transactions until
// Close.
func (o *Observer) listWorker() {
	defer o.wg.Done()

	for {
		select {
		case <-o.ctx.Done():
			return
		case job := <-o.listQueue:
			if o.ctx.Err() != nil {
				return
			}
			o.runListJob(job)
		}
	}
}

// runListJob runs one listing. A panic is recovered and logged, so that the
// worker keeps running.
func (o *Observer) runListJob(job listJob) {
	defer func() {
		if r := recover(); r != nil {
			o.logger.WithField("panic", fmt.Sprint(r)).ErrorContext(o.ctx, "eager: enqueueing new commits panicked")
		}
	}()

	o.enqueueTargets(o.ctx, job.repo, job.targets)
}

// Describe implements prometheus.Collector.
func (o *Observer) Describe(ch chan<- *prometheus.Desc) {
	ch <- droppedListingsDesc
}

// Collect implements prometheus.Collector.
func (o *Observer) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(droppedListingsDesc, prometheus.CounterValue, float64(o.droppedListings.Load()))
}

// enqueueTargets lists the new commits of every target and enqueues the distinct
// ones together.
func (o *Observer) enqueueTargets(ctx context.Context, repo storage.Repository, targets []updateTarget) {
	listCtx, cancel := context.WithTimeout(o.ctx, listTimeout)
	defer cancel()

	var commits []git.ObjectID
	seen := make(map[git.ObjectID]struct{})

	for _, target := range targets {
		listed, err := o.listNewCommits(listCtx, repo, target.oldOID, target.newOID)
		if err != nil {
			o.logger.WithError(err).WithField("reference", target.reference).WarnContext(ctx, "eager: listing new commits failed")
			continue
		}

		for _, oid := range listed {
			if _, ok := seen[oid]; ok {
				continue
			}
			seen[oid] = struct{}{}
			commits = append(commits, oid)
		}
	}

	if len(commits) == 0 || listCtx.Err() != nil {
		return
	}

	o.enqueuer.Enqueue(repo, commits)
}

// updateTarget is one committed update of a reference to a new value.
type updateTarget struct {
	reference string
	oldOID    string
	newOID    string
}

// updatedTargets returns the updates that set a reference to a new value, in
// the order of the updates. Deletions are skipped, and so are the updates that
// repeat an earlier one.
func updatedTargets(updates []changelog.Update) []updateTarget {
	var targets []updateTarget
	seen := make(map[updateTarget]struct{}, len(updates))

	for _, update := range updates {
		if changelog.IsZeroOID(update.NewOID) {
			continue
		}

		target := updateTarget{reference: update.Reference, oldOID: update.OldOID, newOID: update.NewOID}
		if _, ok := seen[target]; ok {
			continue
		}

		seen[target] = struct{}{}
		targets = append(targets, target)
	}

	return targets
}
