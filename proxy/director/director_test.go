package director_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	gitalyregistry "github.com/feder-cr/open_scion/internal/grpc/protoregistry"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/proxy"
	"github.com/feder-cr/open_scion/proxy/director"
	"github.com/feder-cr/open_scion/proxy/grant"
	"github.com/feder-cr/open_scion/proxy/policy"
	policytestdata "github.com/feder-cr/open_scion/proxy/policy/testdata"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

type blobServer struct {
	gitalypb.UnimplementedBlobServiceServer
	t *testing.T
}

func (server blobServer) ListBlobs(request *gitalypb.ListBlobsRequest, stream grpc.ServerStreamingServer[gitalypb.ListBlobsResponse]) error {
	require.Equal(server.t, "default", request.GetRepository().GetStorageName())
	incoming, ok := metadata.FromIncomingContext(stream.Context())
	require.True(server.t, ok)
	require.Equal(server.t, []string{"gkg-indexer"}, incoming.Get("client_name"))
	require.Equal(server.t, []string{"validated-correlation-id"}, incoming.Get("x-gitlab-correlation-id"))
	require.Empty(server.t, incoming.Get("untrusted"))
	return stream.Send(&gitalypb.ListBlobsResponse{Blobs: []*gitalypb.ListBlobsResponse_Blob{{Data: []byte("proxied")}}})
}

func TestRoundTrip(t *testing.T) {
	backendListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	backendServer := grpc.NewServer()
	gitalypb.RegisterBlobServiceServer(backendServer, blobServer{t: t})
	go func() { require.NoError(t, backendServer.Serve(backendListener)) }()
	t.Cleanup(func() {
		backendServer.Stop()
		_ = backendListener.Close()
	})

	backendConnection, err := grpc.NewClient(backendListener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backendConnection.Close()) })

	resolvedGrant := &grant.Grant{
		Coordinates: grant.Coordinates{
			Address: backendListener.Addr().String(),
			CallMetadata: map[string]string{
				"client_name":             "gkg-indexer",
				"x-gitlab-correlation-id": "grant-correlation-id",
			},
		},
		Authorization: grant.Authorization{
			Profile:        "readonly_repository",
			Repository:     &gitalypb.Repository{StorageName: "default", RelativePath: "project.git"},
			AllowedMethods: []string{"/gitaly.BlobService/ListBlobs"},
		},
	}

	streamDirector := director.New(
		director.DialFunc(func(_ context.Context, coordinates grant.Coordinates) (*grpc.ClientConn, error) {
			require.Equal(t, backendListener.Addr().String(), coordinates.Address)
			return backendConnection, nil
		}),
		director.GrantResolverFunc(func(context.Context) (*grant.Grant, error) { return resolvedGrant, nil }),
		director.WithCorrelationIDProvider(director.CorrelationIDProviderFunc(func(ctx context.Context) string {
			incoming, ok := metadata.FromIncomingContext(ctx)
			require.True(t, ok)
			require.Equal(t, []string{"untrusted-correlation-id"}, incoming.Get("x-gitlab-correlation-id"))
			return "validated-correlation-id"
		})),
	)
	proxyServer := grpc.NewServer(
		grpc.ForceServerCodec(proxy.NewCodec()),
		grpc.UnknownServiceHandler(proxy.TransparentHandler(streamDirector)),
	)
	proxyListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { require.NoError(t, proxyServer.Serve(proxyListener)) }()
	t.Cleanup(func() {
		proxyServer.Stop()
		_ = proxyListener.Close()
	})

	proxyConnection, err := grpc.NewClient(proxyListener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, proxyConnection.Close()) })

	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(
		"untrusted", "drop-me",
		"x-gitlab-correlation-id", "untrusted-correlation-id",
	))
	stream, err := gitalypb.NewBlobServiceClient(proxyConnection).ListBlobs(ctx, &gitalypb.ListBlobsRequest{
		Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "project.git"},
	})
	require.NoError(t, err)
	response, err := stream.Recv()
	require.NoError(t, err)
	require.True(t, proto.Equal(&gitalypb.ListBlobsResponse{Blobs: []*gitalypb.ListBlobsResponse_Blob{{Data: []byte("proxied")}}}, response))
	_, err = stream.Recv()
	require.ErrorIs(t, err, io.EOF)
}

type peeker struct {
	frame []byte
	err   error
}

func (p peeker) Peek() ([]byte, error) { return p.frame, p.err }

func readonlyGrant(method string) *grant.Grant {
	return &grant.Grant{Authorization: grant.Authorization{
		Profile:        "readonly_repository",
		Repository:     &gitalypb.Repository{StorageName: "default", RelativePath: "project.git"},
		AllowedMethods: []string{method},
	}}
}

