package verify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// fakeVerdicts is a Tester whose verdict per commit comes from bad. Calls are
// recorded in order.
type fakeVerdicts struct {
	bad   func(oid string) bool
	timed map[string]bool
	err   error
	calls []string
}

func (f *fakeVerdicts) Test(_ context.Context, commit string) (TestResult, error) {
	f.calls = append(f.calls, commit)
	if f.err != nil {
		return TestResult{}, f.err
	}
	if f.timed[commit] {
		return TestResult{Commit: commit, TimedOut: true}, nil
	}
	return TestResult{Commit: commit, Passed: !f.bad(commit)}, nil
}

func autoBisectFailure(t Tester, commits []CommitInfo) Failure {
	return Failure{
		Branch: "main",
		Good:   "base",
		Bad:    commits[len(commits)-1].ID,
		Tester: t,
	}
}

func TestAutoBisector_NoBaselineIsNotified(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	commits := fakeCommits(3)
	tester := &fakeVerdicts{bad: func(string) bool { return true }}

	t.Run("the first failure of a branch is notified and not bisected", func(t *testing.T) {
		notes := &recordingNotifier{}
		b := &AutoBisector{Commits: &fakeSource{commits: commits}, Notifier: notes}
		f := autoBisectFailure(tester, commits)
		f.Good = ""

		require.ErrorIs(t, b.Bisect(ctx, f), ErrNoBaseline)
		require.Empty(t, tester.calls)
		require.Len(t, notes.outcomes, 1)
		require.Equal(t, StatusNoBaseline, notes.outcomes[0].Status)
		require.Equal(t, f.Branch, notes.outcomes[0].Branch)
		require.Equal(t, f.Bad, notes.outcomes[0].Tip)
	})

	t.Run("a failing notifier is returned with the sentinel", func(t *testing.T) {
		notes := &recordingNotifier{err: errors.New("webhook down")}
		b := &AutoBisector{Commits: &fakeSource{commits: commits}, Notifier: notes}
		f := autoBisectFailure(tester, commits)
		f.Good = ""

		err := b.Bisect(ctx, f)
		require.ErrorIs(t, err, ErrNoBaseline)
		require.ErrorContains(t, err, "webhook down")
	})
}

func TestVerdictTester(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	passing := verdictTester{tester: &fakeVerdicts{bad: func(string) bool { return false }}}
	ok, err := passing.Test(ctx, "c")
	require.NoError(t, err)
	require.True(t, ok)

	failing := verdictTester{tester: &fakeVerdicts{bad: func(string) bool { return true }}}
	ok, err = failing.Test(ctx, "c")
	require.NoError(t, err)
	require.False(t, ok)

	timed := verdictTester{tester: &fakeVerdicts{bad: func(string) bool { return false }, timed: map[string]bool{"c": true}}}
	ok, err = timed.Test(ctx, "c")
	require.NoError(t, err)
	require.False(t, ok, "a timed out run is a failure")

	broken := verdictTester{tester: &fakeVerdicts{err: errors.New("cannot start")}}
	_, err = broken.Test(ctx, "c")
	require.ErrorContains(t, err, "cannot start")
}

