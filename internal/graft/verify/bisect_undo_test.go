package verify

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// fakeScratch builds scratch commits named "scratch:<base>:<culprits>" and
// records the calls and the releases.
type fakeScratch struct {
	calls      [][]string
	released   int
	err        error
	releaseErr error
}

func (s *fakeScratch) RevertOnto(_ context.Context, base string, culprits []string) (string, func() error, error) {
	s.calls = append(s.calls, append([]string{base}, culprits...))
	if s.err != nil {
		return "", func() error { return nil }, s.err
	}
	return "scratch:" + base + ":" + strings.Join(culprits, ","), func() error {
		s.released++
		return s.releaseErr
	}, nil
}

// undoRange is a first-parent range c0..c(n-1) in which the commits of bad
// break the test, and reverts maps the index of an automatic revert to the
// index of the culprit it undoes.
type undoRange struct {
	commits []CommitInfo
	bad     []int
	reverts map[int]int
}

func newUndoRange(n int, bad []int, reverts map[int]int) *undoRange {
	r := &undoRange{commits: fakeCommits(n), bad: bad, reverts: reverts}
	for j, i := range reverts {
		r.commits[j].CommitterEmail = DefaultRevertCommitterEmail
		r.commits[j].Message = revertMessage(r.commits[i])
	}
	return r
}

func (r *undoRange) index(oid string) int {
	return slices.IndexFunc(r.commits, func(c CommitInfo) bool { return c.ID == oid })
}

// fails reports whether the tree of oid, a commit of the range or a scratch
// commit of fakeScratch, contains a breaking change that is not undone.
func (r *undoRange) fails(oid string, _ int) bool {
	var undone []string
	if rest, ok := strings.CutPrefix(oid, "scratch:"); ok {
		base, culprits, _ := strings.Cut(rest, ":")
		oid = base
		undone = strings.Split(culprits, ",")
	}
	k := r.index(oid)
	for _, b := range r.bad {
		if b > k || slices.Contains(undone, r.commits[b].ID) {
			continue
		}
		reverted := false
		for j, i := range r.reverts {
			if i == b && j <= k {
				reverted = true
			}
		}
		if !reverted {
			return true
		}
	}
	return false
}

func (r *undoRange) finder(scratch ScratchReverter) (*CulpritFinder, *fakeTester) {
	tester := &fakeTester{bad: r.fails}
	return &CulpritFinder{
		Tester:   tester,
		Commits:  &fakeSource{commits: r.commits},
		RevertOf: autoRevertOf(Signature{Name: DefaultRevertCommitterName, Email: DefaultRevertCommitterEmail}),
		Scratch:  scratch,
	}, tester
}

