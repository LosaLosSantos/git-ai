package gittest

import (
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// NewCommandFactory creates a new Git command factory.
func NewCommandFactory(tb testing.TB, cfg config.Cfg, opts ...gitcmd.ExecCommandFactoryOption) *gitcmd.ExecCommandFactory {
	tb.Helper()
	factory, cleanup, err := gitcmd.NewExecCommandFactory(cfg, testhelper.SharedLogger(tb), opts...)
	if err != nil {
		if strings.Contains(err.Error(), "no such file or directory") {
			require.FailNowf(tb, "Gitaly tests execute against the bundled Git environment. Have you run `make build-bundled-git`? Error: %s", err.Error())
		}
	}
	require.NoError(tb, err)
	tb.Cleanup(cleanup)
	return factory
}
