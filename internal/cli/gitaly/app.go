package gitaly

import (
	"fmt"

	"github.com/feder-cr/open_scion/internal/version"
	"github.com/urfave/cli/v3"
)

func init() {
	// Override the version printer to include Go runtime and OS/arch details.
	cli.VersionPrinter = func(ctx *cli.Command) {
		fmt.Fprintln(ctx.Writer, version.GetVersionDetails("Gitaly"))
	}
}

// NewApp returns a new gitaly app.
func NewApp() *cli.Command {
	return &cli.Command{
		Name:        "gitaly",
		Usage:       "a Git RPC service",
		Description: "Gitaly is a Git RPC service for handling Git calls.",
		Version:     version.GetVersionString("Gitaly"),
		// serveAction is also here in the root to keep the CLI backwards compatible
		// with the previous way to launch Gitaly with just `gitaly <configfile>`. We
		// may want to deprecate this eventually.
		Action: serveAction,
		Commands: []*cli.Command{
			newServeCommand(),
			newCheckCommand(),
			newConfigurationCommand(),
			newHooksCommand(),
			newBundleURICommand(),
			newGitCommand(),
			newPoolCommand(),
		},
	}
}
