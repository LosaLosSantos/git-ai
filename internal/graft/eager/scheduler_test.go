package eager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// countingWriter counts the writes per repository and tracks how many writes
// run at the same time.
type countingWriter struct {
	mu       sync.Mutex
	counts   map[string]int
	running  map[string]int
	maxAll   int
	inFlight int
}

func newCountingWriter() *countingWriter {
	return &countingWriter{counts: make(map[string]int), running: make(map[string]int)}
}

func (w *countingWriter) write(_ context.Context, repo storage.Repository) error {
	key := repo.GetRelativePath()

	w.mu.Lock()
	w.counts[key]++
	w.running[key]++
	w.inFlight++
	if w.inFlight > w.maxAll {
		w.maxAll = w.inFlight
	}
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.running[key]--
		w.inFlight--
		w.mu.Unlock()
	}()

	time.Sleep(time.Millisecond)
	return nil
}

func (w *countingWriter) count(relPath string) int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.counts[relPath]
}

func repoNamed(relPath string) *gitalypb.Repository {
	return &gitalypb.Repository{StorageName: "default", RelativePath: relPath}
}

func TestScheduler_BurstCollapsesIntoOneWrite(t *testing.T) {
	ctx := testhelper.Context(t)
	writer := newCountingWriter()

	s := NewScheduler(ctx, testhelper.NewLogger(t), writer.write, 50*time.Millisecond, time.Second)
	defer s.Close()

	for i := 0; i < 20; i++ {
		s.Schedule(repoNamed("a.git"))
	}

	require.Eventually(t, func() bool { return writer.count("a.git") == 1 }, 2*time.Second, time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, writer.count("a.git"), "the burst is written once")
}

func TestScheduler_UpdateDuringWriteRunsWriteAgain(t *testing.T) {
	ctx := testhelper.Context(t)

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32

	write := func(_ context.Context, _ storage.Repository) error {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return nil
	}

	s := NewScheduler(ctx, testhelper.NewLogger(t), write, time.Millisecond, time.Second)
	defer s.Close()

	s.Schedule(repoNamed("a.git"))
	<-started

	s.Schedule(repoNamed("a.git"))
	close(release)

	require.Eventually(t, func() bool { return calls.Load() == 2 }, 2*time.Second, time.Millisecond,
		"a schedule during a write leaves the write to run once more")
}

func TestScheduler_MaxWaitBoundsPostponement(t *testing.T) {
	ctx := testhelper.Context(t)
	writer := newCountingWriter()

	s := NewScheduler(ctx, testhelper.NewLogger(t), writer.write, 20*time.Millisecond, 60*time.Millisecond)
	defer s.Close()

	// A steady stream of updates never lets the debounce period pass on its own.
	stop := time.After(400 * time.Millisecond)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-stop:
			break loop
		case <-ticker.C:
			s.Schedule(repoNamed("a.git"))
		}
	}

	require.Eventually(t, func() bool { return writer.count("a.git") >= 3 }, 2*time.Second, time.Millisecond,
		"the max wait forces writes during a stream of updates")
}

func TestScheduler_RepositoriesAreIndependent(t *testing.T) {
	ctx := testhelper.Context(t)
	writer := newCountingWriter()

	s := NewScheduler(ctx, testhelper.NewLogger(t), writer.write, time.Millisecond, time.Second)
	defer s.Close()

	s.Schedule(repoNamed("a.git"))
	s.Schedule(repoNamed("b.git"))

	require.Eventually(t, func() bool {
		return writer.count("a.git") == 1 && writer.count("b.git") == 1
	}, 2*time.Second, time.Millisecond)
}

func TestScheduler_ConcurrencyIsBounded(t *testing.T) {
	ctx := testhelper.Context(t)
	writer := newCountingWriter()

	s := NewScheduler(ctx, testhelper.NewLogger(t), writer.write, time.Millisecond, time.Second)
	defer s.Close()

	for i := 0; i < 8; i++ {
		s.Schedule(repoNamed(fmt.Sprintf("repo-%d.git", i)))
	}

	require.Eventually(t, func() bool {
		writer.mu.Lock()
		defer writer.mu.Unlock()

		return len(writer.counts) == 8 && writer.inFlight == 0
	}, 5*time.Second, time.Millisecond)

	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.LessOrEqual(t, writer.maxAll, maxConcurrentWrites)
}

