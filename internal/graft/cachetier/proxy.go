package cachetier

import (
	"context"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	grpcclient "github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/grpc/listenmux"
	"github.com/feder-cr/git-ai/internal/grpc/middleware/panichandler"
	"github.com/feder-cr/git-ai/internal/grpc/sidechannel"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

// DialUpstream dials addr for use as the upstream of a cache tier. The
// connection forces the proxy codec, which lets proxied RPCs forward raw
// frames unchanged. It also speaks the sidechannel protocol and proxies
// sidechannels, as Praefect does, so that sidechannel RPCs such as
// SSHUploadPackWithSidechannel pass through the tier. Typed calls work on the
// same connection.
func DialUpstream(ctx context.Context, addr string, logger log.Logger) (*grpc.ClientConn, error) {
	registry := sidechannel.NewRegistry()

	return grpcclient.New(ctx, addr,
		grpcclient.WithHandshaker(sidechannel.NewClientHandshaker(logger, registry)),
		grpcclient.WithGrpcOptions([]grpc.DialOption{
			grpc.WithDefaultCallOptions(grpc.ForceCodec(proxy.NewCodec())),
			grpc.WithChainUnaryInterceptor(sidechannel.NewUnaryProxy(registry, logger)),
			grpc.WithChainStreamInterceptor(sidechannel.NewStreamProxy(registry, logger)),
		}),
	)
}

// NewGRPCServer returns a gRPC server that serves ObjectService from the cache
// and transparently proxies every other RPC to the upstream. It also serves the
// standard gRPC health service without proxying it.
func NewGRPCServer(s *Server) *grpc.Server {
	// Accept backchannel connections, as Praefect does, so that clients which
	// dial with sidechannel support can use the tier. The proxy then opens their
	// sidechannels towards the upstream.
	lm := listenmux.New(insecure.NewCredentials())
	lm.Register(backchannel.NewServerHandshaker(s.logger, backchannel.NewRegistry(), nil))

	srv := grpc.NewServer(
		grpc.Creds(lm),
		grpc.ForceServerCodec(proxy.NewCodec()),
		grpc.UnknownServiceHandler(proxy.TransparentHandler(s.director)),
		// A panic in one request, for example on a damaged cache entry, fails that
		// request instead of killing the process and every other request with it.
		grpc.ChainUnaryInterceptor(panichandler.UnaryPanicHandler(s.logger)),
		grpc.ChainStreamInterceptor(panichandler.StreamPanicHandler(s.logger)),
	)

	gitalypb.RegisterObjectServiceServer(srv, s)

	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, healthServer)

	return srv
}

// director routes a proxied call to the upstream. It peeks at the first request
// frame, which the proxy then forwards unchanged.
func (s *Server) director(ctx context.Context, _ string, peeker proxy.StreamPeeker) (*proxy.StreamParameters, error) {
	frame, err := peeker.Peek()
	if err != nil {
		return nil, structerr.NewInternal("peek first request frame: %v", err)
	}

	outCtx, err := s.outgoingContext(ctx)
	if err != nil {
		return nil, err
	}

	s.proxiedCalls.Add(1)
	return proxy.NewStreamParameters(proxy.Destination{
		Ctx:  outCtx,
		Conn: s.upstreamConn,
		Msg:  frame,
	}, nil, nil, nil), nil
}

// outgoingContext returns the context for an upstream call made for the client
// request carrying ctx. All of the client's incoming metadata is forwarded, so
// authorization, correlation IDs and feature flags reach the upstream. If the
// client sent no authorization, the cache's own token is used when one is
// configured. A fill that serves other clients may reuse this context only
// because the request that created it was authorized first, see authorize.
func (s *Server) outgoingContext(ctx context.Context) (context.Context, error) {
	incoming, _ := metadata.FromIncomingContext(ctx)
	outgoing := incoming.Copy()

	if len(outgoing.Get("authorization")) == 0 && s.token != "" {
		md, err := s.tokenMetadata(ctx)
		if err != nil {
			return nil, err
		}
		for key, values := range md {
			outgoing.Set(key, values)
		}
	}

	return metadata.NewOutgoingContext(ctx, outgoing), nil
}

func (s *Server) tokenMetadata(ctx context.Context) (map[string]string, error) {
	md, err := gitalyauth.RPCCredentialsV2(s.token).GetRequestMetadata(ctx)
	if err != nil {
		return nil, structerr.NewInternal("sign upstream request: %v", err)
	}
	return md, nil
}
