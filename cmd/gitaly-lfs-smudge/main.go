package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git/smudge"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/tracing"
)

func requireStdin(msg string) {
	var out string

	stat, err := os.Stdin.Stat()
	if err != nil {
		out = fmt.Sprintf("Cannot read from STDIN. %s (%s)", msg, err)
	} else if (stat.Mode() & os.ModeCharDevice) != 0 {
		out = fmt.Sprintf("Cannot read from STDIN. %s", msg)
	}

	if len(out) > 0 {
		fmt.Println(out)
		os.Exit(1)
	}
}

func main() {
	requireStdin("This command should be run by the Git 'smudge' filter")

	ctx, _ := tracing.PropagateFromEnv(os.Environ())

	logger, logCloser, err := command.NewSubprocessLogger(ctx, os.Getenv, "gitaly-lfs-smudge")
	if err != nil {
		fmt.Fprintf(os.Stderr, "new subprocess logger: %q", err)
		os.Exit(1)
	}

	defer func() {
		if err := logCloser.Close(); err != nil {
			fmt.Printf("close log: %q", err)
			os.Exit(1)
		}
	}()

	if err := run(ctx, os.Environ(), os.Stdout, os.Stdin, logger); err != nil {
		logger.WithError(err).Error("gitaly-lfs-smudge failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, environment []string, out io.Writer, in io.Reader, logger log.Logger) error {
	cfg, err := smudge.ConfigFromEnvironment(environment)
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	switch cfg.DriverType {
	case smudge.DriverTypeFilter:
		if err := filter(ctx, cfg, out, in, logger); err != nil {
			return fmt.Errorf("running smudge filter: %w", err)
		}

		return nil
	case smudge.DriverTypeProcess:
		if err := process(ctx, cfg, out, in, logger); err != nil {
			return fmt.Errorf("running smudge process: %w", err)
		}

		return nil
	default:
		return fmt.Errorf("unknown driver type: %v", cfg.DriverType)
	}
}
