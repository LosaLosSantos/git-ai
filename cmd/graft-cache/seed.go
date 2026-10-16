package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/feder-cr/git-ai/internal/graft/cachetier"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// snapshotOptions are the flags of "graft-cache snapshot".
type snapshotOptions struct {
	upstream string
	token    string
	storage  string
	repo     string
	rev      string
	out      string
}

// parseSnapshotOptions parses the arguments of "graft-cache snapshot". It is
// separate from runSnapshot so that the flag handling can be tested.
func parseSnapshotOptions(args []string, stderr io.Writer) (snapshotOptions, error) {
	var opts snapshotOptions

	fs := flag.NewFlagSet("graft-cache snapshot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.upstream, "upstream", "", "Gitaly or graft-cache address to read from, e.g. unix:<path> or tcp://<host:port> (required)")
	fs.StringVar(&opts.token, "token", "", "token sent upstream as the caller's credentials")
	fs.StringVar(&opts.storage, "storage", "default", "storage name of the repository")
	fs.StringVar(&opts.repo, "repo", "", "relative path of the repository below the storage (required)")
	fs.StringVar(&opts.rev, "rev", "", "revision whose root tree is snapshotted, e.g. main (required)")
	fs.StringVar(&opts.out, "out", "", "seed file to write (required)")

	if err := fs.Parse(args); err != nil {
		return snapshotOptions{}, err
	}
	if fs.NArg() > 0 {
		return snapshotOptions{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	switch {
	case opts.upstream == "":
		return snapshotOptions{}, errors.New("--upstream is required")
	case opts.repo == "":
		return snapshotOptions{}, errors.New("--repo is required")
	case opts.rev == "":
		return snapshotOptions{}, errors.New("--rev is required")
	case opts.out == "":
		return snapshotOptions{}, errors.New("--out is required")
	case opts.storage == "":
		return snapshotOptions{}, errors.New("--storage must not be empty")
	}
	return opts, nil
}

// runSnapshot implements "graft-cache snapshot". It writes the seed file atomically:
// the data goes to a temporary file in the target directory, which is renamed
// into place only after the snapshot succeeded.
func runSnapshot(args []string) error {
	opts, err := parseSnapshotOptions(args, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	logger, err := log.Configure(os.Stderr, "text", "info")
	if err != nil {
		return fmt.Errorf("configure logger: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	upstream, err := cachetier.DialUpstream(ctx, opts.upstream, logger)
	if err != nil {
		return fmt.Errorf("dial upstream: %w", err)
	}
	defer upstream.Close()

	tmp, err := os.CreateTemp(filepath.Dir(opts.out), ".graft-seed-*")
	if err != nil {
		return fmt.Errorf("create seed file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	repo := &gitalypb.Repository{StorageName: opts.storage, RelativePath: opts.repo}
	report, err := cachetier.WriteSnapshot(ctx, upstream, opts.token, repo, opts.rev, tmp)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("snapshot %s of %s: %w", opts.rev, opts.repo, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close seed file: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("chmod seed file: %w", err)
	}
	if err := os.Rename(tmp.Name(), opts.out); err != nil {
		return fmt.Errorf("publish seed file: %w", err)
	}

	fmt.Fprintf(os.Stdout, "snapshot: %d objects, %d bytes, %d missing -> %s\n",
		report.Objects, report.Bytes, report.Missing, opts.out)
	return nil
}

// splitSeedFiles returns the non-empty paths of a comma-separated --seed value.
func splitSeedFiles(value string) []string {
	var paths []string
	for _, path := range strings.Split(value, ",") {
		if path = strings.TrimSpace(path); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// loadSeedFiles preloads the cache from each seed file, in order, before the tier
// serves requests. A file that cannot be opened or is not a seed file stops the
// start. Damaged records are skipped and reported.
func loadSeedFiles(server *cachetier.Server, paths []string, logger log.Logger) error {
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open seed %s: %w", path, err)
		}
		report, err := server.Seed(f)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("seed %s: %w", path, err)
		}

		entry := logger.WithField("seed", path).
			WithField("objects", report.Objects).
			WithField("bytes", report.Bytes).
			WithField("corrupt", report.Corrupt).
			WithField("skipped", report.Skipped)
		if report.Corrupt > 0 {
			entry.Warn("graft-cache: seed has damaged records, they were skipped")
		} else {
			entry.Info("graft-cache: seeded")
		}
	}
	return nil
}
