package cli

import (
	"context"
	"time"

	gitalyauth "github.com/feder-cr/open_scion/auth"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/tracing"
	"google.golang.org/grpc"
)

// Dial creates a gRPC connection to a Gitaly or Praefect server with the specified timeout and options.
// It automatically adds the standard unary and stream interceptors and authentication credentials if a token is provided.
func Dial(ctx context.Context, addr, token string, timeout time.Duration, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	opts = append(opts,
		grpc.WithStatsHandler(tracing.NewGRPCClientStatsHandler()),
		client.UnaryInterceptor(),
		client.StreamInterceptor(),
	)

	if len(token) > 0 {
		opts = append(opts,
			grpc.WithPerRPCCredentials(
				gitalyauth.RPCCredentialsV2(token),
			),
		)
	}

	return client.New(ctx, addr, client.WithGrpcOptions(opts))
}