func TestScheduler_FailingAndPanickingWritesDoNotStopIt(t *testing.T) {
	ctx := testhelper.Context(t)

	var calls atomic.Int32
	write := func(_ context.Context, _ storage.Repository) error {
		switch calls.Add(1) {
		case 1:
			return errors.New("write failed")
		case 2:
			panic("write panicked")
		default:
			return nil
		}
	}

	s := NewScheduler(ctx, testhelper.NewLogger(t), write, time.Millisecond, time.Second)
	defer s.Close()

	for i := 0; i < 3; i++ {
		s.Schedule(repoNamed("a.git"))
		require.Eventually(t, func() bool { return calls.Load() == int32(i+1) }, 2*time.Second, time.Millisecond)
		time.Sleep(5 * time.Millisecond)
	}
}

func TestScheduler_BusyRepositoryIsRetried(t *testing.T) {
	ctx := testhelper.Context(t)

	var calls atomic.Int32
	write := func(_ context.Context, _ storage.Repository) error {
		if calls.Add(1) <= 3 {
			return fmt.Errorf("%w: housekeeping is running", ErrRepositoryBusy)
		}
		return nil
	}

	s := NewScheduler(ctx, testhelper.NewLogger(t), write, time.Millisecond, time.Second)
	defer s.Close()

	s.Schedule(repoNamed("a.git"))

	require.Eventually(t, func() bool { return calls.Load() == 4 }, 2*time.Second, time.Millisecond,
		"a write that found the repository busy is retried without a new update")

	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.pending) == 0
	}, 2*time.Second, time.Millisecond, "the successful write ends the retries")

	time.Sleep(20 * time.Millisecond)
	require.Equal(t, int32(4), calls.Load())
}

func TestScheduler_BusyRetriesAreBounded(t *testing.T) {
	ctx := testhelper.Context(t)

	var calls atomic.Int32
	write := func(_ context.Context, _ storage.Repository) error {
		calls.Add(1)
		return ErrRepositoryBusy
	}

	s := NewScheduler(ctx, testhelper.NewLogger(t), write, time.Millisecond, time.Second)
	defer s.Close()

	s.Schedule(repoNamed("a.git"))

	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.pending) == 0
	}, 5*time.Second, time.Millisecond, "the write is dropped after the retries")
	require.Equal(t, int32(maxBusyRetries+1), calls.Load())
}

func TestScheduler_FailedWriteIsNotRetried(t *testing.T) {
	ctx := testhelper.Context(t)

	var calls atomic.Int32
	write := func(_ context.Context, _ storage.Repository) error {
		calls.Add(1)
		return errors.New("write failed")
	}

	s := NewScheduler(ctx, testhelper.NewLogger(t), write, time.Millisecond, time.Second)
	defer s.Close()

	s.Schedule(repoNamed("a.git"))

	require.Eventually(t, func() bool { return calls.Load() == 1 }, 2*time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, int32(1), calls.Load(), "only a busy repository is retried")
}

func TestScheduler_CloseDropsPendingAndWaitsForRunning(t *testing.T) {
	ctx := testhelper.Context(t)

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32

	write := func(_ context.Context, repo storage.Repository) error {
		calls.Add(1)
		if repo.GetRelativePath() == "running.git" {
			close(started)
			<-release
		}
		return nil
	}

	s := NewScheduler(ctx, testhelper.NewLogger(t), write, time.Millisecond, time.Second)

	s.Schedule(repoNamed("running.git"))
	<-started

	// A write that waits for its timer is dropped by Close.
	s2 := NewScheduler(ctx, testhelper.NewLogger(t), write, time.Hour, time.Hour)
	s2.Schedule(repoNamed("pending.git"))

	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()

	select {
	case <-closed:
		require.FailNow(t, "Close returned while a write was running")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	<-closed

	s.Close()
	s.Schedule(repoNamed("after.git"))
	s2.Close()

	require.Equal(t, int32(1), calls.Load(), "only the running write ran")
}