func TestCulpritFinder_UndoesRevertedCulprits(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	t.Run("a second culprit between a culprit and its revert is found", func(t *testing.T) {
		// c1 broke the test and was reverted by c3; c2 broke it too.
		r := newUndoRange(5, []int{1, 2}, map[int]int{3: 1})
		scratch := &fakeScratch{}
		f, tester := r.finder(scratch)

		res, err := f.Find(ctx, "base", r.commits[4].ID)
		require.NoError(t, err)
		require.NotNil(t, res.Culprit)
		require.Equal(t, r.commits[2].ID, res.Culprit.ID)
		require.Equal(t, []string{r.commits[1].ID}, res.Undone)
		require.Equal(t, res.Tests, len(tester.calls))

		require.NotEmpty(t, scratch.calls, "the commits before the revert are tested with c1 undone")
		for _, call := range scratch.calls {
			require.Equal(t, []string{r.commits[1].ID}, call[1:])
		}
		require.Equal(t, len(scratch.calls), scratch.released, "every scratch commit is released")
	})

	t.Run("without the undo the old culprit comes back", func(t *testing.T) {
		r := newUndoRange(5, []int{1, 2}, map[int]int{3: 1})
		f := &CulpritFinder{Tester: &fakeTester{bad: r.fails}, Commits: &fakeSource{commits: r.commits}}

		res, err := f.Find(ctx, "base", r.commits[4].ID)
		require.NoError(t, err)
		require.Equal(t, r.commits[1].ID, res.Culprit.ID, "the plain search blames the reverted culprit")
	})

	t.Run("a second culprit after the revert is found", func(t *testing.T) {
		r := newUndoRange(6, []int{1, 4}, map[int]int{2: 1})
		f, _ := r.finder(&fakeScratch{})

		res, err := f.Find(ctx, "base", r.commits[5].ID)
		require.NoError(t, err)
		require.Equal(t, r.commits[4].ID, res.Culprit.ID)
	})

	t.Run("the revert itself can be the culprit", func(t *testing.T) {
		// c3 reverts c1, and the revert breaks the test.
		r := newUndoRange(5, []int{1, 3}, map[int]int{3: 1})
		f, _ := r.finder(&fakeScratch{})

		res, err := f.Find(ctx, "base", r.commits[4].ID)
		require.NoError(t, err)
		require.Equal(t, r.commits[3].ID, res.Culprit.ID)
	})

	t.Run("a revert of a commit outside the range needs no scratch commit", func(t *testing.T) {
		r := newUndoRange(4, []int{2}, nil)
		r.commits[1].CommitterEmail = DefaultRevertCommitterEmail
		r.commits[1].Message = revertMessage(CommitInfo{ID: "0123456789012345678901234567890123456789", Subject: "old"})
		scratch := &fakeScratch{}
		f, _ := r.finder(scratch)

		res, err := f.Find(ctx, "base", r.commits[3].ID)
		require.NoError(t, err)
		require.Equal(t, r.commits[2].ID, res.Culprit.ID)
		require.Empty(t, scratch.calls)
		require.Empty(t, res.Undone)
	})

	t.Run("a revert by another committer is not taken into account", func(t *testing.T) {
		r := newUndoRange(5, []int{1, 2}, map[int]int{3: 1})
		r.commits[3].CommitterEmail = "agent@example.com"
		scratch := &fakeScratch{}
		f, _ := r.finder(scratch)

		_, err := f.Find(ctx, "base", r.commits[4].ID)
		require.NoError(t, err)
		require.Empty(t, scratch.calls)
	})

	t.Run("a range that needs a scratch commit fails without a scratch reverter", func(t *testing.T) {
		r := newUndoRange(5, []int{1, 2}, map[int]int{3: 1})
		f, _ := r.finder(nil)

		_, err := f.Find(ctx, "base", r.commits[4].ID)
		require.ErrorContains(t, err, "no scratch reverter")
	})

	t.Run("a scratch revert that fails stops the search", func(t *testing.T) {
		r := newUndoRange(5, []int{1, 2}, map[int]int{3: 1})
		f, _ := r.finder(&fakeScratch{err: ErrRevertConflict})

		_, err := f.Find(ctx, "base", r.commits[4].ID)
		require.ErrorIs(t, err, ErrRevertConflict)
	})

	t.Run("a scratch commit that cannot be released is an error", func(t *testing.T) {
		r := newUndoRange(5, []int{1, 2}, map[int]int{3: 1})
		f, _ := r.finder(&fakeScratch{releaseErr: errors.New("hook says no")})

		_, err := f.Find(ctx, "base", r.commits[4].ID)
		require.ErrorContains(t, err, "hook says no")
	})
}

func TestCulpritFinder_UndoneCulpritsOrder(t *testing.T) {
	t.Parallel()

	// c3 reverts c0 and c4 reverts c1.
	r := newUndoRange(6, nil, map[int]int{3: 0, 4: 1})
	f, _ := r.finder(nil)
	id := func(i int) string { return r.commits[i].ID }

	undo := f.undoneCulprits(r.commits)
	require.Equal(t, [][]string{
		{id(0)},
		{id(1), id(0)},
		{id(1), id(0)},
		{id(1)},
		nil,
		nil,
	}, undo)
}

