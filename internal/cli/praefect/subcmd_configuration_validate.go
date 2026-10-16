package praefect

import (
	"context"
	"io"

	"github.com/feder-cr/git-ai/cmd"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/praefect/config"
	"github.com/urfave/cli/v3"
)

const validationErrorCode = 2

func newConfigurationValidateCommand() *cli.Command {
	return &cli.Command{
		Name:  "validate",
		Usage: "validates configuration",
		Description: `Validate Praefect configuration.

Applies validation rules to Praefect configuration provided on stdin and returns validation
errors in JSON format on stdout.

Example: praefect configuration validate < praefect.config.toml`,
		Action: configurationValidateAction,
	}
}

func configurationValidateAction(ctx context.Context, cmd *cli.Command) error {
	log.ConfigureCommand()

	if cmd.Args().Present() {
		_ = cli.ShowSubcommandHelp(cmd)
		return cli.Exit("invalid argument(s)", 1)
	}

	if code := validateConfiguration(cmd.Reader, cmd.Writer, cmd.ErrWriter); code != 0 {
		return cli.Exit("", code)
	}

	return nil
}

// validateConfiguration checks if provided configuration is valid.
func validateConfiguration(reader io.Reader, outWriter, errWriter io.Writer) int {
	cfg, err := config.FromReader(reader)
	if err != nil {
		if cmd.WriteTomlReadError(err, outWriter, errWriter) {
			return validationErrorCode
		}
		return 1
	}

	if !cmd.Validate(&cfg, outWriter, errWriter) {
		return validationErrorCode
	}

	return 0
}
