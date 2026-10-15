package landlane

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

func TestCompleteEarlyDeliversBeforeTheExecutorReturns(t *testing.T) {
	ctx := testhelper.Context(t)
	gate := make(chan struct{})
	completed := make(chan bool, 1)

	exec := func(execCtx context.Context, _ Key, reqs []string) ([]Result[string], error) {
		completed <- CompleteEarly(execCtx, 0, Result[string]{Value: "early-" + reqs[0]})
		<-gate

		out := make([]Result[string], len(reqs))
		for i, req := range reqs {
			out[i].Value = "late-" + req
		}
		return out, nil
	}
	r := newRegistry(t, Config{Enabled: true, Window: time.Hour, MaxSize: 3}, exec, nil)

	first := submitAsync(ctx, r, testKey, "r0")
	require.Eventually(t, func() bool { return queueLen(r) == 1 }, 10*time.Second, time.Millisecond)
	second := submitAsync(ctx, r, testKey, "r1")
	require.Eventually(t, func() bool { return queueLen(r) == 2 }, 10*time.Second, time.Millisecond)
	third := submitAsync(ctx, r, testKey, "r2")

	require.True(t, <-completed, "the request of the batch must be completed early")

	// The executor is still blocked on the gate: the submitter of the early
	// request must already have returned.
	res := requireResult(t, first)
	require.NoError(t, res.err)
	require.Equal(t, "early-r0", res.resp.Value)
	require.Equal(t, 3, res.resp.BatchSize)
	require.Equal(t, 0, res.resp.BatchPosition)

	close(gate)

	// The executor's result for the early request is ignored, the others are not.
	res = requireResult(t, second)
	require.NoError(t, res.err)
	require.Equal(t, "late-r1", res.resp.Value)
	require.Equal(t, 1, res.resp.BatchPosition)

	res = requireResult(t, third)
	require.NoError(t, res.err)
	require.Equal(t, "late-r2", res.resp.Value)
	require.Equal(t, 2, res.resp.BatchPosition)
}

func TestCompleteEarlyIgnoresLaterResultsAndRejectsRepeatsAndBadIndexes(t *testing.T) {
	ctx := testhelper.Context(t)
	checks := make(chan []bool, 1)

	exec := func(execCtx context.Context, _ Key, reqs []string) ([]Result[string], error) {
		checks <- []bool{
			CompleteEarly(execCtx, 0, Result[string]{Value: "early"}),
			CompleteEarly(execCtx, 0, Result[string]{Value: "again"}),
			CompleteEarly(execCtx, 1, Result[string]{Value: "other"}),
			CompleteEarly(execCtx, len(reqs), Result[string]{Value: "beyond"}),
			CompleteEarly(execCtx, -1, Result[string]{Value: "negative"}),
		}

		return []Result[string]{{Value: "late-0"}, {Value: "late-1"}}, nil
	}
	r := newRegistry(t, Config{Enabled: true, Window: 2 * time.Second, MaxSize: 2}, exec, nil)

	first := submitAsync(ctx, r, testKey, "r0")
	require.Eventually(t, func() bool { return queueLen(r) == 1 }, 10*time.Second, time.Millisecond)
	second := submitAsync(ctx, r, testKey, "r1")

	require.Equal(t, []bool{true, false, true, false, false}, <-checks)

	res := requireResult(t, first)
	require.NoError(t, res.err)
	require.Equal(t, "early", res.resp.Value, "the first result of the request is the early one")

	res = requireResult(t, second)
	require.NoError(t, res.err)
	require.Equal(t, "other", res.resp.Value, "the first early result of an index wins")
}

