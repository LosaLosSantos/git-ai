package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestStopGRPCServer_ForcesStopOfLongLivedStreams checks that shutdown does not
// wait forever for a stream that only ends when its client cancels, such as a
// WatchChangesets stream the client keeps open.
func TestStopGRPCServer_ForcesStopOfLongLivedStreams(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		close(started)
		<-stream.Context().Done()
		return stream.Context().Err()
	}))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(listener) }()

	conn, err := grpc.NewClient("passthrough:///"+listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/test.Service/Watch")
	require.NoError(t, err)
	require.NoError(t, stream.CloseSend())

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the stream never reached the server")
	}

	require.False(t, stopGRPCServer(srv, 100*time.Millisecond), "the stream outlives the graceful stop timeout")
}

// TestStopGRPCServer_IdleServerStopsGracefully checks that a server without
// running RPCs stops gracefully, within the timeout.
func TestStopGRPCServer_IdleServerStopsGracefully(t *testing.T) {
	t.Parallel()

	require.True(t, stopGRPCServer(grpc.NewServer(), 10*time.Second))
}
