package server

import (
	"os"
	"testing"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mdfile"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/internal/version"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

func TestGitalyServerInfo(t *testing.T) {
	cfg := testcfg.Build(t, testcfg.WithStorages("default", "broken"))

	addr := runServer(t, cfg, testserver.WithDisablePraefect())

	require.NoError(t, os.RemoveAll(cfg.Storages[1].Path), "second storage needs to be invalid")

	client := newServerClient(t, addr)
	ctx := testhelper.Context(t)

	require.NoError(t, mdfile.WriteMetadataFile(ctx, cfg.Storages[0].Path))
	metadata, err := mdfile.ReadMetadataFile(cfg.Storages[0].Path)
	require.NoError(t, err)

	c, err := client.ServerInfo(ctx, &gitalypb.ServerInfoRequest{})
	require.NoError(t, err)

	require.Equal(t, version.GetVersion(), c.GetServerVersion())

	require.Len(t, c.GetStorageStatuses(), len(cfg.Storages))
	require.True(t, c.GetStorageStatuses()[0].GetReadable())
	require.True(t, c.GetStorageStatuses()[0].GetWriteable())
	require.NotEmpty(t, c.GetStorageStatuses()[0].GetFsType())
	require.Equal(t, uint32(1), c.GetStorageStatuses()[0].GetReplicationFactor())

	require.False(t, c.GetStorageStatuses()[1].GetReadable())
	require.False(t, c.GetStorageStatuses()[1].GetWriteable())
	require.Equal(t, metadata.GitalyFilesystemID, c.GetStorageStatuses()[0].GetFilesystemId())
	require.Equal(t, uint32(1), c.GetStorageStatuses()[1].GetReplicationFactor())
}

func runServer(t *testing.T, cfg config.Cfg, opts ...testserver.GitalyServerOpt) string {
	return testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterServerServiceServer(srv, NewServer(deps))
	}, opts...)
}

func TestServerNoAuth(t *testing.T) {
	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{Auth: auth.Config{Token: "some"}}))

	addr := runServer(t, cfg)

	conn, err := client.New(testhelper.Context(t), addr)
	require.NoError(t, err)
	t.Cleanup(func() { testhelper.MustClose(t, conn) })
	ctx := testhelper.Context(t)

	client := gitalypb.NewServerServiceClient(conn)
	_, err = client.ServerInfo(ctx, &gitalypb.ServerInfoRequest{})

	testhelper.RequireGrpcCode(t, err, codes.Unauthenticated)
}

func newServerClient(t *testing.T, serverSocketPath string) gitalypb.ServerServiceClient {
	connOpts := []grpc.DialOption{
		grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(testhelper.RepositoryAuthToken)),
	}
	conn, err := client.New(testhelper.Context(t), serverSocketPath, client.WithGrpcOptions(connOpts))
	require.NoError(t, err)
	t.Cleanup(func() { testhelper.MustClose(t, conn) })

	return gitalypb.NewServerServiceClient(conn)
}