func TestCompleteEarlyDeliversErrorsAndSurvivesBatchErrors(t *testing.T) {
	ctx := testhelper.Context(t)
	boom := errors.New("boom")

	exec := func(execCtx context.Context, _ Key, _ []string) ([]Result[string], error) {
		okValue := CompleteEarly(execCtx, 0, Result[string]{Value: "early"})
		okErr := CompleteEarly(execCtx, 1, Result[string]{Err: boom})
		if !okValue || !okErr {
			return nil, errors.New("early completion failed")
		}

		return nil, boom
	}
	r := newRegistry(t, Config{Enabled: true, Window: time.Hour, MaxSize: 2}, exec, nil)

	first := submitAsync(ctx, r, testKey, "r0")
	require.Eventually(t, func() bool { return queueLen(r) == 1 }, 10*time.Second, time.Millisecond)
	second := submitAsync(ctx, r, testKey, "r1")

	// The batch error fails the requests the executor did not complete, but not
	// the ones completed early.
	res := requireResult(t, first)
	require.NoError(t, res.err)
	require.Equal(t, "early", res.resp.Value)

	res = requireResult(t, second)
	require.ErrorIs(t, res.err, boom)
	require.Equal(t, 2, res.resp.BatchSize)
	require.Equal(t, 1, res.resp.BatchPosition)
}

func TestCompleteEarlyRejectsForeignContexts(t *testing.T) {
	ctx := testhelper.Context(t)

	require.False(t, CompleteEarly(ctx, 0, Result[string]{Value: "foreign"}), "a context without a batch")

	t.Run("another result type", func(t *testing.T) {
		completed := make(chan bool, 1)
		exec := func(execCtx context.Context, _ Key, _ []string) ([]Result[string], error) {
			completed <- CompleteEarly[int](execCtx, 0, Result[int]{Value: 1})
			return []Result[string]{{Value: "late"}}, nil
		}
		r := newRegistry(t, unbatched(50), exec, nil)

		resp, err := r.Submit(ctx, testKey, "r0")
		require.NoError(t, err)
		require.Equal(t, "late", resp.Value)
		require.False(t, <-completed)
	})

	t.Run("disabled registry runs requests alone", func(t *testing.T) {
		completed := make(chan bool, 1)
		exec := func(execCtx context.Context, _ Key, _ []string) ([]Result[string], error) {
			completed <- CompleteEarly(execCtx, 0, Result[string]{Value: "early"})
			return []Result[string]{{Value: "late"}}, nil
		}
		r := newRegistry(t, Config{Enabled: false, MaxSize: 50}, exec, nil)

		resp, err := r.Submit(ctx, testKey, "r0")
		require.NoError(t, err)
		require.Equal(t, "late", resp.Value)
		require.False(t, <-completed)
	})

	t.Run("after the batch returned", func(t *testing.T) {
		var mu sync.Mutex
		var saved context.Context
		exec := func(execCtx context.Context, _ Key, _ []string) ([]Result[string], error) {
			mu.Lock()
			saved = execCtx
			mu.Unlock()

			return []Result[string]{{Value: "late"}}, nil
		}
		r := newRegistry(t, unbatched(50), exec, nil)

		resp, err := r.Submit(ctx, testKey, "r0")
		require.NoError(t, err)
		require.Equal(t, "late", resp.Value)

		mu.Lock()
		defer mu.Unlock()
		require.False(t, CompleteEarly(saved, 0, Result[string]{Value: "stale"}))
	})
}

func TestCompleteEarlyConcurrentCallsCompleteEachRequestOnce(t *testing.T) {
	ctx := testhelper.Context(t)
	const n = 50

	wins := make(chan int, 1)
	exec := func(execCtx context.Context, _ Key, reqs []string) ([]Result[string], error) {
		var won atomic.Int32

		var wg sync.WaitGroup
		for i := range reqs {
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()

					if CompleteEarly(execCtx, i, Result[string]{Value: "early-" + reqs[i]}) {
						won.Add(1)
					}
				}()
			}
		}
		wg.Wait()
		wins <- int(won.Load())

		out := make([]Result[string], len(reqs))
		for i, req := range reqs {
			out[i].Value = "late-" + req
		}
		return out, nil
	}
	r := newRegistry(t, Config{Enabled: true, Window: time.Hour, MaxSize: n}, exec, nil)

	chans := make([]<-chan result, n)
	for i := range n {
		chans[i] = submitAsync(ctx, r, testKey, fmt.Sprintf("r%d", i))
	}

	require.Equal(t, n, <-wins, "every request is completed early exactly once")

	for i, ch := range chans {
		res := requireResult(t, ch)
		require.NoError(t, res.err)
		require.Equal(t, n, res.resp.BatchSize)
		require.Equal(t, "early-"+fmt.Sprintf("r%d", i), res.resp.Value)
	}
}
