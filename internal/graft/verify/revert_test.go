package verify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// revertStep is one scripted answer of fakeReverter.Revert.
type revertStep struct {
	id  string
	err error
}

// fakeReverter returns the scripted steps in order and records the requests.
// tips are returned by Tip in order; the last one repeats. The reverts it
// performs are the branch history it reports to the cap.
type fakeReverter struct {
	tips     []string
	steps    []revertStep
	tipCalls int
	requests []RevertRequest
	history  []CommitInfo
}

func (f *fakeReverter) Tip(_ context.Context, _ string) (string, error) {
	i := f.tipCalls
	if i >= len(f.tips) {
		i = len(f.tips) - 1
	}
	f.tipCalls++
	return f.tips[i], nil
}

func (f *fakeReverter) Revert(_ context.Context, req RevertRequest) (string, error) {
	f.requests = append(f.requests, req)
	id, err := "revert-oid", error(nil)
	if len(f.steps) > 0 {
		step := f.steps[0]
		if len(f.steps) > 1 {
			f.steps = f.steps[1:]
		}
		id, err = step.id, step.err
	}
	if err == nil {
		f.history = append(f.history, revertCommit(id, req))
	}
	return id, err
}

// CommitsSince implements RevertHistory.
func (f *fakeReverter) CommitsSince(_ context.Context, _ string, since time.Time) ([]CommitInfo, error) {
	return commitsSince(f.history, since), nil
}

// revertCommit is the commit the server makes for a revert request.
func revertCommit(id string, req RevertRequest) CommitInfo {
	return CommitInfo{ID: id, Subject: "revert", Message: req.Message, CommitterEmail: req.Committer.Email, CommittedAt: req.At}
}

// commitsSince returns the commits of list at or after since.
func commitsSince(list []CommitInfo, since time.Time) []CommitInfo {
	var out []CommitInfo
	for _, c := range list {
		if !c.CommittedAt.Before(since) {
			out = append(out, c)
		}
	}
	return out
}

// recordingNotifier keeps every outcome it was given.
type recordingNotifier struct {
	outcomes []RevertOutcome
	err      error
}

func (n *recordingNotifier) Notify(_ context.Context, out RevertOutcome) error {
	n.outcomes = append(n.outcomes, out)
	return n.err
}

var (
	culpritCommit = CommitInfo{
		ID:      "1111111111111111111111111111111111111111",
		Subject: "break the parser",
		Message: "break the parser\n\nDetails.\n",
	}
	testTip = "2222222222222222222222222222222222222222"
)

func failedResult(c CommitInfo) *BisectResult {
	return &BisectResult{Base: "base", Tip: testTip, Candidates: 3, Tests: 3, Culprit: &c}
}

func newAutoReverter(r Reverter, n Notifier, p RevertPolicy) *AutoReverter {
	return &AutoReverter{Reverter: r, History: r.(RevertHistory), Notifier: n, Policy: p, Now: func() time.Time {
		return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	}}
}

