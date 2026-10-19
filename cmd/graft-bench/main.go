// Command graft-bench measures landing throughput and latency of a Graft
// server under concurrent agents. Each agent commits changes on private
// branches and lands them with UserLand on a shared target branch (one by
// default, see --branches). The conflict rate controls how many changes touch
// a shared region of one file. See doc/graft/BENCH.md.
//
// Example:
//
//	graft-bench --address unix:/run/gitaly.sock --repo bench.git \
//	    --agents 50 --changes 10 --conflict-rate 0.2 --mode land
//
// Modes land, land-resolve, land-batch and queue differ in how the changes
// are landed (see Mode). --branches spreads the agents over several branches.
//
// Multi-node runs spread the repositories over several servers, one server per
// address, and report every node as well as the whole run:
//
//	graft-bench --addrs unix:/run/node1.sock,unix:/run/node2.sock,unix:/run/node3.sock \
//	    --repo bench.git --repos 6 --agents 60 --create-repo --token T
//
// Output is a table by default; --format json prints one JSON object. The
// same result can be saved as JSON (--json-out) or as a Markdown report
// (--md-out). Saved JSON results are compared side by side with:
//
//	graft-bench compare --format markdown land.json land-batch.json
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/feder-cr/git-ai/sdk"
)

// options are the parsed command-line options.
type options struct {
	address    string
	token      string
	agentToken string
	storage    string
	repo       string
	createRepo bool
	format     string
	label      string
	jsonOut    string
	mdOut      string
	timeout    time.Duration
	config     Config
	repos      int
	placement  placement
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the benchmark and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "compare" {
		return runCompare(args[1:], stdout, stderr)
	}

	opts, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "graft-bench: %v\n", err)
		return 2
	}

	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	if opts.placement.multi {
		return runMultiNode(ctx, opts, stdout, stderr)
	}

	if opts.createRepo {
		if err := createRepository(ctx, opts.address, opts.token, opts.storage, opts.repo); err != nil {
			fmt.Fprintf(stderr, "graft-bench: create repository %s: %v\n", opts.repo, err)
			return 1
		}
	}

	dialOpts := []sdk.DialOption{sdk.WithStorage(opts.storage)}
	switch {
	case opts.token != "":
		dialOpts = append(dialOpts, sdk.WithToken(opts.token))
	case opts.agentToken != "":
		dialOpts = append(dialOpts, sdk.WithAgentToken(opts.agentToken))
	}

	client, err := sdk.Dial(ctx, opts.address, dialOpts...)
	if err != nil {
		fmt.Fprintf(stderr, "graft-bench: dial %s: %v\n", opts.address, err)
		return 1
	}
	defer func() { _ = client.Close() }()

	res, runErr := Run(ctx, opts.config, NewSDKBackend(client, opts.repo))
	if runErr != nil && res.Changes == 0 {
		fmt.Fprintf(stderr, "graft-bench: %v\n", runErr)
		return 1
	}
	res.Label = opts.label

	// A truncated run still prints what it measured, but it is a failure.
	if err := writeResult(stdout, opts.format, res); err != nil {
		fmt.Fprintf(stderr, "graft-bench: write output: %v\n", err)
		return 1
	}
	if err := writeReportFiles(res, opts.jsonOut, opts.mdOut); err != nil {
		fmt.Fprintf(stderr, "graft-bench: %v\n", err)
		return 1
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "graft-bench: %v\n", runErr)
		return 1
	}

	return 0
}

