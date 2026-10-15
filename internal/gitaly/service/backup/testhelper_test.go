package backup_test

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service/setup"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

func setupBackupService(tb testing.TB, opt ...testserver.GitalyServerOpt) (config.Cfg, gitalypb.BackupServiceClient) {
	cfg := testcfg.Build(tb)

	addr := testserver.RunGitalyServer(tb, cfg, setup.RegisterAll, opt...)
	cfg.SocketPath = addr

	cc, err := client.New(testhelper.Context(tb), cfg.SocketPath)
	require.NoError(tb, err)
	tb.Cleanup(func() { testhelper.MustClose(tb, cc) })

	return cfg, gitalypb.NewBackupServiceClient(cc)
}
