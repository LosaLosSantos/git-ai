package reviewroots

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/dbreview"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
)

// Triggers of a sweep, the label of the sweep metrics.
const (
	// TriggerClose is the sweep of the commits of a review that was just closed.
	TriggerClose = "close"
	// TriggerHousekeeping is the backstop that runs with the object
	// housekeeping of a repository (Housekeep).
	TriggerHousekeeping = "housekeeping"
)

// Sweeper runs the sweeps and the backfill of the review roots and counts
// them, so that a failure that its caller does not return (the sweep of a
// close, which is recorded already) is visible, and a persistent one shows as
// a growing counter.
type Sweeper struct {
	deleted   prometheus.Counter
	protected prometheus.Counter
	missing   prometheus.Counter
	failures  *prometheus.CounterVec
}

var _ prometheus.Collector = (*Sweeper)(nil)

// DefaultSweeper is the sweeper of the process; gitaly serve registers it.
var DefaultSweeper = NewSweeper()

// NewSweeper returns a sweeper with its own counters.
func NewSweeper() *Sweeper {
	s := &Sweeper{
		deleted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_review_roots_deleted_total",
			Help: "Number of keep-around references of review commits deleted because no open review uses them.",
		}),
		protected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_review_roots_backfilled_total",
			Help: "Number of keep-around references of open-review commits written by the housekeeping backfill.",
		}),
		missing: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_review_roots_missing_total",
			Help: "Number of open-review commits found missing from a repository by the housekeeping backfill.",
		}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "graft_review_roots_sweep_failures_total",
			Help: "Number of sweeps of review keep-around references that failed, by trigger (close, housekeeping).",
		}, []string{"trigger"}),
	}
	for _, trigger := range []string{TriggerClose, TriggerHousekeeping} {
		s.failures.WithLabelValues(trigger)
	}
	return s
}

// Describe implements prometheus.Collector.
func (s *Sweeper) Describe(descs chan<- *prometheus.Desc) {
	s.deleted.Describe(descs)
	s.protected.Describe(descs)
	s.missing.Describe(descs)
	s.failures.Describe(descs)
}

// Collect implements prometheus.Collector.
func (s *Sweeper) Collect(metrics chan<- prometheus.Metric) {
	s.deleted.Collect(metrics)
	s.protected.Collect(metrics)
	s.missing.Collect(metrics)
	s.failures.Collect(metrics)
}

// Sweep runs Sweep and counts it under trigger.
func (s *Sweeper) Sweep(ctx context.Context, repo *localrepo.Repo, reviews store.ReviewStore, candidates []git.ObjectID, trigger string) (SweepReport, error) {
	report, err := Sweep(ctx, repo, reviews, candidates)
	s.deleted.Add(float64(report.Deleted))
	if err != nil {
		s.failures.WithLabelValues(trigger).Inc()
	}
	return report, err
}

// Housekeep is the backstop of the review roots, run with the object
// housekeeping of the repository (gitaly's daily maintenance) in every mode:
// it writes the keep-around references that open reviews lack (Backfill) and
// deletes those that no open review uses (Sweep of every keep-around
// reference), so that a reference a close could not delete, or one written
// before a crash, does not keep its objects forever. In authoritative mode the
// reviews are the database's and the local repository is caught up first (fresh
// is the node's Freshener); an untracked repository, or one whose import is
// not complete, has no reviews in the database and is skipped. In off and
// shadow mode (core nil or not authoritative) the reviews are the files of the
// repository.
func (s *Sweeper) Housekeep(ctx context.Context, core *corestore.Core, fresh corestore.Freshener, repo *localrepo.Repo, repoProto storage.Repository) (BackfillReport, SweepReport, error) {
	reviews, ok, err := housekeepingStore(ctx, core, fresh, repo, repoProto)
	if err != nil {
		s.failures.WithLabelValues(TriggerHousekeeping).Inc()
		return BackfillReport{}, SweepReport{}, err
	}
	if !ok {
		return BackfillReport{}, SweepReport{}, nil
	}

	backfill, err := Backfill(ctx, repo, reviews)
	s.protected.Add(float64(backfill.Protected))
	s.missing.Add(float64(len(backfill.Missing)))
	if err != nil {
		s.failures.WithLabelValues(TriggerHousekeeping).Inc()
		return backfill, SweepReport{}, fmt.Errorf("backfill: %w", err)
	}

	sweep, err := s.Sweep(ctx, repo, reviews, nil, TriggerHousekeeping)
	if err != nil {
		return backfill, sweep, fmt.Errorf("sweep: %w", err)
	}
	return backfill, sweep, nil
}

// housekeepingStore returns the review store of the repository for Housekeep;
// ok is false when the repository has none to keep.
func housekeepingStore(ctx context.Context, core *corestore.Core, fresh corestore.Freshener, repo *localrepo.Repo, repoProto storage.Repository) (store.ReviewStore, bool, error) {
	if !core.Authoritative() {
		path, err := repo.Path(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("repository path: %w", err)
		}
		return store.Open(path), true, nil
	}

	record, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repoProto))
	switch {
	case errors.Is(err, corestore.ErrRepositoryNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("storage core: %w", err)
	case record.HeadSeq == 0:
		return nil, false, nil
	}
	if err := fresh.EnsureFresh(ctx, repoProto); err != nil {
		return nil, false, fmt.Errorf("catch up: %w", err)
	}
	return dbreview.New(ctx, core.Meta, record.ID), true, nil
}