func TestAutoRevertOf(t *testing.T) {
	t.Parallel()
	committer := Signature{Name: DefaultRevertCommitterName, Email: DefaultRevertCommitterEmail}
	culprit := CommitInfo{ID: "1111111111111111111111111111111111111111", Subject: "break"}
	revert := CommitInfo{ID: "r", Message: revertMessage(culprit), CommitterEmail: committer.Email}

	target, ok := autoRevertOf(committer)(revert)
	require.True(t, ok)
	require.Equal(t, culprit.ID, target)

	other := revert
	other.CommitterEmail = "agent@example.com"
	_, ok = autoRevertOf(committer)(other)
	require.False(t, ok, "only the verifier's reverts count")

	truncated := revert
	truncated.Truncated = true
	_, ok = autoRevertOf(committer)(truncated)
	require.False(t, ok)

	middle := revert
	middle.Message = "x\n\n" + AutoRevertTrailer + ": " + culprit.ID + "\n\nnot a trailer block\n"
	_, ok = autoRevertOf(committer)(middle)
	require.False(t, ok, "a trailer outside the last paragraph does not count")

	plain := revert
	plain.Message = "fix things\n"
	_, ok = autoRevertOf(committer)(plain)
	require.False(t, ok)
}

// TestAutoBisector_DecisionsReachLogAndError checks that every decision is
// logged and that a notification that fails is returned, so that a revert is
// never left without a record when the webhook is down.
func TestAutoBisector_DecisionsReachLogAndError(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	newLogger := func() (*slog.Logger, *bytes.Buffer) {
		var buf bytes.Buffer
		return slog.New(slog.NewTextHandler(&buf, nil)), &buf
	}

	t.Run("a revert with a failing notifier", func(t *testing.T) {
		commits := fakeCommits(4)
		tester := &fakeVerdicts{bad: func(oid string) bool { return oid >= commits[2].ID }}
		rev := &fakeReverter{tips: []string{commits[3].ID}}
		logger, buf := newLogger()
		b := &AutoBisector{
			Commits: &fakeSource{commits: commits},
			Reverts: newAutoReverter(rev, &recordingNotifier{err: errors.New("webhook answered 500")}, RevertPolicy{MaxPerHour: 3}),
			Logger:  logger,
		}

		err := b.Bisect(ctx, autoBisectFailure(tester, commits))
		require.Len(t, rev.requests, 1, "the revert is applied")
		require.ErrorContains(t, err, "webhook answered 500")
		require.ErrorContains(t, err, string(StatusReverted))
		require.ErrorContains(t, err, "revert-oid")

		log := buf.String()
		require.Contains(t, log, "bisect decision")
		require.Contains(t, log, "status=reverted")
		require.Contains(t, log, "culprit="+commits[2].ID)
		require.Contains(t, log, "revert_commit=revert-oid")
		require.Contains(t, log, "webhook answered 500")
	})

	t.Run("a refusal with a working notifier", func(t *testing.T) {
		commits := fakeCommits(3)
		tester := &fakeVerdicts{bad: func(string) bool { return true }}
		rev := &fakeReverter{tips: []string{commits[2].ID}}
		logger, buf := newLogger()
		b := &AutoBisector{
			Commits: &fakeSource{commits: commits},
			Reverts: newAutoReverter(rev, &recordingNotifier{}, RevertPolicy{MaxPerHour: 0}),
			Logger:  logger,
		}

		require.NoError(t, b.Bisect(ctx, autoBisectFailure(tester, commits)))
		require.Empty(t, rev.requests)
		require.Contains(t, buf.String(), "status=refused")
	})

	t.Run("a report and a missing baseline are logged", func(t *testing.T) {
		commits := fakeCommits(3)
		tester := &fakeVerdicts{bad: func(string) bool { return true }}
		logger, buf := newLogger()
		b := &AutoBisector{Commits: &fakeSource{commits: commits}, Logger: logger}

		require.NoError(t, b.Bisect(ctx, autoBisectFailure(tester, commits)))
		require.Contains(t, buf.String(), "status=culprit-found")

		f := autoBisectFailure(tester, commits)
		f.Good = ""
		require.ErrorIs(t, b.Bisect(ctx, f), ErrNoBaseline)
		require.Contains(t, buf.String(), "status=no-baseline")
	})
}
