package gitcmd

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

func newCommandFactory(tb testing.TB, cfg config.Cfg, opts ...ExecCommandFactoryOption) *ExecCommandFactory {
	gitCmdFactory, cleanup, err := NewExecCommandFactory(cfg, testhelper.SharedLogger(tb), opts...)
	require.NoError(tb, err)
	tb.Cleanup(cleanup)
	return gitCmdFactory
}
