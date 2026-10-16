package derived

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// fakeRepository is a storage.Repository for tests that never looks at the
// repository beyond passing it to the open function.
type fakeRepository struct {
	storage.Repository
	relativePath string
}

func (r fakeRepository) GetRelativePath() string { return r.relativePath }

// poolTestOpen returns an open function whose environment stores into store.
func poolTestOpen(store *Store) OpenWarmEnvFunc {
	return func(context.Context, storage.Repository) (WarmEnv, func(), error) {
		return WarmEnv{Store: store}, nil, nil
	}
}

// metricValues collects the derived warm metrics of c and returns their values by
// metric name.
func metricValues(t *testing.T, c prometheus.Collector) map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric)
	go func() {
		c.Collect(ch)
		close(ch)
	}()

	names := []string{metricWarmQueueDepth, metricWarmDropped, metricWarmComputed, metricWarmErrors}
	values := make(map[string]float64)
	for metric := range ch {
		var out dto.Metric
		require.NoError(t, metric.Write(&out))

		for _, name := range names {
			if !strings.Contains(metric.Desc().String(), `fqName: "`+name+`"`) {
				continue
			}
			if gauge := out.GetGauge(); gauge != nil {
				values[name] = gauge.GetValue()
			} else {
				values[name] = out.GetCounter().GetValue()
			}
		}
	}

	for _, name := range names {
		require.Contains(t, values, name, "metric %s is collected", name)
	}
	return values
}

// runPool runs pool in the background until the test ends.
func runPool(t *testing.T, pool *Pool) {
	t.Helper()

	ctx, cancel := context.WithCancel(testhelper.Context(t))
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(ctx)
	}()

	t.Cleanup(func() {
		pool.Close()
		cancel()
		<-done
	})
}

func TestNewPool_PanicsOnInvalidSizes(t *testing.T) {
	logger := testhelper.NewLogger(t)
	open := poolTestOpen(nil)

	require.Panics(t, func() { NewPool(logger, 0, 1, open) })
	require.Panics(t, func() { NewPool(logger, 1, 0, open) })
}

func TestPool_EnqueueDropsAndCountsWhenQueueIsFull(t *testing.T) {
	pool := NewPool(testhelper.NewLogger(t), 1, 2, poolTestOpen(NewStore(t.TempDir())))
	repo := fakeRepository{relativePath: "repo"}

	// Run is not started, so nothing drains the queue. The queue holds two batches.
	pool.Enqueue(repo, []git.ObjectID{fakeOID(0), fakeOID(1)})
	pool.Enqueue(repo, []git.ObjectID{fakeOID(2), fakeOID(3)})
	pool.Enqueue(repo, []git.ObjectID{fakeOID(4), fakeOID(5), fakeOID(6)})
	pool.Enqueue(repo, nil)

	values := metricValues(t, pool)
	require.Equal(t, float64(2), values[metricWarmQueueDepth])
	require.Equal(t, float64(3), values[metricWarmDropped], "the commits of the refused batch are counted")

	// Close drops the batches that were still queued.
	pool.Close()
	values = metricValues(t, pool)
	require.Equal(t, float64(0), values[metricWarmQueueDepth])
	require.Equal(t, float64(7), values[metricWarmDropped])

	pool.Enqueue(repo, []git.ObjectID{fakeOID(7)})
	require.Equal(t, float64(8), metricValues(t, pool)[metricWarmDropped], "a closed pool accepts nothing")
}

func TestPool_RunsEveryRegisteredWarmerAndSurvivesFailures(t *testing.T) {
	registerToyWarmer(t, "toy-ok", toyWarmer)
	registerToyWarmer(t, "toy-fail", func(context.Context, WarmEnv, []git.ObjectID) error {
		return errors.New("warmer failed")
	})
	registerToyWarmer(t, "toy-panic", func(context.Context, WarmEnv, []git.ObjectID) error {
		panic("warmer panicked")
	})

	store := NewStore(t.TempDir())
	pool := NewPool(testhelper.NewLogger(t), 2, 10, poolTestOpen(store))
	runPool(t, pool)

	repo := fakeRepository{relativePath: "repo"}
	pool.Enqueue(repo, []git.ObjectID{fakeOID(0), fakeOID(1), fakeOID(2)})
	pool.Enqueue(repo, []git.ObjectID{fakeOID(3), fakeOID(4)})

	require.Eventually(t, func() bool {
		values := metricValues(t, pool)
		// Only toy-ok succeeds, once per commit. Both failing warmers fail on both batches.
		return values[metricWarmComputed] == 5 && values[metricWarmErrors] == 4
	}, 10*time.Second, time.Millisecond)

	for i := 0; i < 5; i++ {
		value, found, err := store.Kind("toy", 1).Get(fakeOID(i))
		require.NoError(t, err)
		require.True(t, found, "commit %d was warmed", i)
		require.Equal(t, "value-"+fakeOID(i).String(), string(value))
	}
}

func TestPool_CountsOpenFailures(t *testing.T) {
	registerToyWarmer(t, "toy-unused", toyWarmer)

	open := func(context.Context, storage.Repository) (WarmEnv, func(), error) {
		return WarmEnv{}, nil, errors.New("no such repository")
	}
	pool := NewPool(testhelper.NewLogger(t), 1, 4, open)
	runPool(t, pool)

	pool.Enqueue(fakeRepository{relativePath: "missing"}, []git.ObjectID{fakeOID(0)})

	require.Eventually(t, func() bool {
		return metricValues(t, pool)[metricWarmErrors] == 1
	}, 10*time.Second, time.Millisecond)
	require.Equal(t, float64(0), metricValues(t, pool)[metricWarmComputed])
}

func TestPool_BoundsConcurrency(t *testing.T) {
	const workers = 2
	const batches = 20

	var active, maxActive atomic.Int64
	registerToyWarmer(t, "toy-slow", func(_ context.Context, _ WarmEnv, commits []git.ObjectID) error {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		return nil
	})

	pool := NewPool(testhelper.NewLogger(t), workers, batches, poolTestOpen(NewStore(t.TempDir())))
	repo := fakeRepository{relativePath: "repo"}
	for i := 0; i < batches; i++ {
		pool.Enqueue(repo, []git.ObjectID{fakeOID(i)})
	}
	runPool(t, pool)

	require.Eventually(t, func() bool {
		return metricValues(t, pool)[metricWarmComputed] == batches
	}, 10*time.Second, time.Millisecond)
	require.LessOrEqual(t, maxActive.Load(), int64(workers))
}

func TestPool_RunReturnsOnContextAndOnClose(t *testing.T) {
	pool := NewPool(testhelper.NewLogger(t), 2, 1, poolTestOpen(nil))

	ctx, cancel := context.WithCancel(testhelper.Context(t))
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(ctx)
	}()

	cancel()
	requireClosed(t, done)

	// Run on a closed pool returns at once, and Close is idempotent.
	pool.Close()
	pool.Close()
	pool.Run(testhelper.Context(t))
}

func TestPool_CloseStopsRunningWorkers(t *testing.T) {
	pool := NewPool(testhelper.NewLogger(t), 3, 1, poolTestOpen(nil))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pool.Run(testhelper.Context(t))
	}()

	pool.Close()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	requireClosed(t, done)
}

// requireClosed fails the test unless done is closed within a few seconds.
func requireClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the pool to stop")
	}
}
