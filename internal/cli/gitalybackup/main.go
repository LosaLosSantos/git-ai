package gitalybackup

import (
	"fmt"

	"github.com/feder-cr/open_scion/internal/version"
	"github.com/urfave/cli/v3"
)

func init() {
	cli.VersionPrinter = func(cmd *cli.Command) {
		fmt.Fprintln(cmd.Writer, version.GetVersionString(binaryName))
	}
}

const (
	progname = "gitaly-backup"

	pathFlagName = "path"
	binaryName   = "Gitaly Backup"
)

// NewApp returns a new gitaly[backup app.
func NewApp() *cli.Command {
	return &cli.Command{
		Name:    progname,
		Usage:   "create gitaly backups",
		Version: version.GetVersionString(binaryName),
		Commands: []*cli.Command{
			newCreateCommand(),
			newRestoreCommand(),
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  pathFlagName,
				Usage: "Directory where the backup files will be created/restored.",
			},
		},
	}
}
