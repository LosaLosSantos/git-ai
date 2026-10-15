package log_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/testhelper"
	grpcmwloggingv2 "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/interop/grpc_testing"
)

// syncRecorder wraps an *os.File so a test can both persist log output to a real
// file on disk and observe how many times that output was flushed via Sync.
type syncRecorder struct {
	*os.File
	syncs atomic.Int32
}

func (r *syncRecorder) Sync() error {
	r.syncs.Add(1)
	return r.File.Sync()
}

// blockingTestService is a grpc_testing.TestService whose UnaryCall logs a
// message, signals that it has started, and then blocks until it is released.
// This lets a test hold an RPC in-flight while the server is shut down.
type blockingTestService struct {
	grpc_testing.UnimplementedTestServiceServer
	logger   log.Logger
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (s *blockingTestService) UnaryCall(context.Context, *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
	defer close(s.finished)

	s.logger.Info("rpc handler started")
	close(s.started)
	<-s.release
	s.logger.Info("rpc handler finished")
	return &grpc_testing.SimpleResponse{}, nil
}

// loggingServerHarness holds all necessary objects to run the test
// and validate that the logs have been flushed before the server shutsdown.
type loggingServerHarness struct {
	client   grpc_testing.TestServiceClient
	server   *grpc.Server
	svc      *blockingTestService
	recorder *syncRecorder
	logger   log.Logger
	logPath  string
	serveErr chan error
}

func TestConvertLoggingFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		desc     string
		input    grpcmwloggingv2.Fields
		expected map[string]any
	}{
		{
			desc:     "Converting v2 logging fields to map[string]any, even number of fields",
			input:    grpcmwloggingv2.Fields{"k1", "v1", "k2", "v2"},
			expected: map[string]any{"k1": "v1", "k2": "v2"},
		},
		{
			desc:     "Converting v2 logging fields to map[string]any, odd number of fields",
			input:    grpcmwloggingv2.Fields{"k1", "v1", "k2"},
			expected: map[string]any{"k1": "v1", "k2": ""},
		},
		{
			desc:     "Converting v2 logging fields to map[string]any, duplicate keys",
			input:    grpcmwloggingv2.Fields{"k1", "v1", "k1", "v2"},
			expected: map[string]any{"k1": "v2"},
		},
		{
			desc:     "Converting v2 logging fields to map[string]any, empty input",
			input:    grpcmwloggingv2.Fields{},
			expected: map[string]any{},
		},
		{
			desc:     "Converting v2 logging fields to map[string]any, nil input",
			input:    nil,
			expected: map[string]any{},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			actual := log.ConvertLoggingFields(tc.input)
			require.Equal(t, tc.expected, actual)
		})
	}
}

// TestFlush_persistsInFlightLogsOnGracefulStop verifies that when the server is
// gracefully shut down while an RPC is still in-flight, all messages logged by
// that RPC are flushed to disk by log.Flush and can be recovered from the file.
func TestFlush_persistsInFlightLogsOnGracefulStop(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	h := setupLoggingServer(t, ctx)

	callErr := make(chan error, 1)
	go func() {
		_, err := h.client.UnaryCall(ctx, &grpc_testing.SimpleRequest{})
		callErr <- err
	}()

	// Wait until the RPC is in-flight inside the handler. At this point
	// "rpc handler started" has been logged.
	<-h.svc.started

	// GracefulStop blocks until in-flight RPCs finish, mirroring Gitaly's
	// shutdown. Release the handler so it can complete and log its final
	// message during the shutdown window.
	stopped := make(chan struct{})
	go func() {
		h.server.GracefulStop()
		close(stopped)
	}()
	close(h.svc.release)
	<-stopped

	require.NoError(t, <-callErr)
	require.NoError(t, <-h.serveErr)

	// Validate that Flush has been called 0 times yet.
	require.Zero(t, h.recorder.syncs.Load())

	// Now flush logs
	require.NoError(t, h.logger.Flush())
	require.Equal(t, int32(1), h.recorder.syncs.Load())

	// Read the logs back from a separate handle to confirm they were persisted
	// to the file rather than dropped on shutdown.
	persisted := string(testhelper.MustReadFile(t, h.logPath))
	require.Contains(t, persisted, "rpc handler started")
	require.Contains(t, persisted, "rpc handler finished")
}

// TestFlush_persistsAlreadyLoggedMessagesOnHardStop verifies that messages that
// were already logged by an in-flight RPC survive a hard server stop (which
// does not wait for in-flight RPCs) and are flushed to disk on shutdown.
func TestFlush_persistsAlreadyLoggedMessagesOnHardStop(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	h := setupLoggingServer(t, ctx)

	callErr := make(chan error, 1)
	go func() {
		_, err := h.client.UnaryCall(ctx, &grpc_testing.SimpleRequest{})
		callErr <- err
	}()

	// Wait until the RPC is in-flight; "rpc handler started" is now logged.
	<-h.svc.started

	// A hard stop does not wait for in-flight RPCs to finish, so it returns
	// while the handler is still blocked.
	h.server.Stop()
	require.NoError(t, <-h.serveErr)

	// The message logged before the stop must not be lost: flushing on shutdown
	// persists it to disk.
	require.Zero(t, h.recorder.syncs.Load())

	require.NoError(t, h.logger.Flush())
	require.Equal(t, int32(1), h.recorder.syncs.Load())

	persisted := string(testhelper.MustReadFile(t, h.logPath))
	require.Contains(t, persisted, "rpc handler started")

	// Let the now-cancelled handler unwind. Wait for it to finish writing and
	// drain the client result so the goroutine doesn't outlive the test and
	// race with the log file being closed during cleanup.
	close(h.svc.release)
	<-h.svc.finished
	<-callErr
}

// setupLoggingServer starts a real gRPC server whose logger is configured just
// like the Gitaly serve path: a sync writer wrapping a file-backed writer. The
// returned struct allows a test to drive an in-flight RPC, shut the server
// down and then assert on what was persisted to disk.
func setupLoggingServer(t *testing.T, ctx context.Context) *loggingServerHarness {
	t.Helper()

	// Create the log file
	logPath := filepath.Join(testhelper.TempDir(t), "gitaly.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)

	// Close the file on test exit
	t.Cleanup(func() { require.NoError(t, logFile.Close()) })

	recorder := &syncRecorder{File: logFile}

	logger, err := log.Configure(log.NewSyncWriter(recorder), "json", "info")
	require.NoError(t, err)

	svc := &blockingTestService{
		logger:   logger,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}

	server := grpc.NewServer(grpc.ChainUnaryInterceptor(logger.UnaryServerInterceptor()))
	grpc_testing.RegisterTestServiceServer(server, svc)

	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	// serveErr is used to collect the error (or nil) when the server shuts down
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	cc, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cc.Close()) })

	return &loggingServerHarness{
		client:   grpc_testing.NewTestServiceClient(cc),
		server:   server,
		svc:      svc,
		recorder: recorder,
		logger:   logger,
		logPath:  logPath,
		serveErr: serveErr,
	}
}
