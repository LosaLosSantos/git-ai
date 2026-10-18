package reviewroots_test

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/reviewroots"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// counter returns the value of the counter family name of the sweeper (with
// the trigger label when trigger is not empty).
func counter(t *testing.T, s *reviewroots.Sweeper, name, trigger string) float64 {
	t.Helper()

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(s))
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if trigger == "" {
				return metric.GetCounter().GetValue()
			}
			for _, label := range metric.GetLabel() {
				if label.GetName() == "trigger" && label.GetValue() == trigger {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// brokenStore is a review store whose listing fails.
type brokenStore struct{ store.ReviewStore }

func (brokenStore) List(string, bool, int) ([]*gitalypb.Review, error) {
	return nil, errors.New("the review files cannot be read")
}

func TestSweeperCounts(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	c0 := f.write("c0")
	require.NoError(t, reviewroots.Protect(f.ctx, f.repo, []git.ObjectID{c0}))

	s := reviewroots.NewSweeper()
	_, err := s.Sweep(f.ctx, f.repo, brokenStore{}, nil, reviewroots.TriggerClose)
	require.Error(t, err)
	require.Equal(t, float64(1), counter(t, s, "graft_review_roots_sweep_failures_total", reviewroots.TriggerClose))
	require.Zero(t, counter(t, s, "graft_review_roots_sweep_failures_total", reviewroots.TriggerHousekeeping))

	report, err := s.Sweep(f.ctx, f.repo, f.reviews, nil, reviewroots.TriggerClose)
	require.NoError(t, err)
	require.Equal(t, 1, report.Deleted)
	require.Equal(t, float64(1), counter(t, s, "graft_review_roots_deleted_total", ""))
	require.Equal(t, float64(1), counter(t, s, "graft_review_roots_sweep_failures_total", reviewroots.TriggerClose))
}

// TestHousekeepWithoutTheStorageCore is the backstop in off and shadow mode:
// the reviews are the files of the repository; the references open reviews
// lack are written, and a leaked one (a close whose sweep failed) is deleted.
func TestHousekeepWithoutTheStorageCore(t *testing.T) {
	t.Parallel()

	for _, mode := range []corestore.Mode{corestore.ModeOff, corestore.ModeShadow} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			c0 := f.write("c0")
			c1 := f.write("c1", c0)
			leaked := f.write("leaked", c0)
			f.review(c1.String(), c0.String(), gitalypb.Review_STATE_OPEN_UNSPECIFIED)
			require.NoError(t, reviewroots.Protect(f.ctx, f.repo, []git.ObjectID{leaked}))

			var core *corestore.Core
			if mode == corestore.ModeShadow {
				core = coretest.Open(t, coretest.Options{Mode: corestore.ModeShadow})
			}

			s := reviewroots.NewSweeper()
			backfill, sweep, err := s.Housekeep(f.ctx, core, corestore.NopFreshener{}, f.repo, f.repo)
			require.NoError(t, err)
			require.Equal(t, 2, backfill.Protected)
			require.Empty(t, backfill.Missing)
			require.Equal(t, reviewroots.SweepReport{Deleted: 1, Kept: 2}, sweep)
			require.Equal(t, keepAroundOf(c0, c1), f.keepArounds())

			require.Equal(t, float64(2), counter(t, s, "graft_review_roots_backfilled_total", ""))
			require.Equal(t, float64(1), counter(t, s, "graft_review_roots_deleted_total", ""))
			require.Zero(t, counter(t, s, "graft_review_roots_sweep_failures_total", reviewroots.TriggerHousekeeping))
		})
	}
}

// TestHousekeepSkipsUntrackedRepositories: in authoritative mode the reviews
// are the database's; a repository the database does not track has none, and
// its local references are left alone.
func TestHousekeepSkipsUntrackedRepositories(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	c0 := f.write("c0")
	require.NoError(t, reviewroots.Protect(f.ctx, f.repo, []git.ObjectID{c0}))

	core := coretest.Open(t, coretest.Options{Mode: corestore.ModeAuthoritative})
	s := reviewroots.NewSweeper()
	backfill, sweep, err := s.Housekeep(f.ctx, core, corestore.NopFreshener{}, f.repo, f.repo)
	require.NoError(t, err)
	require.Equal(t, reviewroots.BackfillReport{}, backfill)
	require.Equal(t, reviewroots.SweepReport{}, sweep)
	require.Equal(t, keepAroundOf(c0), f.keepArounds())
}