// parseOptions parses args. Usage goes to stderr.
func parseOptions(args []string, stderr io.Writer) (options, error) {
	var opts options

	fs := flag.NewFlagSet("graft-bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.address, "address", "", "server address: unix:<path>, tcp://<host:port> or tls://<host:port> (required)")
	fs.StringVar(&opts.token, "token", "", "shared token (default $GRAFT_TOKEN)")
	fs.StringVar(&opts.agentToken, "agent-token", "", "agent token (default $GRAFT_AGENT_TOKEN)")
	fs.StringVar(&opts.storage, "storage", sdk.DefaultStorage, "storage name")
	fs.StringVar(&opts.repo, "repo", "", "relative path of the repository (required; with --repos, the base name of the repositories)")
	addrs := fs.String("addrs", "", "comma-separated server addresses, one Gitaly per address (replaces --address; repositories are placed round robin)")
	fs.IntVar(&opts.repos, "repos", 0, "number of repositories, placed round robin over the nodes and worked on round robin by the agents (default: one per address)")
	fs.BoolVar(&opts.createRepo, "create-repo", false, "create the repository first, through the server's RepositoryService (needs --token)")
	fs.StringVar(&opts.config.Branch, "branch", DefaultBranch, "branch the changes land on (with --branches > 1, the prefix of the branch names)")
	fs.IntVar(&opts.config.Branches, "branches", 1, "number of branches the agents are spread over, round robin")
	fs.IntVar(&opts.config.Agents, "agents", DefaultAgents, "concurrent agents (the benchmark targets 50 and 100)")
	fs.IntVar(&opts.config.ChangesPerAgent, "changes", DefaultChangesPerAgent, "changes per agent")
	fs.Float64Var(&opts.config.ConflictRate, "conflict-rate", 0, "fraction of changes that touch the shared region (0 to 1)")
	mode := fs.String("mode", string(ModeLand), "landing mode: land, land-resolve, land-batch or queue")
	fs.DurationVar(&opts.config.TestDuration, "test-duration", 0, "simulated test time per change (queue mode)")
	fs.Int64Var(&opts.config.Seed, "seed", 1, "seed of the change plan")
	maxAttempts := fs.Int("max-attempts", 0, "server-side compare-and-swap attempts per land (0 = server default)")
	fs.StringVar(&opts.format, "format", "table", "output format: table or json")
	fs.StringVar(&opts.label, "label", "", "name of the run in reports, such as batching-on-50 (no | or newlines)")
	fs.StringVar(&opts.jsonOut, "json-out", "", "also write the result as JSON to this file")
	fs.StringVar(&opts.mdOut, "md-out", "", "also write the result as a Markdown report to this file")
	fs.DurationVar(&opts.timeout, "timeout", 10*time.Minute, "overall timeout of the run")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}

	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	token, agentToken, err := resolveTokens(explicit, opts.token, opts.agentToken,
		os.Getenv("GRAFT_TOKEN"), os.Getenv("GRAFT_AGENT_TOKEN"))
	if err != nil {
		return options{}, err
	}
	opts.token, opts.agentToken = token, agentToken

	opts.config.Mode = Mode(*mode)
	opts.config.MaxAttempts = int32(*maxAttempts)

	place, err := resolvePlacement(opts.address, *addrs, opts.repos, opts.config.Agents)
	if err != nil {
		return options{}, err
	}
	opts.placement = place
	if place.multi {
		opts.config.Repos = place.repos
	}

	switch {
	case opts.repo == "":
		return options{}, errors.New("--repo is required")
	case opts.createRepo && opts.token == "":
		return options{}, errors.New("--create-repo needs the shared --token")
	case opts.format != "table" && opts.format != "json":
		return options{}, fmt.Errorf("unknown format %q, want table or json", opts.format)
	case strings.ContainsAny(opts.label, "|\r\n"):
		return options{}, errors.New("--label must not contain | or newlines")
	case opts.timeout <= 0:
		return options{}, errors.New("--timeout must be positive")
	}

	if err := opts.config.Validate(); err != nil {
		return options{}, err
	}

	return opts, nil
}

// resolveTokens picks the credentials of the run. An explicit flag wins over
// its environment variable, so one flag never conflicts with the other
// variable. Without flags, the shared token of $GRAFT_TOKEN is used in
// preference to $GRAFT_AGENT_TOKEN when both are set. Passing both flags is an
// error.
func resolveTokens(explicit map[string]bool, flagToken, flagAgentToken, envToken, envAgentToken string) (string, string, error) {
	tokenSet, agentSet := explicit["token"], explicit["agent-token"]
	switch {
	case tokenSet && agentSet:
		if flagToken != "" && flagAgentToken != "" {
			return "", "", errors.New("--token and --agent-token are mutually exclusive")
		}
		return flagToken, flagAgentToken, nil
	case tokenSet:
		return flagToken, "", nil
	case agentSet:
		return "", flagAgentToken, nil
	case envToken != "":
		return envToken, "", nil
	default:
		return "", envAgentToken, nil
	}
}

