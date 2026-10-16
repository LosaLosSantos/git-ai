// Command graft-superfan follows the changeset log of a Graft server and sends
// digests of the changesets per component to notifiers (stdout, webhook,
// Slack). It reads events from `graft watch --json` on standard input, from a
// file, or from a command it runs. See doc/graft/SUPERFAN.md for the
// configuration and the formats.
//
// Usage:
//
//	graft-superfan --config superfan.json --events -
//	graft-superfan --config superfan.json --watch-cmd "graft watch --json --after {after}" --details-cmd "graft show --json {oid}"
//
// Exit status: 0 on success or interrupt, 1 on a failure, 2 on a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/superfan"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// defaultTick is how often an idle stream is checked for elapsed windows.
const defaultTick = 30 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run executes one command line and returns the exit status.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "chat" {
		return runChat(ctx, args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("graft-superfan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "superfan configuration (JSON)")
	events := fs.String("events", "-", `event source: "-" for standard input, or a file of JSON lines`)
	watchCmd := fs.String("watch-cmd", "", `command that prints events, split on spaces (no shell); "{after}" is replaced by the cursor`)
	detailsCmd := fs.String("details-cmd", "", `command that prints the details of a commit, split on spaces; "{oid}" is replaced by the commit`)
	cursorPath := fs.String("cursor", "", "cursor file (overrides the configuration)")
	tick := fs.Duration("tick", defaultTick, "how often to close elapsed windows on an idle stream; 0 disables it (use 0 for replays)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "graft-superfan: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return exitUsage
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "graft-superfan: --config is required")
		fs.Usage()
		return exitUsage
	}
	if *tick < 0 {
		fmt.Fprintln(stderr, "graft-superfan: --tick must not be negative")
		return exitUsage
	}
	eventsSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "events" {
			eventsSet = true
		}
	})
	if *watchCmd != "" && eventsSet {
		// One source only: an explicit --events would otherwise be ignored.
		fmt.Fprintln(stderr, "graft-superfan: --events and --watch-cmd are mutually exclusive")
		return exitUsage
	}

	data, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "graft-superfan: read config: %v\n", err)
		return exitError
	}
	cfg, err := superfan.ParseConfig(data)
	if err != nil {
		fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
		return exitError
	}

	path := *cursorPath
	if path == "" {
		path = cfg.Cursor
	}
	var store *superfan.CursorStore
	var after uint64
	if path != "" {
		store = superfan.NewCursorStore(path)
		if after, err = store.Load(); err != nil {
			fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
			return exitError
		}
	}

	src, closeSrc, err := openSource(ctx, *watchCmd, *events, after, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
		return exitError
	}
	defer closeSrc()

	opts := superfan.Options{
		Config:       cfg,
		Source:       src,
		Stdout:       stdout,
		Cursor:       store,
		After:        after,
		TickInterval: *tick,
		Logger:       slog.New(slog.NewTextHandler(stderr, nil)),
	}
	if *detailsCmd != "" {
		opts.Details = superfan.CommandDetails{Argv: strings.Fields(*detailsCmd)}
	}
	sf, err := superfan.New(opts)
	if err != nil {
		fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
		return exitError
	}
	if err := sf.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
		return exitError
	}
	return exitOK
}

// openSource returns the event source and the function that releases it.
func openSource(ctx context.Context, watchCmd, events string, after uint64, stdin io.Reader) (superfan.EventSource, func(), error) {
	if watchCmd != "" {
		argv := strings.Fields(strings.ReplaceAll(watchCmd, "{after}", strconv.FormatUint(after, 10)))
		src, err := superfan.StartCommand(ctx, argv)
		if err != nil {
			return nil, nil, err
		}
		return src, func() { _ = src.Close() }, nil
	}
	if events == "-" {
		return superfan.NewReaderSource(stdin, nil), func() {}, nil
	}
	f, err := os.Open(events)
	if err != nil {
		return nil, nil, fmt.Errorf("open events: %w", err)
	}
	return superfan.NewReaderSource(f, f), func() { _ = f.Close() }, nil
}
