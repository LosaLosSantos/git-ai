package gitaly

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/eager"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// eagerCounts counts what an eager Observer does with the committed updates.
type eagerCounts struct {
	listed   atomic.Int32
	enqueued atomic.Int32
	writes   atomic.Int32
}

// countingEnqueuer counts the Enqueue calls.
type countingEnqueuer struct{ counts *eagerCounts }

func (e countingEnqueuer) Enqueue(_ storage.Repository, _ []git.ObjectID) {
	e.counts.enqueued.Add(1)
}

// newEagerObserverForTest builds the Observer the way run does, with the options
// from newEagerOptions and counting collaborators.
func newEagerObserverForTest(t *testing.T, cfg config.Cfg) (*eager.Observer, *eagerCounts) {
	t.Helper()

	counts := &eagerCounts{}
	opts := newEagerOptions(cfg)
	opts.Debounce = 5 * time.Millisecond

	observer := eager.NewObserver(testhelper.Context(t), testhelper.SharedLogger(t), opts, eager.Deps{
		Enqueuer: countingEnqueuer{counts: counts},
		ListNewCommits: func(_ context.Context, _ storage.Repository, _, newOID string) ([]git.ObjectID, error) {
			counts.listed.Add(1)
			return []git.ObjectID{git.ObjectID(newOID)}, nil
		},
		WriteCommitGraph: func(_ context.Context, _ storage.Repository) error {
			counts.writes.Add(1)
			return nil
		},
	})
	t.Cleanup(observer.Close)

	return observer, counts
}

func TestNewEagerOptions(t *testing.T) {
	defaults := eager.Options{
		ListWorkers:   config.DefaultGraftDerivedWorkers,
		ListQueueSize: config.DefaultGraftDerivedQueueSize,
	}

	for _, tc := range []struct {
		desc     string
		derived  config.GraftDerived
		expected eager.Options
	}{
		{
			desc:     "plain configuration",
			expected: defaults,
		},
		{
			desc:    "commit graph only",
			derived: config.GraftDerived{CommitGraph: true},
			expected: func() eager.Options {
				o := defaults
				o.CommitGraph = true
				return o
			}(),
		},
		{
			desc:    "eager only",
			derived: config.GraftDerived{Eager: true},
			expected: func() eager.Options {
				o := defaults
				o.Eager = true
				return o
			}(),
		},
		{
			desc:     "disabled overrides eager and commit graph",
			derived:  config.GraftDerived{Disabled: true, Eager: true, CommitGraph: true},
			expected: defaults,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.expected, newEagerOptions(config.Cfg{Graft: config.Graft{Derived: tc.derived}}))
		})
	}
}

// TestEagerObserverByConfiguration checks what the server's Observer does after
// committed updates in the three configurations of the spec.
func TestEagerObserverByConfiguration(t *testing.T) {
	testRepo := &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"}
	updates := []changelog.Update{{
		Reference: "refs/heads/main",
		OldOID:    "0000000000000000000000000000000000000000",
		NewOID:    "1111111111111111111111111111111111111111",
	}}

	t.Run("plain configuration writes nothing and enqueues nothing", func(t *testing.T) {
		observer, counts := newEagerObserverForTest(t, config.Cfg{})

		observer.ObserveCommitted(testhelper.Context(t), testRepo, updates)

		time.Sleep(50 * time.Millisecond)
		require.Zero(t, counts.listed.Load())
		require.Zero(t, counts.enqueued.Load())
		require.Zero(t, counts.writes.Load(), "no commit-graph write without [graft.derived] commit_graph")
	})

	t.Run("commit_graph writes the commit-graph", func(t *testing.T) {
		observer, counts := newEagerObserverForTest(t, config.Cfg{
			Graft: config.Graft{Derived: config.GraftDerived{CommitGraph: true}},
		})

		observer.ObserveCommitted(testhelper.Context(t), testRepo, updates)

		require.Eventually(t, func() bool { return counts.writes.Load() == 1 }, time.Second, time.Millisecond)
		require.Zero(t, counts.enqueued.Load(), "eager derived data is not enabled")
	})

	t.Run("eager enqueues without commit-graph writes", func(t *testing.T) {
		observer, counts := newEagerObserverForTest(t, config.Cfg{
			Graft: config.Graft{Derived: config.GraftDerived{Eager: true}},
		})

		observer.ObserveCommitted(testhelper.Context(t), testRepo, updates)

		require.Eventually(t, func() bool { return counts.enqueued.Load() == 1 }, time.Second, time.Millisecond)
		time.Sleep(50 * time.Millisecond)
		require.Zero(t, counts.writes.Load(), "no commit-graph write without commit_graph")
	})

	t.Run("disabled derived data writes nothing", func(t *testing.T) {
		observer, counts := newEagerObserverForTest(t, config.Cfg{
			Graft: config.Graft{Derived: config.GraftDerived{Disabled: true, Eager: true, CommitGraph: true}},
		})

		observer.ObserveCommitted(testhelper.Context(t), testRepo, updates)

		time.Sleep(50 * time.Millisecond)
		require.Zero(t, counts.listed.Load())
		require.Zero(t, counts.enqueued.Load())
		require.Zero(t, counts.writes.Load())
	})
}
