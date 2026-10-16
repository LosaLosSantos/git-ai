package praefect

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/praefect/config"
	"github.com/feder-cr/git-ai/internal/praefect/datastore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mockChecksumServer is a minimal RepositoryServiceServer that returns
// configurable checksums per storage name.
type mockChecksumServer struct {
	gitalypb.UnimplementedRepositoryServiceServer
	mu       sync.Mutex
	checksum string
	err      error
}

func (m *mockChecksumServer) CalculateChecksum(_ context.Context, _ *gitalypb.CalculateChecksumRequest) (*gitalypb.CalculateChecksumResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return &gitalypb.CalculateChecksumResponse{Checksum: m.checksum}, nil
}

// startChecksumServer starts a real gRPC server backed by mockChecksumServer
// and returns a connected *grpc.ClientConn.
func startChecksumServer(t *testing.T, srv *mockChecksumServer) *grpc.ClientConn {
	t.Helper()
	ctx := testhelper.Context(t)

	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := grpc.NewServer()
	gitalypb.RegisterRepositoryServiceServer(s, srv)
	t.Cleanup(s.Stop)
	go func() { _ = s.Serve(ln) }()

	conn, err := client.New(ctx, fmt.Sprintf("tcp://%s", ln.Addr()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// simpleMockPTS is a bare-minimum PendingTransactionStore for tests.
type simpleMockPTS struct {
	mu      sync.Mutex
	records []datastore.PendingTransaction
	deleted []int64
}

func (m *simpleMockPTS) RecordPendingTransaction(_ context.Context, _ datastore.PendingTransaction) (int64, error) {
	panic("not expected in recovery tests")
}

func (m *simpleMockPTS) DeletePendingTransaction(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, id)
	return nil
}

func (m *simpleMockPTS) GetPendingTransactions(_ context.Context) ([]datastore.PendingTransaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]datastore.PendingTransaction(nil), m.records...), nil
}

