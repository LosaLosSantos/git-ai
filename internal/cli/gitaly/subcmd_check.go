package gitaly

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/feder-cr/git-ai"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/config/prometheus"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/urfave/cli/v3"
)

func newCheckCommand() *cli.Command {
	return &cli.Command{
		Name:  "check",
		Usage: "verify internal API is accessible",
		UsageText: `gitaly check <gitaly_config_file>

Example: gitaly check gitaly.config.toml`,
		Description: "Check that the internal Gitaly API is accessible.",
		ArgsUsage:   "<configfile>",
		Action:      checkAction,
	}
}

func checkAction(ctx context.Context, cmd *cli.Command) (returnedErr error) {
	logger := log.ConfigureCommand()

	if cmd.NArg() != 1 || cmd.Args().First() == "" {
		if err := cli.ShowSubcommandHelp(cmd); err != nil {
			return err
		}

		return cli.Exit("error: invalid argument(s)", 2)
	}

	configPath := cmd.Args().First()
	cfg, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("loading configuration %q: %w", configPath, err)
	}

	if cfg.RuntimeDir != "" {
		if err := config.PruneOldGitalyProcessDirectories(logger, cfg.RuntimeDir); err != nil {
			return fmt.Errorf("prune runtime directories: %w", err)
		}
	}

	cfg, err = config.SetupRuntimeDirectory(cfg, os.Getpid())
	if err != nil {
		return fmt.Errorf("setup runtime directory: %w", err)
	}

	defer func() {
		if rmErr := os.RemoveAll(cfg.RuntimeDir); rmErr != nil {
			returnedErr = errors.Join(err, rmErr)
		}
	}()

	// Since this subcommand invokes a Git command, we need to unpack the bundled Git binaries
	// from the Gitaly binary.
	if err := gitaly.UnpackAuxiliaryBinaries(cfg.RuntimeDir, func(string) bool {
		return true
	}); err != nil {
		return fmt.Errorf("unpack auxiliary binaries: %w", err)
	}

	fmt.Fprint(cmd.Writer, "Checking GitLab API access: ")
	info, err := checkAPI(cfg, logger)
	if err != nil {
		fmt.Fprintln(cmd.Writer, "FAILED")
		return err
	}

	fmt.Fprintln(cmd.Writer, "OK")
	fmt.Fprintf(cmd.Writer, "GitLab version: %s\n", info.Version)
	fmt.Fprintf(cmd.Writer, "GitLab revision: %s\n", info.Revision)
	fmt.Fprintf(cmd.Writer, "GitLab Api version: %s\n", info.APIVersion)
	fmt.Fprintf(cmd.Writer, "Redis reachable for GitLab: %t\n", info.RedisReachable)
	fmt.Fprintln(cmd.Writer, "OK")

	return returnedErr
}

func checkAPI(cfg config.Cfg, logger log.Logger) (*gitlab.CheckInfo, error) {
	var gitlabAPI gitlab.Client
	if cfg.Graft.Standalone {
		// Graft standalone mode never contacts GitLab: hooks are answered by the
		// built-in policy client, so that is the client to check.
		gitlabAPI = gitlab.NewStandaloneClient(cfg.Graft, logger)
	} else {
		httpClient, err := gitlab.NewHTTPClient(logger, cfg.Gitlab, cfg.TLS, prometheus.Config{})
		if err != nil {
			return nil, err
		}
		gitlabAPI = httpClient
	}

	gitCmdFactory, cleanup, err := gitcmd.NewExecCommandFactory(cfg, logger)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return hook.NewManager(
		cfg,
		config.NewLocator(cfg),
		logger,
		gitCmdFactory,
		nil,
		nil,
		gitlabAPI,
		hook.NewPostReceiveRegistry(),
	).Check(context.Background())
}
