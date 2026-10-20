package manager

import (
	"context"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	"github.com/feder-cr/git-ai/internal/git/housekeeping/config"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	gitalycfgprom "github.com/feder-cr/git-ai/internal/gitaly/config/prometheus"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/prometheus/client_golang/prometheus"
)

// Manager is a housekeeping manager. It is supposed to handle housekeeping tasks for repositories
// such as the cleanup of unneeded files and optimizations for the repository's data structures.
type Manager interface {
	// CleanStaleData removes any stale data in the repository as per the provided configuration.
	CleanStaleData(context.Context, *localrepo.Repo, housekeeping.CleanStaleDataConfig) error
	// OptimizeRepository optimizes the repository's data structures such that it can be more
	// efficiently served.
	OptimizeRepository(context.Context, *localrepo.Repo, ...OptimizeRepositoryOption) error
	// RunMVCCGarbageCollection removes the MVCC manifests and artifacts a repository no
	// longer needs. It must be called with no MVCC cache open for the repository.
	RunMVCCGarbageCollection(context.Context, *localrepo.Repo, housekeeping.MVCCConfig) error
	// OffloadRepository offloads the repository objects onto a second storage
	OffloadRepository(context.Context, *localrepo.Repo, config.OffloadingConfig) error
	// RehydrateRepository restores an offloaded repository by downloading objects from remote storage
	// back to local storage. The prefix parameter specifies the object prefix in the remote storage
	// where the repository objects are stored.
	RehydrateRepository(context.Context, *localrepo.Repo, string) error
}

// repositoryState holds the housekeeping state for individual repositories. This structure can be
// used to sync between different housekeeping goroutines. It is safer to access this via the methods
// of repositoryStates structure.
type repositoryState struct {
	sync.Mutex

	// isRunning is used to indicate if housekeeping is running.
	isRunning bool
	// backgroundWrite is set while a background commit-graph write runs, see
	// WriteCommitGraphIfIdle. It is closed when the write finishes.
	backgroundWrite chan struct{}
}

// refCountedState keeps count of number of goroutines using a particular repository state, this is used
// to ensure that we only delete a particular state of a repository when there are no goroutines which
// are accessing it.
type refCountedState struct {
	// state is a pointer to a single repositories state.
	state *repositoryState
	// rc keeps count of the number of goroutines using the state.
	rc uint32
}

// repositoryStates holds per-repository information to sync between different goroutines.
// Access to the internal fields should be done via the methods provided by the struct.
type repositoryStates struct {
	sync.Mutex
	// values is map which denotes per-repository housekeeping state.
	values map[string]*refCountedState
}

func repositoryStatesKey(repo storage.Repository) string {
	return repo.GetStorageName() + ":" + repo.GetRelativePath()
}

// getState provides the state and cleanup function for a given repository path.
// The cleanup function deletes the state if the caller is the last goroutine referencing
// the state.
func (s *repositoryStates) getState(repo storage.Repository) (*repositoryState, func()) {
	key := repositoryStatesKey(repo)

	s.Lock()
	defer s.Unlock()

	value, ok := s.values[key]
	if !ok {
		s.values[key] = &refCountedState{
			rc:    0,
			state: &repositoryState{},
		}
		value = s.values[key]
	}

	value.rc++

	return value.state, func() {
		s.Lock()
		defer s.Unlock()

		value.rc--
		if value.rc == 0 {
			delete(s.values, key)
		}
	}
}

// tryRunningHousekeeping denotes if housekeeping can be run on a given repository.
// If successful, it also provides a cleanup function which resets the state so other
// goroutines can run housekeeping on the repository.
func (s *repositoryStates) tryRunningHousekeeping(repo storage.Repository) (successful bool, _ func()) {
	state, cleanup := s.getState(repo)
	defer func() {
		if !successful {
			cleanup()
		}
	}()

	state.Lock()
	defer state.Unlock()

	if state.isRunning {
		return false, nil
	}
	state.isRunning = true

	return true, func() {
		defer cleanup()

		state.Lock()
		defer state.Unlock()

		state.isRunning = false
	}
}

