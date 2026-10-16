package eager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/log"
)

const (
	// maxConcurrentWrites bounds the commit-graph writes that run at the same time
	// across all repositories. A repository never has more than one write at a time.
	maxConcurrentWrites = 2
	// maxBusyRetries bounds how often in a row a write that found the repository
	// busy is retried before it is dropped. Retries are a debounce period apart.
	maxBusyRetries = 60
)

// ErrRepositoryBusy is wrapped by the error of a CommitGraphWriter when the
// repository is busy with other maintenance, such as housekeeping. The Scheduler
// does not treat it as a failure: it retries the write after the debounce
// period.
var ErrRepositoryBusy = errors.New("repository busy")

// repoKey identifies a repository in the scheduler.
type repoKey struct {
	storage string
	relPath string
}

func keyOf(repo storage.Repository) repoKey {
	return repoKey{storage: repo.GetStorageName(), relPath: repo.GetRelativePath()}
}

// Scheduler runs a CommitGraphWriter per repository, debounced. Schedule
// postpones the write until no Schedule call arrived for the debounce period,
// but at most maxWait after the first call of the burst. A Schedule call that
// arrives while a write runs makes the write run once more afterwards, so no
// update is left without a commit-graph write. At most maxConcurrentWrites writes
// run at a time.
type Scheduler struct {
	logger   log.Logger
	write    CommitGraphWriter
	debounce time.Duration
	maxWait  time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	// slots limits the writes running across repositories.
	slots chan struct{}
	// wg tracks the writes in progress, so that Close can wait for them.
	wg sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	pending map[repoKey]*pendingWrite
}

// pendingWrite is the state of a repository that has a write waiting or running.
type pendingWrite struct {
	// repo is the repository of the write, from the latest Schedule call.
	repo storage.Repository
	// timer fires the write when it is armed, identified by gen.
	timer *time.Timer
	gen   uint64
	// firstAt is when the current burst of Schedule calls started.
	firstAt time.Time
	running bool
	// dirty is set when Schedule was called while the write was running.
	dirty bool
	// busyRetries counts the writes in a row that found the repository busy.
	busyRetries int
}

// NewScheduler returns a Scheduler that runs write. Zero durations take the
// defaults. ctx is the parent of the context of the writes, and Close cancels it.
func NewScheduler(ctx context.Context, logger log.Logger, write CommitGraphWriter, debounce, maxWait time.Duration) *Scheduler {
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	if maxWait <= 0 {
		maxWait = DefaultMaxWait
	}
	if maxWait < debounce {
		maxWait = debounce
	}

	ctx, cancel := context.WithCancel(ctx)

	return &Scheduler{
		logger:   logger,
		write:    write,
		debounce: debounce,
		maxWait:  maxWait,
		ctx:      ctx,
		cancel:   cancel,
		slots:    make(chan struct{}, maxConcurrentWrites),
		pending:  make(map[repoKey]*pendingWrite),
	}
}

// Schedule requests a commit-graph write for repo. It never blocks on the write
// itself. After Close it does nothing.
func (s *Scheduler) Schedule(repo storage.Repository) {
	key := keyOf(repo)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}

	p, ok := s.pending[key]
	if !ok {
		p = &pendingWrite{firstAt: time.Now()}
		s.pending[key] = p
	}
	p.repo = repo

	if p.running {
		p.dirty = true
		return
	}

	s.arm(key, p)
}

// arm starts the timer of p, replacing the armed one. The caller must hold s.mu.
func (s *Scheduler) arm(key repoKey, p *pendingWrite) {
	delay := s.debounce
	if remaining := s.maxWait - time.Since(p.firstAt); remaining < delay {
		delay = remaining
	}
	if delay < 0 {
		delay = 0
	}

	if p.timer != nil {
		p.timer.Stop()
	}

	p.gen++
	gen := p.gen
	p.timer = time.AfterFunc(delay, func() { s.fire(key, gen) })
}

// fire runs the write of the repository of key if gen is still the armed timer.
func (s *Scheduler) fire(key repoKey, gen uint64) {
	s.mu.Lock()

	p, ok := s.pending[key]
	if !ok || s.closed || p.gen != gen || p.running {
		s.mu.Unlock()
		return
	}

	p.timer = nil
	p.running = true
	p.dirty = false
	repo := p.repo
	s.wg.Add(1)
	s.mu.Unlock()

	defer s.wg.Done()

	busy := s.runWrite(repo)

	s.mu.Lock()
	defer s.mu.Unlock()

	p.running = false
	if s.closed {
		delete(s.pending, key)
		return
	}

	if busy {
		p.busyRetries++
		if p.busyRetries <= maxBusyRetries {
			// Retry after the debounce period, as if an update had arrived.
			p.dirty = true
		} else {
			p.busyRetries = 0
			s.logger.WithField("storage", repo.GetStorageName()).WithField("relative_path", repo.GetRelativePath()).
				WarnContext(s.ctx, "eager: commit-graph write dropped, the repository stayed busy")
		}
	} else {
		p.busyRetries = 0
	}

	if !p.dirty {
		delete(s.pending, key)
		return
	}

	p.firstAt = time.Now()
	s.arm(key, p)
}

// runWrite waits for a free write slot and writes the commit-graph of repo. A
// failing or panicking write is logged and does not affect the scheduler. It
// reports whether the write found the repository busy and should be retried.
func (s *Scheduler) runWrite(repo storage.Repository) (busy bool) {
	select {
	case s.slots <- struct{}{}:
	case <-s.ctx.Done():
		return false
	}
	defer func() { <-s.slots }()

	defer func() {
		if r := recover(); r != nil {
			s.logger.WithField("panic", fmt.Sprint(r)).ErrorContext(s.ctx, "eager: writing commit-graph panicked")
		}
	}()

	logger := s.logger.WithField("storage", repo.GetStorageName()).WithField("relative_path", repo.GetRelativePath())

	err := s.write(s.ctx, repo)
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrRepositoryBusy):
		logger.WithError(err).DebugContext(s.ctx, "eager: commit-graph write postponed, the repository is busy")
		return true
	default:
		logger.WithError(err).ErrorContext(s.ctx, "eager: writing commit-graph failed")
		return false
	}
}

// Close stops the scheduler. It drops the writes that wait for their timer,
// cancels the context of the writes and waits for the writes in progress. Close
// is idempotent.
func (s *Scheduler) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}

	s.closed = true
	for key, p := range s.pending {
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		if !p.running {
			delete(s.pending, key)
		}
	}
	s.mu.Unlock()

	s.cancel()
	s.wg.Wait()
}