func noDialDirector(t *testing.T, resolvedGrant *grant.Grant) (proxy.StreamDirector, *int) {
	t.Helper()
	dials := 0
	streamDirector := director.New(
		director.DialFunc(func(context.Context, grant.Coordinates) (*grpc.ClientConn, error) {
			dials++
			return nil, errors.New("unexpected dial")
		}),
		director.GrantResolverFunc(func(context.Context) (*grant.Grant, error) { return resolvedGrant, nil }),
	)
	return streamDirector, &dials
}

func TestDescriptorDenialsHappenBeforeDial(t *testing.T) {
	resolvedGrant := readonlyGrant("/gitaly.RepositoryService/RepositoryExists")
	streamDirector, dials := noDialDirector(t, resolvedGrant)

	for _, test := range []struct{ name, method, reason string }{
		{name: "profile", method: "/gitaly.CommitService/CommitLanguages", reason: "proxy: not_accessor"},
		{name: "allowlist", method: "/gitaly.RepositoryService/GetArchive", reason: "proxy: method_not_allowed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := streamDirector(t.Context(), test.method, peeker{})
			require.Equal(t, codes.PermissionDenied, status.Code(err))
			require.Equal(t, test.reason, status.Convert(err).Message())
			require.Zero(t, *dials)
		})
	}
}

func TestRequestDenialsHappenBeforeDial(t *testing.T) {
	const fullMethod = "/gitaly.BlobService/ListBlobs"

	requestFrame := func(t *testing.T, repository *gitalypb.Repository) []byte {
		t.Helper()
		frame, err := proto.Marshal(&gitalypb.ListBlobsRequest{Repository: repository})
		require.NoError(t, err)
		return frame
	}

	for _, test := range []struct {
		name, reason string
		frame        func(*testing.T) []byte
	}{
		{name: "malformed request", reason: "proxy: malformed_request", frame: func(*testing.T) []byte { return []byte{0xff} }},
		{name: "unpopulated repository", reason: "proxy: no_target_repository", frame: func(t *testing.T) []byte {
			return requestFrame(t, nil)
		}},
		{name: "repository mismatch", reason: "proxy: repo_scope", frame: func(t *testing.T) []byte {
			return requestFrame(t, &gitalypb.Repository{StorageName: "other", RelativePath: "project.git"})
		}},
		{name: "object directory", reason: "proxy: object_directory", frame: func(t *testing.T) []byte {
			return requestFrame(t, &gitalypb.Repository{StorageName: "default", RelativePath: "project.git", GitObjectDirectory: "objects"})
		}},
		{name: "alternate object directories", reason: "proxy: object_directory", frame: func(t *testing.T) []byte {
			return requestFrame(t, &gitalypb.Repository{StorageName: "default", RelativePath: "project.git", GitAlternateObjectDirectories: []string{"alternate"}})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			streamDirector, dials := noDialDirector(t, readonlyGrant(fullMethod))
			_, err := streamDirector(t.Context(), fullMethod, peeker{frame: test.frame(t)})
			require.Equal(t, codes.PermissionDenied, status.Code(err))
			require.Equal(t, test.reason, status.Convert(err).Message())
			require.Zero(t, *dials)
		})
	}
}

func TestAmbiguousTargetDenialHappensBeforeDial(t *testing.T) {
	const fullMethod = "/gitalyproxytest.TestService/TestMethod"
	originalRegistry := gitalyregistry.GitalyProtoPreregistered
	t.Cleanup(func() { gitalyregistry.GitalyProtoPreregistered = originalRegistry })

	var descriptors []*descriptorpb.FileDescriptorProto
	for _, path := range gitalypb.GitalyProtos {
		file, err := protoregistry.GlobalFiles.FindFileByPath(path)
		require.NoError(t, err)
		descriptors = append(descriptors, protodesc.ToFileDescriptorProto(file))
	}
	testFile := protodesc.ToFileDescriptorProto(policytestdata.File_proxy_policy_testdata_ambiguous_proto)
	options := &descriptorpb.MethodOptions{}
	proto.SetExtension(options, gitalypb.E_OpType, &gitalypb.OperationMsg{
		Op:         gitalypb.OperationMsg_ACCESSOR,
		ScopeLevel: gitalypb.OperationMsg_REPOSITORY,
	})
	testFile.Service = append(testFile.Service, &descriptorpb.ServiceDescriptorProto{
		Name: proto.String("TestService"),
		Method: []*descriptorpb.MethodDescriptorProto{{
			Name:       proto.String("TestMethod"),
			InputType:  proto.String(".gitalyproxytest.AmbiguousRequest"),
			OutputType: proto.String(".gitalyproxytest.AmbiguousRequest"),
			Options:    options,
		}},
	})
	descriptors = append(descriptors, testFile)
	registry, err := gitalyregistry.New(descriptors...)
	require.NoError(t, err)
	gitalyregistry.GitalyProtoPreregistered = registry

	streamDirector, dials := noDialDirector(t, readonlyGrant(fullMethod))
	_, err = streamDirector(t.Context(), fullMethod, peeker{frame: []byte("not reached")})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Equal(t, "proxy: ambiguous_target_repository", status.Convert(err).Message())
	require.Zero(t, *dials)
}