// tryRunningBackgroundWrite denotes if a background commit-graph write can be run on a given
// repository. It fails while housekeeping or another background write runs on the repository. If
// successful, it also provides a cleanup function which marks the write as finished.
func (s *repositoryStates) tryRunningBackgroundWrite(repo storage.Repository) (successful bool, _ func()) {
	state, cleanup := s.getState(repo)
	defer func() {
		if !successful {
			cleanup()
		}
	}()

	state.Lock()
	defer state.Unlock()

	if state.isRunning || state.backgroundWrite != nil {
		return false, nil
	}

	done := make(chan struct{})
	state.backgroundWrite = done

	return true, func() {
		defer cleanup()

		state.Lock()
		defer state.Unlock()

		state.backgroundWrite = nil
		close(done)
	}
}

// waitForBackgroundWrite waits until the background commit-graph write of the repository, if one
// runs, has finished. The caller must have marked housekeeping as running with
// tryRunningHousekeeping, so that no new background write starts afterwards.
func (s *repositoryStates) waitForBackgroundWrite(ctx context.Context, repo storage.Repository) error {
	state, cleanup := s.getState(repo)
	defer cleanup()

	state.Lock()
	done := state.backgroundWrite
	state.Unlock()

	if done == nil {
		return nil
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RepositoryManager is an implementation of the Manager interface.
type RepositoryManager struct {
	logger log.Logger
	// txManager is Praefect's transaction manager using voting mechanism. It will be deprecated soon.
	txManager transaction.Manager
	// node is used to access the storages.
	node storage.Node

	metrics          *housekeeping.Metrics
	optimizeFunc     func(context.Context, *localrepo.Repo, housekeeping.OptimizationStrategy) error
	repositoryStates repositoryStates

	// cacheFactory allows housekeeping to run in the context of an MVCC cache.
	cacheFactory *mvcc.CacheFactory
	// proposalTTL, when set, makes every housekeeping pass prune the proposal references of the
	// land (refs/graft/proposals/) older than it, on every branch (WithGraftProposalTTL).
	proposalTTL time.Duration
}

// Option configures a RepositoryManager.
type Option func(*RepositoryManager)

// WithGraftProposalTTL makes every housekeeping pass prune the proposals of the land
// (proposals.Prune over refs/graft/proposals/) older than ttl and beyond the cap of each branch,
// so that the proposals of idle and deleted branches expire too (PLAN10 14).
func WithGraftProposalTTL(ttl time.Duration) Option {
	return func(m *RepositoryManager) {
		m.proposalTTL = ttl
	}
}

// WithMVCCCacheSupport enables MVCC-snapshot-aware housekeeping for background
// OptimizeRepository runs (daily maintenance, scheduled housekeeping) that do not
// pass through the mvcc gRPC middleware. cacheFactory should be shared with the
// mvcc gRPC middleware so that both use the same ObjectStorage per storage.
func WithMVCCCacheSupport(cacheFactory *mvcc.CacheFactory) Option {
	return func(m *RepositoryManager) {
		m.cacheFactory = cacheFactory
	}
}

// New creates a new RepositoryManager.
func New(promCfg gitalycfgprom.Config, logger log.Logger, txManager transaction.Manager, node storage.Node, opts ...Option) *RepositoryManager {
	m := &RepositoryManager{
		logger:    logger.WithField("system", "housekeeping"),
		txManager: txManager,
		node:      node,
		metrics:   housekeeping.NewMetrics(promCfg),
		repositoryStates: repositoryStates{
			values: make(map[string]*refCountedState),
		},
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Describe is used to describe Prometheus metrics.
func (m *RepositoryManager) Describe(descs chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(m, descs)
}

// Collect is used to collect Prometheus metrics.
func (m *RepositoryManager) Collect(metrics chan<- prometheus.Metric) {
	m.metrics.Collect(metrics)
}
