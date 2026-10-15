package praefect

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/urfave/cli/v3"
)

func TestMain(m *testing.M) {
	defer func(old func(code int)) { cli.OsExiter = old }(cli.OsExiter)
	cli.OsExiter = func(code int) {}

	defer func(old cli.Flag) { cli.VersionFlag = old }(cli.VersionFlag)
	cli.VersionFlag = stubFlag{}

	defer func(old cli.Flag) { cli.HelpFlag = old }(cli.HelpFlag)
	cli.HelpFlag = stubFlag{}

	testhelper.Run(m)
}