func TestAutoReverter_Apply(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	t.Run("reverts the culprit onto the tip", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		notes := &recordingNotifier{}
		a := newAutoReverter(rev, notes, RevertPolicy{MaxPerHour: 3, Committer: Signature{Name: "graft-verify", Email: "v@example.com"}})

		out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
		require.Equal(t, "revert-oid", out.RevertCommit)
		require.Equal(t, culpritCommit.ID, out.Culprit)
		require.Equal(t, testTip, out.Tip)

		require.Len(t, rev.requests, 1)
		req := rev.requests[0]
		require.Equal(t, "main", req.Branch)
		require.Equal(t, culpritCommit.ID, req.Commit)
		require.Equal(t, testTip, req.ExpectedOld)
		require.Equal(t, Signature{Name: "graft-verify", Email: "v@example.com"}, req.Committer)
		require.Equal(t, "Revert \"break the parser\"\n\nThis reverts commit "+culpritCommit.ID+
			", which failed the verification test command.\n\n"+AutoRevertTrailer+": "+culpritCommit.ID+"\n", req.Message)

		require.Equal(t, []RevertOutcome{out}, notes.outcomes)
	})

	t.Run("dry run reverts nothing", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		notes := &recordingNotifier{}
		a := newAutoReverter(rev, notes, RevertPolicy{DryRun: true, MaxPerHour: 3})

		out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusDryRun, out.Status)
		require.True(t, out.DryRun)
		require.Empty(t, rev.requests)
		require.Len(t, notes.outcomes, 1)
	})

	t.Run("dry run does not consume the cap", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		a := newAutoReverter(rev, nil, RevertPolicy{DryRun: true, MaxPerHour: 1})
		for range 3 {
			out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
			require.NoError(t, err)
			require.Equal(t, StatusDryRun, out.Status)
		}
	})

	t.Run("auto-revert culprit is refused", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		c := CommitInfo{
			ID:      culpritCommit.ID,
			Subject: "Revert \"something\"",
			Message: "Revert \"something\"\n\n" + AutoRevertTrailer + ": 3333333333333333333333333333333333333333\n",
		}
		out, err := newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", failedResult(c))
		require.NoError(t, err)
		require.Equal(t, StatusRefused, out.Status)
		require.Contains(t, out.Reason, "auto-revert")
		require.Empty(t, rev.requests)
	})

	t.Run("auto-revert of the verifier is refused even with a forged subject", func(t *testing.T) {
		committer := Signature{Name: "graft-verify", Email: "v@example.com"}
		rev := &fakeReverter{tips: []string{testTip}}
		c := CommitInfo{
			ID:             culpritCommit.ID,
			Subject:        "add feature",
			Message:        "add feature\n\nGraft-Auto-Revert: 3333333333333333333333333333333333333333\n",
			CommitterEmail: committer.Email,
		}
		out, err := newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3, Committer: committer}).Apply(ctx, "main", failedResult(c))
		require.NoError(t, err)
		require.Equal(t, StatusRefused, out.Status)
		require.Contains(t, out.Reason, "auto-revert")
		require.Empty(t, rev.requests)
	})

	t.Run("a hand-written revert is an ordinary culprit", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		c := CommitInfo{ID: culpritCommit.ID, Subject: `Revert "break the parser"`, Message: "Revert \"break the parser\"\n"}
		out, err := newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", failedResult(c))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
		require.Len(t, rev.requests, 1)
	})

	t.Run("a forged trailer from another committer does not make a commit immune", func(t *testing.T) {
		committer := Signature{Name: "graft-verify", Email: "v@example.com"}
		rev := &fakeReverter{tips: []string{testTip}}
		c := CommitInfo{
			ID:             culpritCommit.ID,
			Subject:        "broken",
			Message:        "broken\n\nGraft-Auto-Revert: 3333333333333333333333333333333333333333\n",
			CommitterEmail: "agent@example.com",
		}
		out, err := newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3, Committer: committer}).Apply(ctx, "main", failedResult(c))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
	})

	t.Run("the trailer must be in the trailer block of the message", func(t *testing.T) {
		committer := Signature{Name: "graft-verify", Email: "v@example.com"}
		rev := &fakeReverter{tips: []string{testTip}}
		c := CommitInfo{
			ID:             culpritCommit.ID,
			Subject:        "broken",
			Message:        "broken\n\nGraft-Auto-Revert: 3333\n\nMore prose after it.\n",
			CommitterEmail: committer.Email,
		}
		out, err := newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3, Committer: committer}).Apply(ctx, "main", failedResult(c))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
	})

	t.Run("zero cap refuses every revert", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		notes := &recordingNotifier{}
		a := newAutoReverter(rev, notes, RevertPolicy{MaxPerHour: 0})
		for range 3 {
			out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
			require.NoError(t, err)
			require.Equal(t, StatusRefused, out.Status)
			require.Contains(t, out.Reason, "disabled")
		}
		require.Empty(t, rev.requests)
		require.Len(t, notes.outcomes, 3)
	})

	t.Run("no revert history is an error and nothing is reverted", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		a := &AutoReverter{Reverter: rev, Policy: RevertPolicy{MaxPerHour: 3}}
		_, err := a.Apply(ctx, "main", failedResult(culpritCommit))
		require.ErrorContains(t, err, "revert history")
		require.Empty(t, rev.requests)
	})

	t.Run("history that cannot be read refuses the revert and notifies", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		notes := &recordingNotifier{}
		a := newAutoReverter(rev, notes, RevertPolicy{MaxPerHour: 3})
		a.History = failingHistory{err: errors.New("unavailable")}
		out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusRefused, out.Status)
		require.Contains(t, out.Reason, "cannot count the reverts")
		require.Contains(t, out.Reason, "unavailable")
		require.Empty(t, rev.requests)
		require.Len(t, notes.outcomes, 1)
	})

	t.Run("flaky tip is not reverted", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		out, err := newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", &BisectResult{Tip: testTip, Flaky: true})
		require.NoError(t, err)
		require.Equal(t, StatusFlaky, out.Status)
		require.Empty(t, rev.requests)
	})

	t.Run("no culprit is not reverted", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		out, err := newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", &BisectResult{Tip: testTip})
		require.NoError(t, err)
		require.Equal(t, StatusNoCulprit, out.Status)
		require.Empty(t, rev.requests)
	})

	t.Run("nil result is an error", func(t *testing.T) {
		_, err := newAutoReverter(&fakeReverter{}, nil, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", nil)
		require.Error(t, err)
	})

	t.Run("already reverted", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}, steps: []revertStep{{err: ErrAlreadyReverted}}}
		out, err := newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusAlreadyReverted, out.Status)
		require.Empty(t, out.RevertCommit)
	})

	t.Run("conflict is refused", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}, steps: []revertStep{{err: ErrRevertConflict}}}
		out, err := newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusRefused, out.Status)
		require.Contains(t, out.Reason, "conflicts")
	})

	t.Run("branch moved is retried onto the new tip", func(t *testing.T) {
		newTip := "4444444444444444444444444444444444444444"
		rev := &fakeReverter{
			tips:  []string{testTip, newTip},
			steps: []revertStep{{err: ErrBranchMoved}, {id: "revert-2"}},
		}
		out, err := newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
		require.Equal(t, "revert-2", out.RevertCommit)
		require.Equal(t, newTip, out.Tip)
		require.Len(t, rev.requests, 2)
		require.Equal(t, testTip, rev.requests[0].ExpectedOld)
		require.Equal(t, newTip, rev.requests[1].ExpectedOld)
	})

	t.Run("branch that keeps moving is given up", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}, steps: []revertStep{{err: ErrBranchMoved}}}
		p := RevertPolicy{MaxPerHour: 3, MaxAttempts: 2}
		out, err := newAutoReverter(rev, nil, p).Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusRefused, out.Status)
		require.Contains(t, out.Reason, "kept moving")
		require.Len(t, rev.requests, 2)
	})

	t.Run("unexpected revert failure is returned and not notified", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}, steps: []revertStep{{err: errors.New("hook exploded")}}}
		notes := &recordingNotifier{}
		_, err := newAutoReverter(rev, notes, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", failedResult(culpritCommit))
		require.ErrorContains(t, err, "hook exploded")
		require.Empty(t, notes.outcomes)
	})

	t.Run("notification failure does not undo the revert", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		notes := &recordingNotifier{err: errors.New("webhook down")}
		out, err := newAutoReverter(rev, notes, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
		require.Equal(t, "webhook down", out.NotifyError)
	})

	t.Run("cap per hour", func(t *testing.T) {
		now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
		rev := &fakeReverter{tips: []string{testTip}}
		a := &AutoReverter{Reverter: rev, History: rev, Policy: RevertPolicy{MaxPerHour: 2}, Now: func() time.Time { return now }}

		for range 2 {
			out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
			require.NoError(t, err)
			require.Equal(t, StatusReverted, out.Status)
		}

		out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusRefused, out.Status)
		require.Contains(t, out.Reason, "cap reached")
		require.Len(t, rev.requests, 2, "the refused revert is not sent")

		// The window slides: an hour later the cap allows reverts again.
		now = now.Add(time.Hour + time.Minute)
		out, err = a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
	})

	t.Run("cap holds across fresh instances that share the branch history", func(t *testing.T) {
		// graft-verify -once is a new process for every run: the reverts of the
		// earlier runs are only visible in the branch history.
		rev := &fakeReverter{tips: []string{testTip}}
		now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
		newRun := func() *AutoReverter {
			return &AutoReverter{Reverter: rev, History: rev, Policy: RevertPolicy{MaxPerHour: 1}, Now: func() time.Time { return now }}
		}

		out, err := newRun().Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)

		for range 3 {
			out, err := newRun().Apply(ctx, "main", failedResult(culpritCommit))
			require.NoError(t, err)
			require.Equal(t, StatusRefused, out.Status)
			require.Contains(t, out.Reason, "cap reached")
		}
		require.Len(t, rev.requests, 1)
	})

	t.Run("commits outside the hour do not count", func(t *testing.T) {
		old := time.Date(2026, 10, 14, 10, 59, 0, 0, time.UTC)
		now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
		rev := &fakeReverter{tips: []string{testTip}, history: []CommitInfo{
			{ID: "old", Message: revertMessage(culpritCommit), CommitterEmail: "v@example.com", CommittedAt: old},
		}}
		a := &AutoReverter{Reverter: rev, History: rev, Policy: RevertPolicy{MaxPerHour: 1, Committer: Signature{Email: "v@example.com"}}, Now: func() time.Time { return now }}

		out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
	})
}