func TestRecoverPendingTransactions(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	const (
		vs      = "vs-1"
		repoID  = int64(1)
		relPath = "rel/path"
		repPath = "rep/path"
		primary = "primary"
		sec1    = "secondary-1"
		sec2    = "secondary-2"
	)

	checksumA := "aabbcc"
	checksumB := "ddeeff"

	t.Run("no pending transactions is a no-op", func(t *testing.T) {
		pts := &simpleMockPTS{}
		rs := datastore.MockRepositoryStore{}
		queue := datastore.NewReplicationEventQueueInterceptor(datastore.NewMemoryReplicationEventQueue(config.Config{}))

		err := RecoverPendingTransactions(ctx, pts, rs, queue, Connections{}, testhelper.SharedLogger(t))
		require.NoError(t, err)
		require.Empty(t, queue.GetEnqueued())
	})

	t.Run("increment not yet done: primary and updated secondary match", func(t *testing.T) {
		primarySrv := &mockChecksumServer{checksum: checksumA}
		sec1Srv := &mockChecksumServer{checksum: checksumA} // matches primary
		sec2Srv := &mockChecksumServer{checksum: checksumB} // diverged

		conns := Connections{vs: {
			primary: startChecksumServer(t, primarySrv),
			sec1:    startChecksumServer(t, sec1Srv),
			sec2:    startChecksumServer(t, sec2Srv),
		}}

		var incrementedPrimary string
		var incrementedSecondaries []string
		rs := datastore.MockRepositoryStore{
			IncrementGenerationFunc: func(_ context.Context, _ int64, p string, s []string) error {
				incrementedPrimary = p
				incrementedSecondaries = s
				return nil
			},
		}
		queue := datastore.NewReplicationEventQueueInterceptor(datastore.NewMemoryReplicationEventQueue(config.Config{}))

		pts := &simpleMockPTS{records: []datastore.PendingTransaction{{
			ID: 1, RepositoryID: repoID, VirtualStorage: vs,
			RelativePath: relPath, ReplicaPath: repPath,
			PrimaryStorage: primary,
			ExpectedVoters: []string{primary, sec1, sec2},
		}}}

		require.NoError(t, RecoverPendingTransactions(ctx, pts, rs, queue, conns, testhelper.SharedLogger(t)))

		require.Equal(t, primary, incrementedPrimary)
		require.Equal(t, []string{sec1}, incrementedSecondaries)

		// sec2 (diverged) gets a replication job
		enqueued := queue.GetEnqueued()
		require.Len(t, enqueued, 1)
		assert.Equal(t, sec2, enqueued[0].Job.TargetNodeStorage)
		assert.Equal(t, primary, enqueued[0].Job.SourceNodeStorage)

		// pending record deleted
		require.Equal(t, []int64{1}, pts.deleted)
	})

	t.Run("IncrementGeneration always called even if generation already advanced", func(t *testing.T) {
		primarySrv := &mockChecksumServer{checksum: checksumA}
		sec1Srv := &mockChecksumServer{checksum: checksumA}

		conns := Connections{vs: {
			primary: startChecksumServer(t, primarySrv),
			sec1:    startChecksumServer(t, sec1Srv),
		}}

		incrementCalled := false
		rs := datastore.MockRepositoryStore{
			IncrementGenerationFunc: func(_ context.Context, _ int64, _ string, _ []string) error {
				incrementCalled = true
				return nil
			},
		}
		queue := datastore.NewReplicationEventQueueInterceptor(datastore.NewMemoryReplicationEventQueue(config.Config{}))

		pts := &simpleMockPTS{records: []datastore.PendingTransaction{{
			ID: 2, RepositoryID: repoID, VirtualStorage: vs,
			RelativePath: relPath, ReplicaPath: repPath,
			PrimaryStorage: primary,
			ExpectedVoters: []string{primary, sec1},
		}}}

		require.NoError(t, RecoverPendingTransactions(ctx, pts, rs, queue, conns, testhelper.SharedLogger(t)))
		require.True(t, incrementCalled, "IncrementGeneration must always be called during recovery")
		require.Empty(t, queue.GetEnqueued())
		require.Equal(t, []int64{2}, pts.deleted)
	})

	t.Run("primary unreachable: record kept for next startup", func(t *testing.T) {
		primarySrv := &mockChecksumServer{err: status.Error(codes.Unavailable, "down")}

		conns := Connections{vs: {
			primary: startChecksumServer(t, primarySrv),
		}}

		rs := datastore.MockRepositoryStore{}
		queue := datastore.NewReplicationEventQueueInterceptor(datastore.NewMemoryReplicationEventQueue(config.Config{}))

		pts := &simpleMockPTS{records: []datastore.PendingTransaction{{
			ID: 3, RepositoryID: repoID, VirtualStorage: vs,
			RelativePath: relPath, ReplicaPath: repPath,
			PrimaryStorage: primary,
			ExpectedVoters: []string{primary},
		}}}

		// RecoverPendingTransactions itself should not fail — it logs and continues
		require.NoError(t, RecoverPendingTransactions(ctx, pts, rs, queue, conns, testhelper.SharedLogger(t)))

		// record NOT deleted: will be retried next startup
		require.Empty(t, pts.deleted)
	})

	t.Run("secondary with no connection treated as outdated", func(t *testing.T) {
		primarySrv := &mockChecksumServer{checksum: checksumA}

		conns := Connections{vs: {
			primary: startChecksumServer(t, primarySrv),
			// sec1 deliberately absent from conns
		}}

		var incrementedSecondaries []string
		rs := datastore.MockRepositoryStore{
			IncrementGenerationFunc: func(_ context.Context, _ int64, _ string, s []string) error {
				incrementedSecondaries = s
				return nil
			},
		}
		queue := datastore.NewReplicationEventQueueInterceptor(datastore.NewMemoryReplicationEventQueue(config.Config{}))

		pts := &simpleMockPTS{records: []datastore.PendingTransaction{{
			ID: 4, RepositoryID: repoID, VirtualStorage: vs,
			RelativePath: relPath, ReplicaPath: repPath,
			PrimaryStorage: primary,
			ExpectedVoters: []string{primary, sec1},
		}}}

		require.NoError(t, RecoverPendingTransactions(ctx, pts, rs, queue, conns, testhelper.SharedLogger(t)))

		// sec1 has no connection → no updated secondaries
		require.Empty(t, incrementedSecondaries)

		enqueued := queue.GetEnqueued()
		require.Len(t, enqueued, 1)
		assert.Equal(t, sec1, enqueued[0].Job.TargetNodeStorage)
		require.Equal(t, []int64{4}, pts.deleted)
	})
}
