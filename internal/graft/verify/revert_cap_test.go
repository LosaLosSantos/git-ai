package verify

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// syncReverter is a Reverter that is safe for concurrent use. Each successful
// revert gets a new ID. A non-nil err is returned by the next call to Revert,
// once.
type syncReverter struct {
	mu       sync.Mutex
	tip      string
	reverts  int
	nextErrs []error
	history  []CommitInfo
}

func (r *syncReverter) Tip(context.Context, string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tip, nil
}

func (r *syncReverter) Revert(_ context.Context, req RevertRequest) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.nextErrs) > 0 {
		err := r.nextErrs[0]
		r.nextErrs = r.nextErrs[1:]
		if err != nil {
			return "", err
		}
	}
	r.reverts++
	id := fmt.Sprintf("revert-%d", r.reverts)
	r.history = append(r.history, revertCommit(id, req))
	return id, nil
}

// CommitsSince implements RevertHistory.
func (r *syncReverter) CommitsSince(_ context.Context, _ string, since time.Time) ([]CommitInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return commitsSince(r.history, since), nil
}

func (r *syncReverter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reverts
}

func TestAutoReverter_CapHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	const cap, callers = 3, 40
	rev := &syncReverter{tip: testTip}
	a := &AutoReverter{Reverter: rev, History: rev, Policy: RevertPolicy{MaxPerHour: cap}, Now: func() time.Time { return now }}

	start := make(chan struct{})
	outcomes := make(chan Status, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
			if err != nil {
				errs <- err
				return
			}
			outcomes <- out.Status
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)
	close(errs)

	require.Empty(t, errs)
	counts := map[Status]int{}
	for s := range outcomes {
		counts[s]++
	}
	require.Equal(t, cap, rev.count(), "exactly the cap is reverted")
	require.Equal(t, cap, counts[StatusReverted])
	require.Equal(t, callers-cap, counts[StatusRefused])
}

func TestAutoReverter_CapSlotReleasedWhenRevertNotPerformed(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	t.Run("refused conflict gives its slot back", func(t *testing.T) {
		rev := &syncReverter{tip: testTip, nextErrs: []error{ErrRevertConflict}}
		a := &AutoReverter{Reverter: rev, History: rev, Policy: RevertPolicy{MaxPerHour: 1}, Now: func() time.Time { return now }}

		out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusRefused, out.Status)

		out, err = a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
	})

	t.Run("already reverted gives its slot back", func(t *testing.T) {
		rev := &syncReverter{tip: testTip, nextErrs: []error{ErrAlreadyReverted}}
		a := &AutoReverter{Reverter: rev, History: rev, Policy: RevertPolicy{MaxPerHour: 1}, Now: func() time.Time { return now }}

		out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusAlreadyReverted, out.Status)

		out, err = a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
	})

	t.Run("unexpected failure gives its slot back", func(t *testing.T) {
		rev := &syncReverter{tip: testTip, nextErrs: []error{errors.New("boom")}}
		a := &AutoReverter{Reverter: rev, History: rev, Policy: RevertPolicy{MaxPerHour: 1}, Now: func() time.Time { return now }}

		_, err := a.Apply(ctx, "main", failedResult(culpritCommit))
		require.ErrorContains(t, err, "boom")

		out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)
	})

	t.Run("a slot is kept after a revert", func(t *testing.T) {
		rev := &syncReverter{tip: testTip}
		a := &AutoReverter{Reverter: rev, History: rev, Policy: RevertPolicy{MaxPerHour: 1}, Now: func() time.Time { return now }}

		out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusReverted, out.Status)

		out, err = a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		require.Equal(t, StatusRefused, out.Status)
		require.Equal(t, 1, rev.count())
	})
}

func TestAutoReverter_DryRunRespectsCap(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	rev := &syncReverter{tip: testTip}
	a := &AutoReverter{Reverter: rev, History: rev, Policy: RevertPolicy{MaxPerHour: 1}, Now: func() time.Time { return now }}
	out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
	require.NoError(t, err)
	require.Equal(t, StatusReverted, out.Status)

	// The cap is shared between dry runs and real reverts, so that a dry run
	// reports what the real run would do.
	a.Policy.DryRun = true
	out, err = a.Apply(ctx, "main", failedResult(culpritCommit))
	require.NoError(t, err)
	require.Equal(t, StatusRefused, out.Status)
	require.Contains(t, out.Reason, "cap reached")
}

func TestAutoReverter_EmptyValuesAreErrors(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	t.Run("empty tip is an error and nothing is reverted", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{""}}
		_, err := newAutoReverter(rev, nil, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", failedResult(culpritCommit))
		require.ErrorContains(t, err, "empty object ID")
		require.Empty(t, rev.requests)
	})

	t.Run("empty revert ID is an error", func(t *testing.T) {
		rev := &fakeReverter{tips: []string{testTip}, steps: []revertStep{{id: ""}}}
		notes := &recordingNotifier{}
		_, err := newAutoReverter(rev, notes, RevertPolicy{MaxPerHour: 3}).Apply(ctx, "main", failedResult(culpritCommit))
		require.ErrorContains(t, err, "no commit ID")
		require.Empty(t, notes.outcomes)
	})
}