func TestAutoBisector_Bisect(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	t.Run("reverts the culprit onto the branch", func(t *testing.T) {
		commits := fakeCommits(10)
		tester := &fakeVerdicts{bad: func(oid string) bool { return oid >= commits[4].ID }}
		rev := &fakeReverter{tips: []string{commits[9].ID}}
		notes := &recordingNotifier{}
		b := &AutoBisector{
			Commits: &fakeSource{commits: commits},
			Reverts: newAutoReverter(rev, notes, RevertPolicy{MaxPerHour: 3}),
		}

		require.NoError(t, b.Bisect(ctx, autoBisectFailure(tester, commits)))

		require.Len(t, rev.requests, 1)
		require.Equal(t, "main", rev.requests[0].Branch)
		require.Equal(t, commits[4].ID, rev.requests[0].Commit)
		require.Equal(t, commits[9].ID, rev.requests[0].ExpectedOld)
		require.Len(t, notes.outcomes, 1)
		require.Equal(t, StatusReverted, notes.outcomes[0].Status)
		require.Equal(t, commits[4].ID, notes.outcomes[0].Culprit)
	})

	t.Run("reports the culprit without reverting when reverts are disabled", func(t *testing.T) {
		commits := fakeCommits(6)
		tester := &fakeVerdicts{bad: func(oid string) bool { return oid >= commits[2].ID }}
		notes := &recordingNotifier{}
		b := &AutoBisector{
			Commits:  &fakeSource{commits: commits},
			Notifier: notes,
			Now:      func() time.Time { return now },
		}

		require.NoError(t, b.Bisect(ctx, autoBisectFailure(tester, commits)))
		require.Len(t, notes.outcomes, 1)
		out := notes.outcomes[0]
		require.Equal(t, StatusCulprit, out.Status)
		require.Equal(t, commits[2].ID, out.Culprit)
		require.Equal(t, commits[2].Subject, out.Subject)
		require.Equal(t, "main", out.Branch)
		require.Equal(t, commits[5].ID, out.Tip)
		require.Equal(t, now, out.At)
		require.Empty(t, out.RevertCommit)
	})

	t.Run("report only with no notifier succeeds", func(t *testing.T) {
		commits := fakeCommits(3)
		tester := &fakeVerdicts{bad: func(string) bool { return true }}
		b := &AutoBisector{Commits: &fakeSource{commits: commits}}
		require.NoError(t, b.Bisect(ctx, autoBisectFailure(tester, commits)))
	})

	t.Run("failing notifier in report-only mode is returned", func(t *testing.T) {
		commits := fakeCommits(3)
		tester := &fakeVerdicts{bad: func(string) bool { return true }}
		b := &AutoBisector{
			Commits:  &fakeSource{commits: commits},
			Notifier: &recordingNotifier{err: errors.New("webhook down")},
		}
		err := b.Bisect(ctx, autoBisectFailure(tester, commits))
		require.ErrorContains(t, err, "webhook down")
	})

	t.Run("flaky tip is reported and not searched", func(t *testing.T) {
		commits := fakeCommits(5)
		tester := &fakeVerdicts{bad: func(string) bool { return false }}
		notes := &recordingNotifier{}
		b := &AutoBisector{Commits: &fakeSource{commits: commits}, Notifier: notes}

		require.NoError(t, b.Bisect(ctx, autoBisectFailure(tester, commits)))
		require.Equal(t, []string{commits[4].ID}, tester.calls, "only the flake check runs")
		require.Len(t, notes.outcomes, 1)
		require.Equal(t, StatusFlaky, notes.outcomes[0].Status)
	})

	t.Run("flaky tip with reverts enabled is not reverted", func(t *testing.T) {
		commits := fakeCommits(5)
		tester := &fakeVerdicts{bad: func(string) bool { return false }}
		rev := &fakeReverter{tips: []string{commits[4].ID}}
		b := &AutoBisector{
			Commits: &fakeSource{commits: commits},
			Reverts: newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3}),
		}

		require.NoError(t, b.Bisect(ctx, autoBisectFailure(tester, commits)))
		require.Empty(t, rev.requests)
	})

	t.Run("no green baseline is refused", func(t *testing.T) {
		commits := fakeCommits(3)
		tester := &fakeVerdicts{bad: func(string) bool { return true }}
		b := &AutoBisector{Commits: &fakeSource{commits: commits}}

		f := autoBisectFailure(tester, commits)
		f.Good = ""
		require.ErrorIs(t, b.Bisect(ctx, f), ErrNoBaseline)
		require.Empty(t, tester.calls)
	})

	t.Run("missing tester and missing commit source are refused", func(t *testing.T) {
		commits := fakeCommits(3)
		b := &AutoBisector{Commits: &fakeSource{commits: commits}}
		f := autoBisectFailure(nil, commits)
		require.Error(t, b.Bisect(ctx, f))

		noSource := &AutoBisector{}
		require.Error(t, noSource.Bisect(ctx, autoBisectFailure(&fakeVerdicts{bad: func(string) bool { return true }}, commits)))
	})

	t.Run("base equal to tip is reported as an empty range", func(t *testing.T) {
		commits := fakeCommits(3)
		tester := &fakeVerdicts{bad: func(string) bool { return true }}
		b := &AutoBisector{Commits: &fakeSource{commits: commits}}

		f := autoBisectFailure(tester, commits)
		f.Good = f.Bad
		require.ErrorIs(t, b.Bisect(ctx, f), ErrEmptyRange)
	})

	t.Run("search failure is returned", func(t *testing.T) {
		commits := fakeCommits(3)
		tester := &fakeVerdicts{bad: func(string) bool { return true }}
		b := &AutoBisector{Commits: &fakeSource{err: errors.New("list failed")}}

		err := b.Bisect(ctx, autoBisectFailure(tester, commits))
		require.ErrorContains(t, err, "list failed")
		require.ErrorContains(t, err, "bisect base")
	})

	t.Run("test that cannot run is returned", func(t *testing.T) {
		commits := fakeCommits(3)
		tester := &fakeVerdicts{bad: func(string) bool { return true }, err: errors.New("no shell")}
		b := &AutoBisector{Commits: &fakeSource{commits: commits}}

		err := b.Bisect(ctx, autoBisectFailure(tester, commits))
		require.ErrorContains(t, err, "no shell")
	})

	t.Run("timed out tip is a failure that gets bisected", func(t *testing.T) {
		commits := fakeCommits(4)
		tester := &fakeVerdicts{
			bad:   func(oid string) bool { return oid == commits[3].ID },
			timed: map[string]bool{commits[3].ID: true},
		}
		notes := &recordingNotifier{}
		b := &AutoBisector{Commits: &fakeSource{commits: commits}, Notifier: notes}

		require.NoError(t, b.Bisect(ctx, autoBisectFailure(tester, commits)))
		require.Equal(t, StatusCulprit, notes.outcomes[0].Status)
		require.Equal(t, commits[3].ID, notes.outcomes[0].Culprit)
	})

	t.Run("a revert failure is returned", func(t *testing.T) {
		commits := fakeCommits(3)
		tester := &fakeVerdicts{bad: func(string) bool { return true }}
		rev := &fakeReverter{tips: []string{commits[2].ID}, steps: []revertStep{{err: errors.New("hook exploded")}}}
		b := &AutoBisector{
			Commits: &fakeSource{commits: commits},
			Reverts: newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3}),
		}

		err := b.Bisect(ctx, autoBisectFailure(tester, commits))
		require.ErrorContains(t, err, "hook exploded")
	})
}
