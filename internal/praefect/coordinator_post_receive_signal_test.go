package praefect

import (
	"context"
	"crypto/sha1"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/praefect/datastore"
	"github.com/feder-cr/open_scion/internal/praefect/transactions"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

// fakeHookService captures SignalPostReceiveReady calls so tests can assert
// whether (and with what failure flag) the post-receive hook was signaled.
type fakeHookService struct {
	gitalypb.UnimplementedHookServiceServer

	mu       sync.Mutex
	called   bool
	failed   bool
	txID     uint64
	signalAt time.Time

	// onSignal, if set, runs inside SignalPostReceiveReady so a test can observe
	// state (e.g. the generation number) at the exact moment the hook is signaled.
	onSignal func()
}

func (f *fakeHookService) SignalPostReceiveReady(
	_ context.Context,
	req *gitalypb.SignalPostReceiveReadyRequest,
) (*gitalypb.SignalPostReceiveReadyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.failed = req.GetPostTransactionFailed()
	f.txID = req.GetTransactionId()
	f.signalAt = time.Now()
	if f.onSignal != nil {
		f.onSignal()
	}
	return &gitalypb.SignalPostReceiveReadyResponse{}, nil
}

// runFakeHookService serves a fakeHookService on a loopback listener and returns it
// alongside a connection that is ready to use.
func runFakeHookService(t *testing.T, ctx context.Context) (*fakeHookService, *grpc.ClientConn) {
	t.Helper()

	svc := &fakeHookService{}
	srv := grpc.NewServer()
	gitalypb.RegisterHookServiceServer(srv, svc)

	ln, _ := testhelper.GetLocalhostListener(t)
	go testhelper.MustServe(t, srv, ln)
	t.Cleanup(srv.Stop)

	// Dial the listener's address literal rather than its "localhost:port" form: a
	// hostname sends gRPC through its DNS resolver, which adds name resolution to the
	// connection setup that the signal RPC would have to fit into its timeout.
	conn, err := client.New(ctx, "tcp://"+ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { testhelper.MustClose(t, conn) })

	// client.New only hands out a dormant connection. Establish it upfront so that the
	// signal RPC doesn't have to complete the dial and the HTTP/2 handshake within its
	// own timeout, which is a wall-clock race on a loaded machine.
	conn.Connect()
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for state := conn.GetState(); state != connectivity.Ready; state = conn.GetState() {
		require.True(t, conn.WaitForStateChange(connectCtx, state),
			"connection did not become ready, last state: %s", state)
	}

	return svc, conn
}

// newSignalingCoordinator returns a Coordinator set up to signal the post-receive hook.
// The signal timeout is deliberately generous: the connection is established before the
// signal is sent, so a slower machine must not turn into a test failure.
func newSignalingCoordinator(t *testing.T) *Coordinator {
	t.Helper()

	return &Coordinator{
		logger:                   testhelper.NewLogger(t),
		postReceiveSignalTimeout: 30 * time.Second,
	}
}

// TestCoordinator_monitorCommittedAndSignal verifies the success-path ordering
// the fix depends on: once the committed phase concludes, the DB write (generation
// increment) runs *before* the PostReceiveHook is signaled, and the signal carries
// the DB-write outcome. It also verifies that no signal is sent when the RPC ends
// before the committed phase concludes.
func TestCoordinator_monitorCommittedAndSignal(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	setup := func(t *testing.T) (*Coordinator, *fakeHookService, *grpc.ClientConn) {
		svc, conn := runFakeHookService(t, ctx)

		return newSignalingCoordinator(t), svc, conn
	}

	t.Run("committed concluded: DB write happens before signal, success", func(t *testing.T) {
		t.Parallel()
		c, svc, conn := setup(t)

		var (
			dbWriteCalled bool
			dbWriteAt     time.Time
		)
		dbWrite := func(context.Context) error {
			dbWriteCalled = true
			dbWriteAt = time.Now()
			return nil
		}

		concluded := make(chan struct{})
		tx := mockTransaction{committedConcluded: concluded}

		done := make(chan error, 1)
		go func() {
			done <- c.monitorCommittedAndSignal(ctx, tx, dbWrite, &postReceiveSignal{
				transactionID:  42,
				primaryConn:    conn,
				primaryStorage: "primary",
			})
		}()

		close(concluded)
		require.NoError(t, <-done)

		svc.mu.Lock()
		defer svc.mu.Unlock()
		require.True(t, dbWriteCalled, "DB write must run on the committed-concluded path")
		require.True(t, svc.called, "post-receive must be signaled")
		require.False(t, svc.failed, "signal must indicate success when the DB write succeeded")
		require.Equal(t, uint64(42), svc.txID)
		require.False(t, svc.signalAt.Before(dbWriteAt),
			"signal must be sent only after the DB write (generation increment)")
	})

	t.Run("committed concluded: DB write failure signals failure", func(t *testing.T) {
		t.Parallel()
		c, svc, conn := setup(t)

		dbWrite := func(context.Context) error {
			return errors.New("increment generation failed")
		}

		concluded := make(chan struct{})
		close(concluded)

		require.NoError(t, c.monitorCommittedAndSignal(ctx, mockTransaction{committedConcluded: concluded}, dbWrite,
			&postReceiveSignal{transactionID: 7, primaryConn: conn, primaryStorage: "primary"}))

		svc.mu.Lock()
		defer svc.mu.Unlock()
		require.True(t, svc.called, "post-receive must still be signaled to avoid a hung hook")
		require.True(t, svc.failed, "signal must carry failure when the DB write failed")
	})

	t.Run("committed phase never concludes: monitor times out", func(t *testing.T) {
		t.Parallel()
		c, svc, conn := setup(t)
		c.committedWaitTimeout = time.Millisecond

		var dbWriteCalled bool
		dbWrite := func(context.Context) error {
			dbWriteCalled = true
			return nil
		}

		// Never-closing latch and a live context: only the timeout can fire.
		err := c.monitorCommittedAndSignal(ctx, mockTransaction{committedConcluded: make(chan struct{})},
			dbWrite, &postReceiveSignal{transactionID: 3, primaryConn: conn, primaryStorage: "primary"})
		require.ErrorContains(t, err, "waiting for committed phase to conclude")

		svc.mu.Lock()
		defer svc.mu.Unlock()
		require.False(t, dbWriteCalled, "DB write must not run when the committed phase never concludes")
		require.False(t, svc.called, "post-receive must not be signaled when the monitor times out")
	})

	t.Run("context canceled before committed: no DB write, no signal", func(t *testing.T) {
		t.Parallel()
		c, svc, conn := setup(t)

		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()

		var dbWriteCalled bool
		dbWrite := func(context.Context) error {
			dbWriteCalled = true
			return nil
		}

		// Never-closing latch: only ctx.Done() can fire.
		require.NoError(t, c.monitorCommittedAndSignal(canceledCtx, mockTransaction{committedConcluded: make(chan struct{})},
			dbWrite, &postReceiveSignal{transactionID: 1, primaryConn: conn, primaryStorage: "primary"}))

		svc.mu.Lock()
		defer svc.mu.Unlock()
		require.False(t, dbWriteCalled, "DB write must not run when the committed phase never concludes")
		require.False(t, svc.called, "post-receive must not be signaled when committed phase never concludes")
	})
}

// TestCoordinator_monitorCommittedAndSignal_realVotes drives the committed latch
// through the *real* transaction manager via the RefTransaction RPC — the same path
// a reference-transaction hook on a Gitaly node uses — rather than hand-closing the
// latch channel like TestCoordinator_monitorCommittedAndSignal does. It proves the
// producer half of the work item #5406 fix end-to-end on the Praefect side:
//
//   - PREPARING and PREPARED votes conclude their own subtransactions but do NOT
//     signal the post-receive hook;
//   - the COMMITTED vote concludes the committed latch, which makes the monitor
//     record the generation write and then signal the hook;
//   - the generation write happens-before the signal (the core #5406 invariant).
//
// The post-receive hook itself (the consumer of the signal) is stood in for by
// fakeHookService. The consumer half — the hook actually blocking on the signal and
// failing on signal-failure/timeout — is covered by
// TestPostReceive_praefectSignalCoordination in internal/gitaly/hook.
func TestCoordinator_monitorCommittedAndSignal_realVotes(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	// fakeHookService stands in for the waiting post-receive hook on the primary.
	svc, conn := runFakeHookService(t, ctx)

	// Real transaction manager fronted by a real Praefect RefTransaction service.
	conf := testConfig(1)
	txMgr := transactions.NewManager(conf, testhelper.SharedLogger(t), &datastore.NoopWriteLockManager{})
	cc, _, cleanup := RunPraefectServer(t, ctx, conf, BuildOptions{
		WithTxMgr:   txMgr,
		WithNodeMgr: nullNodeMgr{}, // to suppress node address issues
	})
	defer cleanup()
	refClient := gitalypb.NewRefTransactionClient(cc)

	const (
		storageName = "mock"
		relPath     = "real/votes/repo.git"
		node        = "primary"
	)

	// A single voter with threshold 1 reaches quorum on its own vote, so each
	// VoteTransaction returns immediately and we can drive the phases sequentially.
	transaction, txCleanup, err := txMgr.RegisterTransaction(ctx, []transactions.Voter{{Name: node, Votes: 1}}, 1)
	require.NoError(t, err)
	defer func() { require.NoError(t, txCleanup()) }()

	// Model the generation write: dbWrite increments it; the fake hook captures the
	// value observed at signal time — i.e. what the post-receive hook (and hence
	// Rails) would see when it is allowed to proceed.
	var generation, genAtSignal int64
	dbWrite := func(context.Context) error {
		atomic.AddInt64(&generation, 1)
		return nil
	}
	svc.onSignal = func() { atomic.StoreInt64(&genAtSignal, atomic.LoadInt64(&generation)) }

	c := newSignalingCoordinator(t)
	monitorDone := make(chan error, 1)
	go func() {
		monitorDone <- c.monitorCommittedAndSignal(ctx, transaction, dbWrite, &postReceiveSignal{
			transactionID:  transaction.ID(),
			primaryConn:    conn,
			primaryStorage: node,
		})
	}()

	hash := sha1.Sum([]byte(relPath))
	vote := func(phase gitalypb.VoteTransactionRequest_Phase) {
		_, err := refClient.VoteTransaction(ctx, &gitalypb.VoteTransactionRequest{
			TransactionId:        transaction.ID(),
			Repository:           &gitalypb.Repository{StorageName: storageName, RelativePath: relPath},
			Node:                 node,
			ReferenceUpdatesHash: hash[:],
			Phase:                phase,
		})
		require.NoError(t, err)
	}

	signaled := func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		return svc.called
	}

	// Preparing and prepared must NOT conclude the committed latch: the latch can
	// only be closed by the COMMITTED vote, so the hook is not signaled yet and no
	// generation write has happened.
	vote(gitalypb.VoteTransactionRequest_PREPARING_PHASE)
	require.False(t, signaled(), "preparing-phase vote must not signal the post-receive hook")
	require.Zero(t, atomic.LoadInt64(&generation), "no generation write before committed phase")

	vote(gitalypb.VoteTransactionRequest_PREPARED_PHASE)
	require.False(t, signaled(), "prepared-phase vote must not signal the post-receive hook")
	require.Zero(t, atomic.LoadInt64(&generation), "no generation write before committed phase")

	// The committed-phase vote concludes the latch: the monitor records the
	// generation write and then signals the waiting hook.
	vote(gitalypb.VoteTransactionRequest_COMMITTED_PHASE)

	select {
	case err := <-monitorDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("monitor did not return after committed phase concluded")
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()
	require.True(t, svc.called, "committed-phase vote must signal the post-receive hook")
	require.False(t, svc.failed, "signal must indicate success when the generation write succeeded")
	require.Equal(t, transaction.ID(), svc.txID)
	require.Equal(t, int64(1), atomic.LoadInt64(&generation), "generation must be incremented exactly once")
	require.Equal(t, int64(1), atomic.LoadInt64(&genAtSignal),
		"generation must be incremented BEFORE the post-receive hook is signaled (work item #5406)")
}