func TestTransportErrorsHideCauseFromStatus(t *testing.T) {
	const (
		fullMethod     = "/gitaly.BlobService/ListBlobs"
		internalTarget = "tcp://praefect.internal:2305"
	)
	frame, err := proto.Marshal(&gitalypb.ListBlobsRequest{Repository: &gitalypb.Repository{
		StorageName:  "default",
		RelativePath: "project.git",
	}})
	require.NoError(t, err)

	for _, test := range []struct {
		name, reason string
		code         codes.Code
		peekError    bool
	}{
		{name: "peek", code: codes.Internal, reason: "proxy: peek_failed", peekError: true},
		{name: "dial", code: codes.Unavailable, reason: "proxy: gitaly_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cause := errors.New("connect " + internalTarget)
			dials := 0
			streamDirector := director.New(
				director.DialFunc(func(context.Context, grant.Coordinates) (*grpc.ClientConn, error) {
					dials++
					return nil, cause
				}),
				director.GrantResolverFunc(func(context.Context) (*grant.Grant, error) {
					return readonlyGrant(fullMethod), nil
				}),
			)
			streamPeeker := peeker{frame: frame}
			if test.peekError {
				streamPeeker = peeker{err: cause}
			}

			_, err := streamDirector(t.Context(), fullMethod, streamPeeker)
			require.ErrorIs(t, err, cause)
			require.Equal(t, test.code, status.Code(err))
			require.Equal(t, test.reason, status.Convert(err).Message())
			require.NotContains(t, status.Convert(err).Message(), internalTarget)
			if test.peekError {
				require.Zero(t, dials)
			} else {
				require.Equal(t, 1, dials)
			}
		})
	}
}

func TestInvalidCorrelationIDIsOmitted(t *testing.T) {
	const fullMethod = "/gitaly.BlobService/ListBlobs"
	frame, err := proto.Marshal(&gitalypb.ListBlobsRequest{Repository: &gitalypb.Repository{
		StorageName:  "default",
		RelativePath: "project.git",
	}})
	require.NoError(t, err)

	for _, invalidCorrelationID := range []string{"contains spaces", strings.Repeat("a", 129)} {
		t.Run(invalidCorrelationID, func(t *testing.T) {
			cause := errors.New("stop after metadata assertion")
			resolvedGrant := readonlyGrant(fullMethod)
			resolvedGrant.Coordinates.CallMetadata = map[string]string{
				"x-gitlab-correlation-id": "grant-value",
			}
			streamDirector := director.New(
				director.DialFunc(func(ctx context.Context, _ grant.Coordinates) (*grpc.ClientConn, error) {
					outgoing, ok := metadata.FromOutgoingContext(ctx)
					require.True(t, ok)
					require.Empty(t, outgoing.Get("x-gitlab-correlation-id"))
					return nil, cause
				}),
				director.GrantResolverFunc(func(context.Context) (*grant.Grant, error) { return resolvedGrant, nil }),
				director.WithCorrelationIDProvider(director.CorrelationIDProviderFunc(func(context.Context) string {
					return invalidCorrelationID
				})),
			)

			_, err := streamDirector(t.Context(), fullMethod, peeker{frame: frame})
			require.ErrorIs(t, err, cause)
		})
	}
}

func TestNoGrantUsesPolicyDenial(t *testing.T) {
	streamDirector := director.New(
		director.DialFunc(func(context.Context, grant.Coordinates) (*grpc.ClientConn, error) {
			t.Fatal("dial must not be called")
			return nil, nil
		}),
		director.GrantResolverFunc(func(context.Context) (*grant.Grant, error) { return nil, nil }),
	)

	_, err := streamDirector(t.Context(), "/gitaly.BlobService/ListBlobs", peeker{})
	var denial *policy.InvariantError
	require.ErrorAs(t, err, &denial)
	require.Equal(t, "no_grant", denial.Reason())
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}
