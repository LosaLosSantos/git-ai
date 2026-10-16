// Command graft-verify runs the test command on the tip of a Graft branch after
// lands, moves the green ref on success and reports failures.
//
// Usage:
//
//	graft-verify -config graft-verify.toml          # run until interrupted
//	graft-verify -config graft-verify.toml -once    # verify the pending lands once (CI)
//
// Exit codes: 0 when the tip is green or nothing was pending, 1 on an error,
// 2 when the tests failed on the tip (with -once).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/feder-cr/git-ai/internal/graft/verify"
)

const (
	exitOK     = 0
	exitError  = 1
	exitFailed = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes graft-verify with args and returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("graft-verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path of the TOML configuration file (required)")
	once := flags.Bool("once", false, "verify the changes after the cursor once, print the outcome as JSON and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "graft-verify: -config is required")
		flags.Usage()
		return exitError
	}

	cfg, err := verify.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "graft-verify: %v\n", err)
		return exitError
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	v, closer, err := verify.Open(ctx, cfg, logger)
	if err != nil {
		fmt.Fprintf(stderr, "graft-verify: %v\n", err)
		return exitError
	}
	defer func() { _ = closer.Close() }()

	if *once {
		return runOnce(ctx, v, cfg, stdout, stderr)
	}
	if err := v.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "graft-verify: %v\n", err)
		return exitError
	}
	return exitOK
}

// runOnce verifies the pending changes once and prints the outcome as JSON.
func runOnce(ctx context.Context, v *verify.Verifier, cfg verify.Config, stdout, stderr io.Writer) int {
	out, ran, err := v.RunOnce(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "graft-verify: %v\n", err)
		return exitError
	}
	if !ran {
		out = verify.Outcome{Branch: cfg.Branch, Skipped: "no change on the branch", Green: v.Green(), Cursor: v.Cursor()}
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(stderr, "graft-verify: write outcome: %v\n", err)
		return exitError
	}
	if ran && out.Skipped == "" && !out.Passed {
		return exitFailed
	}
	return exitOK
}