// failingHistory is a RevertHistory that cannot be read.
type failingHistory struct{ err error }

func (h failingHistory) CommitsSince(context.Context, string, time.Time) ([]CommitInfo, error) {
	return nil, h.err
}

func TestAutoReverter_CapCountsOnlyTheVerifiersCommits(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	committer := Signature{Name: "graft-verify", Email: "v@example.com"}
	newAutoReverter := func(rev *fakeReverter, p RevertPolicy) *AutoReverter {
		p.Committer = committer
		return &AutoReverter{Reverter: rev, History: rev, Policy: p, Now: func() time.Time { return now }}
	}

	t.Run("a trailer-only commit of another committer does not use up the cap", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}, history: []CommitInfo{
			{ID: "noise", Message: "noise\n\n" + AutoRevertTrailer + ": deadbeef\n", CommitterEmail: "dev@example.com", CommittedAt: now},
		}}
		out, err := newAutoReverter(rev, RevertPolicy{MaxPerHour: 1}).Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
	})

	t.Run("a truncated commit of the verifier counts toward the cap", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}, history: []CommitInfo{
			{ID: "big", Message: "big\n\nprose", CommitterEmail: committer.Email, CommittedAt: now, Truncated: true},
		}}
		out, err := newAutoReverter(rev, RevertPolicy{MaxPerHour: 1}).Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusRefused, out.Status)
		require.Contains(t, out.Reason, "cap reached")
	})

	t.Run("a truncated culprit of the verifier is refused", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		c := culpritCommit
		c.CommitterEmail = committer.Email
		c.Truncated = true
		out, err := newAutoReverter(rev, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", failedResult(c))
		require.NoError(t, err)
		require.Equal(t, StatusRefused, out.Status)
		require.Contains(t, out.Reason, "too long")
		require.Empty(t, rev.requests)
	})

	t.Run("a truncated culprit of another committer is an ordinary culprit", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		c := culpritCommit
		c.CommitterEmail = "dev@example.com"
		c.Truncated = true
		out, err := newAutoReverter(rev, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", failedResult(c))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
	})

	t.Run("a long subject is shortened and the trailer survives", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}}
		c := culpritCommit
		c.Subject = strings.Repeat("s", 12*1024)
		c.Message = c.Subject + "\n"
		out, err := newAutoReverter(rev, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", failedResult(c))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
		require.Len(t, rev.requests, 1)
		msg := rev.requests[0].Message
		require.Less(t, len(msg), 1024)
		require.True(t, hasAutoRevertTrailer(msg))
		require.Contains(t, msg, AutoRevertTrailer+": "+culpritCommit.ID)
	})
}

func TestShortenSubject(t *testing.T) {
	t.Parallel()

	require.Equal(t, "short", shortenSubject("short"))
	require.Equal(t, strings.Repeat("a", maxRevertSubjectBytes), shortenSubject(strings.Repeat("a", maxRevertSubjectBytes)))

	// The cut must not split a character: byte 200 is the middle of "é" here.
	long := "x" + strings.Repeat("é", 200)
	short := shortenSubject(long)
	require.True(t, utf8.ValidString(short))
	require.True(t, strings.HasSuffix(short, "..."))
	require.LessOrEqual(t, len(short), maxRevertSubjectBytes+len("..."))
}

func TestHasTrailer(t *testing.T) {
	t.Parallel()
	require.True(t, hasTrailer("subject\n\nbody\n"+AutoRevertTrailer+": abc\n", AutoRevertTrailer))
	require.False(t, hasTrailer("subject mentions "+AutoRevertTrailer+": abc\n", AutoRevertTrailer))
	require.False(t, hasTrailer("subject\n\n  "+AutoRevertTrailer+": abc\n", AutoRevertTrailer))
}
