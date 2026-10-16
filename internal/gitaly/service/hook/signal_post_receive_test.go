package hook

import (
	"testing"

	gitalyhook "github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

func TestSignalPostReceiveReady(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	postReceiveRegistry := gitalyhook.NewPostReceiveRegistry()

	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		deps.PostReceiveRegistry = postReceiveRegistry
		gitalypb.RegisterHookServiceServer(srv, NewServer(deps))
	}, testserver.WithDisablePraefect())

	client, conn := newHooksClient(t, cfg.SocketPath)
	t.Cleanup(func() { conn.Close() })

	storageName := cfg.Storages[0].Name

	// Note: These tests will fail with PermissionDenied because they're not called via
	// Praefect's backchannel. This is the expected security behavior - the RPC should
	// only be callable from Praefect. Integration tests cover the full backchannel flow.

	t.Run("rejects call without backchannel", func(t *testing.T) {
		_, err := client.SignalPostReceiveReady(ctx, &gitalypb.SignalPostReceiveReadyRequest{
			StorageName:   storageName,
			TransactionId: 12345,
		})
		testhelper.RequireGrpcCode(t, err, codes.PermissionDenied)
		require.Contains(t, err.Error(), "can only be called via Praefect backchannel")
	})

	// Note: SignalPostReceiveReady checks for a Praefect backchannel before it
	// validates the request, so without a backchannel every call is rejected with
	// PermissionDenied regardless of its arguments. The transaction_id == 0 guard
	// is only reachable via a real backchannel and is covered by integration tests.

	t.Run("backchannel check precedes request validation", func(t *testing.T) {
		// A zero transaction ID would be rejected by the arg validation, but the
		// backchannel guard fires first, so we still get PermissionDenied here.
		_, err := client.SignalPostReceiveReady(ctx, &gitalypb.SignalPostReceiveReadyRequest{
			StorageName:   storageName,
			TransactionId: 0,
		})
		testhelper.RequireGrpcCode(t, err, codes.PermissionDenied)
	})
}
