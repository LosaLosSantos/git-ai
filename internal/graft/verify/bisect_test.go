package verify

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// fakeCommits lists n commits c0..c(n-1), oldest first.
func fakeCommits(n int) []CommitInfo {
	commits := make([]CommitInfo, n)
	for i := range commits {
		id := fmt.Sprintf("%040d", i+1)
		commits[i] = CommitInfo{ID: id, Subject: fmt.Sprintf("change %d", i), Message: fmt.Sprintf("change %d\n", i)}
	}
	return commits
}

// fakeSource returns a fixed first-parent range.
type fakeSource struct {
	commits []CommitInfo
	err     error
	base    string
	tip     string
}

func (s *fakeSource) FirstParentRange(_ context.Context, base, tip string) ([]CommitInfo, error) {
	s.base, s.tip = base, tip
	if s.err != nil {
		return nil, s.err
	}
	return s.commits, nil
}

// fakeTester fails the commits for which bad returns true. call counts the
// runs from 1, so a script can make the first run differ from later ones.
type fakeTester struct {
	bad   func(oid string, call int) bool
	err   error
	calls []string
}

func (f *fakeTester) Test(_ context.Context, oid string) (bool, error) {
	f.calls = append(f.calls, oid)
	if f.err != nil {
		return false, f.err
	}
	return !f.bad(oid, len(f.calls)), nil
}

// failingFrom makes every commit from index i onwards fail.
func failingFrom(commits []CommitInfo, i int) func(string, int) bool {
	return func(oid string, _ int) bool {
		for j := i; j < len(commits); j++ {
			if commits[j].ID == oid {
				return true
			}
		}
		return false
	}
}

func TestCulpritFinder_Find(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	t.Run("culprit in the middle", func(t *testing.T) {
		commits := fakeCommits(10)
		tester := &fakeTester{bad: failingFrom(commits, 4)}
		f := &CulpritFinder{Tester: tester, Commits: &fakeSource{commits: commits}}

		res, err := f.Find(ctx, "base", commits[9].ID)
		require.NoError(t, err)
		require.False(t, res.Flaky)
		require.NotNil(t, res.Culprit)
		require.Equal(t, commits[4], *res.Culprit)
		require.Equal(t, 10, res.Candidates)
		require.Equal(t, res.Tests, len(tester.calls))
		// One flake check plus a binary search over 10 commits.
		require.LessOrEqual(t, res.Tests, 1+4)
	})

	t.Run("culprit is the first commit", func(t *testing.T) {
		commits := fakeCommits(7)
		f := &CulpritFinder{Tester: &fakeTester{bad: failingFrom(commits, 0)}, Commits: &fakeSource{commits: commits}}

		res, err := f.Find(ctx, "base", commits[6].ID)
		require.NoError(t, err)
		require.Equal(t, commits[0], *res.Culprit)
	})

	t.Run("culprit is the tip", func(t *testing.T) {
		commits := fakeCommits(7)
		f := &CulpritFinder{Tester: &fakeTester{bad: failingFrom(commits, 6)}, Commits: &fakeSource{commits: commits}}

		res, err := f.Find(ctx, "base", commits[6].ID)
		require.NoError(t, err)
		require.Equal(t, commits[6], *res.Culprit)
	})

	t.Run("single commit range", func(t *testing.T) {
		commits := fakeCommits(1)
		f := &CulpritFinder{Tester: &fakeTester{bad: failingFrom(commits, 0)}, Commits: &fakeSource{commits: commits}}

		res, err := f.Find(ctx, "base", commits[0].ID)
		require.NoError(t, err)
		require.Equal(t, commits[0], *res.Culprit)
		require.Equal(t, 1, res.Tests)
	})

	t.Run("flaky tip passes on re-run", func(t *testing.T) {
		commits := fakeCommits(5)
		tester := &fakeTester{bad: func(string, int) bool { return false }}
		f := &CulpritFinder{Tester: tester, Commits: &fakeSource{commits: commits}}

		res, err := f.Find(ctx, "base", commits[4].ID)
		require.NoError(t, err)
		require.True(t, res.Flaky)
		require.Nil(t, res.Culprit)
		require.Equal(t, []string{commits[4].ID}, tester.calls, "only the tip is re-run")
	})

	t.Run("range is passed to the source", func(t *testing.T) {
		commits := fakeCommits(3)
		src := &fakeSource{commits: commits}
		f := &CulpritFinder{Tester: &fakeTester{bad: failingFrom(commits, 0)}, Commits: src}

		_, err := f.Find(ctx, "green", commits[2].ID)
		require.NoError(t, err)
		require.Equal(t, "green", src.base)
		require.Equal(t, commits[2].ID, src.tip)
	})

	t.Run("empty range", func(t *testing.T) {
		f := &CulpritFinder{Tester: &fakeTester{bad: failingFrom(nil, 0)}, Commits: &fakeSource{}}

		_, err := f.Find(ctx, "base", "tip")
		require.ErrorIs(t, err, ErrEmptyRange)
	})

	t.Run("base equals tip", func(t *testing.T) {
		f := &CulpritFinder{Tester: &fakeTester{bad: failingFrom(nil, 0)}, Commits: &fakeSource{}}

		_, err := f.Find(ctx, "same", "same")
		require.ErrorIs(t, err, ErrEmptyRange)
	})

	t.Run("range not ending at tip is rejected", func(t *testing.T) {
		commits := fakeCommits(3)
		f := &CulpritFinder{Tester: &fakeTester{bad: failingFrom(commits, 0)}, Commits: &fakeSource{commits: commits}}

		_, err := f.Find(ctx, "base", "someone-else")
		require.ErrorContains(t, err, "does not end at tip")
	})

	t.Run("missing base or tip", func(t *testing.T) {
		f := &CulpritFinder{Tester: &fakeTester{bad: failingFrom(nil, 0)}, Commits: &fakeSource{}}

		_, err := f.Find(ctx, "", "tip")
		require.Error(t, err)
		_, err = f.Find(ctx, "base", "")
		require.Error(t, err)
	})

	t.Run("source failure", func(t *testing.T) {
		f := &CulpritFinder{Tester: &fakeTester{bad: failingFrom(nil, 0)}, Commits: &fakeSource{err: errors.New("boom")}}

		_, err := f.Find(ctx, "base", "tip")
		require.ErrorContains(t, err, "boom")
	})

	t.Run("test that cannot run stops the bisection", func(t *testing.T) {
		commits := fakeCommits(4)
		tester := &fakeTester{bad: failingFrom(commits, 0), err: errors.New("materialize failed")}
		f := &CulpritFinder{Tester: tester, Commits: &fakeSource{commits: commits}}

		res, err := f.Find(ctx, "base", commits[3].ID)
		require.ErrorContains(t, err, "materialize failed")
		require.Nil(t, res.Culprit)
		require.False(t, res.Flaky)
	})

	t.Run("canceled context", func(t *testing.T) {
		commits := fakeCommits(4)
		tester := &fakeTester{bad: failingFrom(commits, 0)}
		f := &CulpritFinder{Tester: tester, Commits: &fakeSource{commits: commits}}

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := f.Find(canceled, "base", commits[3].ID)
		require.ErrorIs(t, err, context.Canceled)
		require.Empty(t, tester.calls)
	})
}
