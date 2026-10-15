package hook

import (
	"context"
	"crypto/sha1"
	"fmt"
	"net"
	"testing"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/grpc/backchannel"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/internal/transaction/txinfo"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type testTransactionServer struct {
	gitalypb.UnimplementedRefTransactionServer
	handler func(in *gitalypb.VoteTransactionRequest) (*gitalypb.VoteTransactionResponse, error)
}

func (s *testTransactionServer) VoteTransaction(ctx context.Context, in *gitalypb.VoteTransactionRequest) (*gitalypb.VoteTransactionResponse, error) {
	if s.handler != nil {
		return s.handler(in)
	}
	return nil, nil
}

func TestReferenceTransactionHookInvalidArgument(t *testing.T) {
	cfg := testcfg.Build(t)
	serverSocketPath := runHooksServer(t, cfg, nil)

	client, conn := newHooksClient(t, serverSocketPath)
	defer conn.Close()
	ctx := testhelper.Context(t)

	stream, err := client.ReferenceTransactionHook(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&gitalypb.ReferenceTransactionHookRequest{}))
	_, err = stream.Recv()

	testhelper.RequireGrpcError(t, structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet), err)
}

func TestReferenceTransactionHook(t *testing.T) {
	ctx := testhelper.Context(t)

	stdin := []byte(fmt.Sprintf(
		`%[1]s %[2]s refs/heads/branch-1
%[2]s %[1]s refs/heads/branch-2
%[1]s %[2]s HEAD
`,
		gittest.DefaultObjectHash.ZeroOID,
		gittest.DefaultObjectHash.EmptyTreeOID,
	))

	testCases := []struct {
		desc              string
		stdin             []byte
		state             gitalypb.ReferenceTransactionHookRequest_State
		voteResponse      gitalypb.VoteTransactionResponse_TransactionState
		expectedErr       error
		expectedResponse  *gitalypb.ReferenceTransactionHookResponse
		expectedReftxHash []byte
	}{
		{
			desc:         "hook triggers transaction with default state",
			stdin:        stdin,
			voteResponse: gitalypb.VoteTransactionResponse_COMMIT,
			expectedResponse: &gitalypb.ReferenceTransactionHookResponse{
				ExitStatus: &gitalypb.ExitStatus{
					Value: 0,
				},
			},
			expectedReftxHash: stdin,
		},
		{
			desc:         "hook triggers transaction with explicit prepared state",
			stdin:        stdin,
			state:        gitalypb.ReferenceTransactionHookRequest_PREPARED,
			voteResponse: gitalypb.VoteTransactionResponse_COMMIT,
			expectedResponse: &gitalypb.ReferenceTransactionHookResponse{
				ExitStatus: &gitalypb.ExitStatus{
					Value: 0,
				},
			},
			expectedReftxHash: stdin,
		},
		{
			desc:  "hook does not trigger transaction with aborted state",
			stdin: stdin,
			state: gitalypb.ReferenceTransactionHookRequest_ABORTED,
			expectedResponse: &gitalypb.ReferenceTransactionHookResponse{
				ExitStatus: &gitalypb.ExitStatus{
					Value: 0,
				},
			},
		},
		{
			desc:  "hook triggers transaction with committed state",
			stdin: stdin,
			state: gitalypb.ReferenceTransactionHookRequest_COMMITTED,
			expectedResponse: &gitalypb.ReferenceTransactionHookResponse{
				ExitStatus: &gitalypb.ExitStatus{
					Value: 0,
				},
			},
			expectedReftxHash: stdin,
		},
		{
			desc: "hook triggers transaction with default branch update",
			stdin: []byte(fmt.Sprintf(
				`%[1]s %[2]s refs/heads/branch-1
%[2]s %[1]s refs/heads/branch-2
ref:refs/heads/main ref:refs/heads/branch-1 HEAD
`,
				gittest.DefaultObjectHash.ZeroOID,
				gittest.DefaultObjectHash.EmptyTreeOID,
			)),
			state: gitalypb.ReferenceTransactionHookRequest_COMMITTED,
			expectedResponse: &gitalypb.ReferenceTransactionHookResponse{
				ExitStatus: &gitalypb.ExitStatus{
					Value: 0,
				},
			},
			expectedReftxHash: stdin,
		},
		{
			desc:         "hook fails with failed vote",
			stdin:        stdin,
			voteResponse: gitalypb.VoteTransactionResponse_ABORT,
			// This gets intercepted by the Aborted interceptor which replaces the error message
			// "reference-transaction hook: error voting on transaction: transaction was aborted"
			expectedErr: testhelper.ToInterceptedMetadata(
				structerr.NewAborted("The operation could not be completed. Please try again.").WithMetadata(
					"error_details", "reference-transaction hook: error voting on transaction: transaction was aborted"),
			),
			expectedReftxHash: stdin,
		},
		{
			desc:              "hook fails with stopped vote",
			stdin:             stdin,
			voteResponse:      gitalypb.VoteTransactionResponse_STOP,
			expectedErr:       structerr.NewFailedPrecondition("reference-transaction hook: error voting on transaction: transaction was stopped"),
			expectedReftxHash: stdin,
		},
	}

	transactionServer := &testTransactionServer{}
	grpcServer := grpc.NewServer()
	gitalypb.RegisterRefTransactionServer(grpcServer, transactionServer)

	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	backchannelConn, err := client.New(testhelper.Context(t), fmt.Sprintf("tcp://%s", listener.Addr().String()))
	require.NoError(t, err)
	defer backchannelConn.Close()

	registry := backchannel.NewRegistry()
	backchannelID := registry.RegisterBackchannel(backchannelConn)

	errQ := make(chan error)
	go func() {
		errQ <- grpcServer.Serve(listener)
	}()
	defer func() {
		grpcServer.Stop()
		require.NoError(t, <-errQ)
	}()

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			cfg := testcfg.Build(t)

			var reftxHash []byte
			transactionServer.handler = func(in *gitalypb.VoteTransactionRequest) (*gitalypb.VoteTransactionResponse, error) {
				reftxHash = in.GetReferenceUpdatesHash()
				return &gitalypb.VoteTransactionResponse{
					State: tc.voteResponse,
				}, nil
			}

			cfg.SocketPath = runHooksServer(t, cfg, nil, testserver.WithBackchannelRegistry(registry))
			ctx := testhelper.Context(t)

			repo, _ := gittest.CreateRepository(t, ctx, cfg)

			hooksPayload, err := gitcmd.NewHooksPayload(
				ctx,
				cfg,
				repo,
				gittest.DefaultObjectHash,
				&txinfo.Transaction{
					BackchannelID: backchannelID,
					ID:            1234,
					Node:          "node-1",
				},
				nil,
				gitcmd.ReferenceTransactionHook,
				featureflag.FromContext(ctx),
			).Env()
			require.NoError(t, err)

			environment := []string{
				hooksPayload,
			}

			client, conn := newHooksClient(t, cfg.SocketPath)
			defer conn.Close()

			stream, err := client.ReferenceTransactionHook(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.ReferenceTransactionHookRequest{
				Repository:           repo,
				State:                tc.state,
				EnvironmentVariables: environment,
			}))
			require.NoError(t, stream.Send(&gitalypb.ReferenceTransactionHookRequest{
				Stdin: tc.stdin,
			}))
			require.NoError(t, stream.CloseSend())

			resp, err := stream.Recv()
			testhelper.RequireGrpcError(t, tc.expectedErr, err)
			testhelper.ProtoEqual(t, tc.expectedResponse, resp)

			var expectedReftxHash []byte
			if tc.expectedReftxHash != nil {
				hash := sha1.Sum(tc.stdin)
				expectedReftxHash = hash[:]
			}
			require.Equal(t, expectedReftxHash[:], reftxHash)
		})
	}
}
