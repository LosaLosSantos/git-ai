package burdenmonitor

import (
	"context"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// mockServerStream is a minimal grpc.ServerStream for use in interceptor tests.
type mockServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (m *mockServerStream) Context() context.Context     { return m.ctx }
func (m *mockServerStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockServerStream) SendHeader(metadata.MD) error { return nil }
func (m *mockServerStream) SetTrailer(metadata.MD)       {}
func (m *mockServerStream) SendMsg(interface{}) error    { return nil }
func (m *mockServerStream) RecvMsg(interface{}) error    { return nil }

func TestBurdenMonitor_RegisterRPC(t *testing.T) {
	t.Parallel()

	bm := New(testhelper.SharedLogger(t))
	ctx := testhelper.Context(t)

	require.Empty(t, bm.Entries())

	newCtx, entry := bm.RegisterRPC(ctx, "/gitaly.RepositoryService/RepositoryExists")

	require.NotEmpty(t, entry.ID)
	require.Equal(t, "gitaly.RepositoryService", entry.ServiceName)
	require.Equal(t, "RepositoryExists", entry.MethodName)
	require.False(t, entry.StartTime.IsZero())
	require.NotNil(t, entry.Context)
	require.NotNil(t, entry.Cancel)

	require.Len(t, bm.Entries(), 1)

	got, ok := bm.GetRPC(entry.ID)
	require.True(t, ok)
	require.Equal(t, entry, got)

	// The returned context should be derived from the original.
	require.NoError(t, newCtx.Err())
}

func TestBurdenMonitor_DeregisterRPC(t *testing.T) {
	t.Parallel()

	bm := New(testhelper.SharedLogger(t))
	ctx := testhelper.Context(t)

	_, e1 := bm.RegisterRPC(ctx, "/foo.Service/A")
	_, e2 := bm.RegisterRPC(ctx, "/foo.Service/B")
	require.Len(t, bm.Entries(), 2)

	bm.DeregisterRPC(e1.ID)
	require.Len(t, bm.Entries(), 1)

	_, ok := bm.GetRPC(e1.ID)
	require.False(t, ok)

	_, ok = bm.GetRPC(e2.ID)
	require.True(t, ok)

	// Deregistering a non-existent ID is a no-op.
	bm.DeregisterRPC("does-not-exist")
	require.Len(t, bm.Entries(), 1)
}

func TestBurdenMonitor_EntriesSortedBy(t *testing.T) {
	t.Parallel()

	logger := testhelper.NewLogger(t)
	hook := testhelper.AddLoggerHook(logger)
	bm := New(logger)
	ctx := testhelper.Context(t)

	_, e1 := bm.RegisterRPC(ctx, "/foo.Service/A")
	_, e2 := bm.RegisterRPC(ctx, "/foo.Service/B")
	_, e3 := bm.RegisterRPC(ctx, "/foo.Service/C")

	// CPU times: e1=100ms (completed), e2=300ms (running), e3=200ms (completed)
	e1.RegisterCommand(101, "git upload-pack", time.Now())
	e1.MarkCommandCompleted(101, 60*time.Millisecond, 40*time.Millisecond)
	// e2's CPU time is split across more commands than the per-entry logging
	// cap so that logStats' truncation is exercised.
	for i, cpu := range []time.Duration{90, 60, 50, 40, 30, 20, 10} {
		pid := 102 + i
		e2.RegisterCommand(pid, "git pack-objects", time.Now())
		// These are inflight commands, so we need to populate the resource consumption here.
		e2.Commands[pid].UserTime = cpu * time.Millisecond / 2
		e2.Commands[pid].SystemTime = cpu * time.Millisecond / 2
		e2.Commands[pid].AnonRSS = int64(1000 * (i + 1))
	}
	e3.RegisterCommand(110, "git rev-list", time.Now())
	e3.MarkCommandCompleted(110, 150*time.Millisecond, 50*time.Millisecond)

	t.Run("SortByCPU", func(t *testing.T) {
		sorted := bm.entriesSortedBy(SortByCPU)
		require.Len(t, sorted, 3)
		assert.Equal(t, e2.ID, sorted[0].ID) // 300ms
		assert.Equal(t, e3.ID, sorted[1].ID) // 200ms
		assert.Equal(t, e1.ID, sorted[2].ID) // 100ms

		hook.Reset()
		bm.logStats()

		entries := hook.AllEntries()
		require.Len(t, entries, 1)
		logEntry := entries[0]
		require.Equal(t, "burden monitor stats", logEntry.Message)

		require.Equal(t, "foo.Service/B", logEntry.Data["top_cpu_1.rpc"])
		require.Equal(t, "foo.Service/C", logEntry.Data["top_cpu_2.rpc"])
		require.Equal(t, "foo.Service/A", logEntry.Data["top_cpu_3.rpc"])

		require.Equal(t, int64(300), logEntry.Data["top_cpu_1.total_cpu_ms"])
		require.Equal(t, int64(200), logEntry.Data["top_cpu_2.total_cpu_ms"])
		require.Equal(t, int64(100), logEntry.Data["top_cpu_3.total_cpu_ms"])

		require.Equal(t, 7, logEntry.Data["top_cpu_1.active_commands"])

		require.Equal(t, "git pack-objects", logEntry.Data["top_cpu_1.command_1.name"])
		require.Equal(t, 102, logEntry.Data["top_cpu_1.command_1.pid"])
		require.Equal(t, int64(90), logEntry.Data["top_cpu_1.command_1.cpu_ms"])

		// Only the top maxLoggedCommands commands by CPU are logged even
		// though e2 has seven.
		require.Equal(t, int64(30), logEntry.Data["top_cpu_1.command_5.cpu_ms"])
		require.NotContains(t, logEntry.Data, "top_cpu_1.command_6.name")

		// e3's only command has completed and been removed from tracking, so
		// its entry has totals but no per-command breakdown.
		require.Equal(t, 0, logEntry.Data["top_cpu_2.active_commands"])
		require.NotContains(t, logEntry.Data, "top_cpu_2.command_1.name")
	})

	t.Run("SortByMemory", func(t *testing.T) {
		// Only e2 has running commands, so only it carries a live memory
		// burden. e1 and e3 have zero memory and sort arbitrarily after it.
		sorted := bm.entriesSortedBy(SortByMemory)
		require.Len(t, sorted, 3)
		require.Equal(t, e2.ID, sorted[0].ID)

		hook.Reset()
		bm.logStats()

		entries := hook.AllEntries()
		require.Len(t, entries, 1)
		logEntry := entries[0]
		require.Equal(t, "burden monitor stats", logEntry.Message)

		require.Equal(t, "foo.Service/B", logEntry.Data["top_memory_1.rpc"])
		require.Equal(t, int64(28000), logEntry.Data["top_memory_1.total_memory_bytes"])
		require.Contains(t, logEntry.Data, "top_memory_2.rpc")
		require.Contains(t, logEntry.Data, "top_memory_3.rpc")
	})

	t.Run("SortByDuration", func(t *testing.T) {
		// Stagger start times so ordering is deterministic.
		now := time.Now()
		e1.StartTime = now.Add(-3 * time.Second)
		e2.StartTime = now.Add(-1 * time.Second)
		e3.StartTime = now.Add(-2 * time.Second)

		sorted := bm.entriesSortedBy(SortByDuration)
		require.Len(t, sorted, 3)
		assert.Equal(t, e1.ID, sorted[0].ID) // oldest
		assert.Equal(t, e3.ID, sorted[1].ID)
		assert.Equal(t, e2.ID, sorted[2].ID) // newest
	})
}

func TestBurdenMonitor_GetTopNEntries(t *testing.T) {
	t.Parallel()

	bm := New(testhelper.SharedLogger(t))
	ctx := testhelper.Context(t)

	_, e1 := bm.RegisterRPC(ctx, "/foo.Service/A")
	_, e2 := bm.RegisterRPC(ctx, "/foo.Service/B")
	_, e3 := bm.RegisterRPC(ctx, "/foo.Service/C")

	// CPU: e1=50ms, e2=200ms, e3=100ms
	e1.RegisterCommand(101, "git upload-pack", time.Now())
	e1.MarkCommandCompleted(101, 30*time.Millisecond, 20*time.Millisecond)
	e2.RegisterCommand(102, "git pack-objects", time.Now())
	e2.MarkCommandCompleted(102, 130*time.Millisecond, 70*time.Millisecond)
	e3.RegisterCommand(103, "git rev-list", time.Now())
	e3.MarkCommandCompleted(103, 70*time.Millisecond, 30*time.Millisecond)

	t.Run("returns top N sorted descending", func(t *testing.T) {
		top := bm.GetTopNEntries(2, SortByCPU)
		require.Len(t, top, 2)
		require.Equal(t, e2.ID, top[0].ID)
		require.Equal(t, e3.ID, top[1].ID)
	})

	t.Run("n exceeds entry count returns all entries", func(t *testing.T) {
		top := bm.GetTopNEntries(10, SortByCPU)
		require.Len(t, top, 3)
	})

	t.Run("empty monitor returns no entries", func(t *testing.T) {
		bm2 := New(testhelper.SharedLogger(t))
		require.Empty(t, bm2.GetTopNEntries(5, SortByCPU))
	})
}

func TestRPCEntry_CommandTracking(t *testing.T) {
	t.Parallel()

	bm := New(testhelper.SharedLogger(t))
	_, entry := bm.RegisterRPC(testhelper.Context(t), "/foo.Service/Method")

	require.Equal(t, 0, entry.ActiveCommandCount())
	require.Equal(t, time.Duration(0), entry.TotalCPUTime())

	now := time.Now()
	entry.RegisterCommand(201, "git upload-pack", now)
	entry.RegisterCommand(202, "git pack-objects", now)
	require.Equal(t, 2, entry.ActiveCommandCount())

	entry.MarkCommandCompleted(201, 100*time.Millisecond, 50*time.Millisecond)
	require.Equal(t, 1, entry.ActiveCommandCount())
	require.Equal(t, 150*time.Millisecond, entry.TotalCPUTime())

	// Completed commands are removed from the map.
	require.NotContains(t, entry.Commands, 201)
	require.Equal(t, 1, entry.completedCount)
	require.Equal(t, 150*time.Millisecond, entry.completedCPUTime)

	// RSS of a completed command is not part of the live memory burden.
	require.Equal(t, int64(0), entry.TotalMemory())

	entry.MarkCommandCompleted(202, 200*time.Millisecond, 100*time.Millisecond)
	require.Equal(t, 0, entry.ActiveCommandCount())
	require.Equal(t, 450*time.Millisecond, entry.TotalCPUTime())

	require.Empty(t, entry.Commands)
	require.Equal(t, 2, entry.completedCount)
}

func TestRPCEntry_CommandCap(t *testing.T) {
	t.Parallel()

	bm := New(testhelper.SharedLogger(t))
	_, entry := bm.RegisterRPC(testhelper.Context(t), "/foo.Service/Method")

	now := time.Now()
	// Fill the map to the cap with commands of strictly increasing CPU
	// consumption.
	for i := range maxTrackedCommands {
		pid := 1000 + i
		entry.RegisterCommand(pid, "git cat-file", now)
		entry.Commands[pid].UserTime = time.Duration(i+1) * time.Millisecond
	}
	require.Equal(t, maxTrackedCommands, entry.ActiveCommandCount())
	totalBefore := entry.TotalCPUTime()
	evictedBefore := testutil.ToFloat64(commandsEvictedTotal.WithLabelValues("foo.Service", "Method"))

	// Registering one more evicts the lowest-CPU command (pid 1000, 1ms) to
	// make room.
	entry.RegisterCommand(2000, "git rev-list", now)
	require.Equal(t, maxTrackedCommands, entry.ActiveCommandCount())
	require.NotContains(t, entry.Commands, 1000)
	require.Contains(t, entry.Commands, 2000)
	require.Equal(t, evictedBefore+1, testutil.ToFloat64(commandsEvictedTotal.WithLabelValues("foo.Service", "Method")))

	// The evicted command's observed CPU leaves the totals until the command
	// completes.
	require.Equal(t, totalBefore-time.Millisecond, entry.TotalCPUTime())

	// When the evicted command completes, its final consumption — including
	// anything it burned after eviction — is folded into the aggregates.
	entry.MarkCommandCompleted(1000, 5*time.Millisecond, 0)
	require.Equal(t, totalBefore+4*time.Millisecond, entry.TotalCPUTime())

	// Re-registering an already-tracked PID replaces it in place and does not
	// evict.
	entry.RegisterCommand(2000, "git rev-list", now)
	require.Equal(t, maxTrackedCommands, entry.ActiveCommandCount())
	require.Contains(t, entry.Commands, 1001)
	require.Equal(t, evictedBefore+1, testutil.ToFloat64(commandsEvictedTotal.WithLabelValues("foo.Service", "Method")))
}

func TestRPCEntry_MarkCommandCompleted_UntrackedPID(t *testing.T) {
	t.Parallel()

	bm := New(testhelper.SharedLogger(t))
	_, entry := bm.RegisterRPC(testhelper.Context(t), "/foo.Service/Method")

	// A completion for a PID that is no longer tracked (i.e. evicted) still
	// folds its consumption into the aggregates.
	entry.MarkCommandCompleted(9999, 100*time.Millisecond, 50*time.Millisecond)
	require.Equal(t, 0, entry.ActiveCommandCount())
	require.Equal(t, 150*time.Millisecond, entry.TotalCPUTime())
	require.Equal(t, 1, entry.completedCount)
}

func TestUnaryInterceptor(t *testing.T) {
	t.Parallel()

	bm := New(testhelper.SharedLogger(t))
	info := &grpc.UnaryServerInfo{FullMethod: "/gitaly.RepositoryService/RepositoryExists"}

	t.Run("feature flag enabled", func(t *testing.T) {
		ctx := featureflag.IncomingCtxWithFeatureFlag(
			testhelper.Context(t), featureflag.BurdenMonitorTrackCommands, true,
		)

		var handlerCtx context.Context
		_, err := bm.UnaryInterceptor()(ctx, nil, info, func(ctx context.Context, req interface{}) (interface{}, error) {
			handlerCtx = ctx
			require.Len(t, bm.Entries(), 1)
			return nil, nil
		})
		require.NoError(t, err)

		entry, ok := rpcEntryFromContext(handlerCtx)
		require.True(t, ok)
		require.Equal(t, "gitaly.RepositoryService", entry.ServiceName)
		require.Equal(t, "RepositoryExists", entry.MethodName)

		// Entry is deregistered after the handler returns.
		require.Empty(t, bm.Entries())
	})

	t.Run("feature flag disabled", func(t *testing.T) {
		ctx := featureflag.IncomingCtxWithFeatureFlag(
			testhelper.Context(t), featureflag.BurdenMonitorTrackCommands, false,
		)

		_, err := bm.UnaryInterceptor()(ctx, nil, info, func(ctx context.Context, req interface{}) (interface{}, error) {
			require.Empty(t, bm.Entries())
			_, ok := rpcEntryFromContext(ctx)
			require.False(t, ok)
			return nil, nil
		})
		require.NoError(t, err)
	})
}

func TestStreamInterceptor(t *testing.T) {
	t.Parallel()

	bm := New(testhelper.SharedLogger(t))
	info := &grpc.StreamServerInfo{FullMethod: "/gitaly.RepositoryService/RepositoryExists"}

	t.Run("feature flag enabled", func(t *testing.T) {
		ctx := featureflag.IncomingCtxWithFeatureFlag(
			testhelper.Context(t), featureflag.BurdenMonitorTrackCommands, true,
		)
		stream := &mockServerStream{ctx: ctx}

		var handlerStream grpc.ServerStream
		err := bm.StreamInterceptor()(nil, stream, info, func(srv interface{}, s grpc.ServerStream) error {
			handlerStream = s
			require.Len(t, bm.Entries(), 1)
			return nil
		})
		require.NoError(t, err)

		entry, ok := rpcEntryFromContext(handlerStream.Context())
		require.True(t, ok)
		require.Equal(t, "gitaly.RepositoryService", entry.ServiceName)
		require.Equal(t, "RepositoryExists", entry.MethodName)

		require.Empty(t, bm.Entries())
	})

	t.Run("feature flag disabled", func(t *testing.T) {
		ctx := featureflag.IncomingCtxWithFeatureFlag(
			testhelper.Context(t), featureflag.BurdenMonitorTrackCommands, false,
		)
		stream := &mockServerStream{ctx: ctx}

		err := bm.StreamInterceptor()(nil, stream, info, func(srv interface{}, s grpc.ServerStream) error {
			require.Empty(t, bm.Entries())
			_, ok := rpcEntryFromContext(s.Context())
			require.False(t, ok)
			return nil
		})
		require.NoError(t, err)
	})
}

func TestNotifyCommandStarted(t *testing.T) {
	t.Parallel()

	t.Run("no entry in context", func(t *testing.T) {
		// Should be a no-op and not panic.
		NotifyCommandStarted(testhelper.Context(t), 1234, "git upload-pack", time.Now())
	})

	t.Run("entry in context", func(t *testing.T) {
		entry := &RPCEntry{Commands: make(map[int]*CommandStats)}
		ctx := contextWithRPCEntry(testhelper.Context(t), entry)

		NotifyCommandStarted(ctx, 5678, "git pack-objects", time.Now())

		require.Equal(t, 1, entry.ActiveCommandCount())
	})
}

func TestNotifyCommandCompleted(t *testing.T) {
	t.Parallel()

	t.Run("no entry in context", func(t *testing.T) {
		// Should be a no-op and not panic.
		NotifyCommandCompleted(testhelper.Context(t), 1234, 10*time.Millisecond, 5*time.Millisecond)
	})

	t.Run("entry in context", func(t *testing.T) {
		entry := &RPCEntry{Commands: make(map[int]*CommandStats)}
		ctx := contextWithRPCEntry(testhelper.Context(t), entry)

		NotifyCommandStarted(ctx, 5678, "git pack-objects", time.Now())
		NotifyCommandCompleted(ctx, 5678, 100*time.Millisecond, 50*time.Millisecond)

		require.Equal(t, 0, entry.ActiveCommandCount())
		require.Equal(t, 150*time.Millisecond, entry.TotalCPUTime())
		require.NotContains(t, entry.Commands, 5678)
	})
}