// writeResult prints res as a table or as one JSON object.
func writeResult(w io.Writer, format string, res Result) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	rows := [][2]string{
		{"label", res.Label},
		{"mode", string(res.Mode)},
		{"agents", fmt.Sprint(res.Agents)},
		{"branches", fmt.Sprint(res.Branches)},
		{"changes", fmt.Sprintf("%d (%d per agent, conflict rate %.2f)", res.Changes, res.ChangesPerAgent, res.ConflictRate)},
		{"landed", fmt.Sprintf("%d (%d already landed)", res.Landed, res.AlreadyLanded)},
		{"failed", fmt.Sprintf("%d (%d conflicts)", res.Failed, res.Conflicts)},
		{"observed conflicts", fmt.Sprintf("%.3f", res.ObservedConflictRate)},
		{"resolutions", fmt.Sprint(res.Resolutions)},
		{"attempts", fmt.Sprintf("%d over all changes (%.2f per landed change, max %d)", res.Attempts, res.AttemptsPerLand, res.MaxAttempts)},
		{"attempts histogram", formatHistogram(res.AttemptsHistogram) + " (landed changes)"},
		{"failed attempts histogram", formatHistogram(res.FailedAttemptsHistogram) + " (failed changes)"},
		{"unavailable retries", fmt.Sprintf("%d (client retries of Unavailable, landed changes)", res.UnavailableRetries)},
		{"batch size", fmt.Sprintf("mean %.2f, max %d", res.BatchSizeMean, res.BatchSizeMax)},
		{"batch size histogram", formatHistogram(res.BatchSizeHistogram)},
		{"duration", fmt.Sprintf("%.2fs", res.DurationSeconds)},
		{"lands/min", fmt.Sprintf("%.1f", res.LandsPerMinute)},
		{"latency p50", fmt.Sprintf("%.1fms (landed changes)", res.P50MS)},
		{"latency p95", fmt.Sprintf("%.1fms (landed changes)", res.P95MS)},
		{"latency p99", fmt.Sprintf("%.1fms (landed changes)", res.P99MS)},
		{"failed p95", fmt.Sprintf("%.1fms (failed changes)", res.FailedP95MS)},
		{"UserCommitFiles", formatRPCLatency(res.RPCs[rpcCommitFiles])},
		{"UserLand", formatRPCLatency(res.RPCs[rpcLand])},
	}
	for _, row := range rows {
		if row[0] == "label" && row[1] == "" {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\n", row[0], row[1])
	}
	writeNodeRows(tw, res)
	for _, class := range sortedKeys(res.FailureClasses) {
		fmt.Fprintf(tw, "failure\t%d x %s\n", res.FailureClasses[class], class)
	}
	for _, e := range res.Errors {
		fmt.Fprintf(tw, "error\t%s\n", e)
	}

	return tw.Flush()
}

// writeReportFiles saves res as JSON to jsonPath and as a Markdown report to
// mdPath. An empty path writes nothing.
func writeReportFiles(res Result, jsonPath, mdPath string) error {
	if jsonPath != "" {
		if err := writeFile(jsonPath, func(w io.Writer) error {
			return writeResult(w, "json", res)
		}); err != nil {
			return fmt.Errorf("write --json-out: %w", err)
		}
	}
	if mdPath != "" {
		if err := writeFile(mdPath, func(w io.Writer) error {
			return writeMarkdownReport(w, res)
		}); err != nil {
			return fmt.Errorf("write --md-out: %w", err)
		}
	}

	return nil
}

// writeFile creates path and fills it with write.
func writeFile(path string, write func(io.Writer) error) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()

	bw := bufio.NewWriter(f)
	if err := write(bw); err != nil {
		return err
	}

	return bw.Flush()
}
