package main

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/feder-cr/git-ai/sdk"
)

// runMultiNode runs the benchmark over the repositories of a multi-node
// placement. The output is the one of a single-node run, plus the placement
// and the result of every node.
func runMultiNode(ctx context.Context, opts options, stdout, stderr io.Writer) int {
	top := Topology{Addrs: opts.placement.addrs, Repos: opts.placement.repos}

	clients := make([]*sdk.Client, len(top.Addrs))
	defer closeClients(clients)
	for node, addr := range top.Addrs {
		client, err := sdk.Dial(ctx, addr, dialOptions(opts)...)
		if err != nil {
			fmt.Fprintf(stderr, "graft-bench: dial %s: %v\n", addr, err)
			return 1
		}
		clients[node] = client
	}

	repos := make([]Backend, top.Repos)
	for repo := range repos {
		node := top.NodeOfRepo(repo)
		path := top.RepoPath(opts.repo, repo)
		if opts.createRepo {
			if err := createRepository(ctx, top.Addrs[node], opts.token, opts.storage, path); err != nil {
				fmt.Fprintf(stderr, "graft-bench: create repository %s on %s: %v\n", path, top.Addrs[node], err)
				return 1
			}
		}
		repos[repo] = NewSDKBackend(clients[node], path)
	}

	cfg := opts.config
	ex, err := execute(ctx, cfg, NewMultiBackend(repos))
	if err != nil {
		fmt.Fprintf(stderr, "graft-bench: %v\n", err)
		return 1
	}

	res, runErr := ex.summarize(ctx, cfg)
	if runErr != nil && res.Changes == 0 {
		fmt.Fprintf(stderr, "graft-bench: %v\n", runErr)
		return 1
	}
	res.Label = opts.label
	res.Repos = top.Repos
	res.Nodes = aggregateNodes(cfg, top, ex.samples, ex.elapsed)

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

// dialOptions returns the options that dial a server with the credentials and
// storage of opts.
func dialOptions(opts options) []sdk.DialOption {
	dialOpts := []sdk.DialOption{sdk.WithStorage(opts.storage)}
	switch {
	case opts.token != "":
		dialOpts = append(dialOpts, sdk.WithToken(opts.token))
	case opts.agentToken != "":
		dialOpts = append(dialOpts, sdk.WithAgentToken(opts.agentToken))
	}

	return dialOpts
}

// closeClients closes the clients that were dialed. Nil entries are skipped.
func closeClients(clients []*sdk.Client) {
	for _, client := range clients {
		if client != nil {
			_ = client.Close()
		}
	}
}

// MultiBackend runs the benchmark over several repositories, each with its own
// backend. Agent a commits and lands in repository a modulo the number of
// repositories, so the agents of one repository work on that repository only.
//
// Prepare prepares the branch on every repository and remembers the tip of
// each. Commit uses the tip of the agent's repository, so the base that the run
// passes to Commit is not used.
type MultiBackend struct {
	repos []Backend

	mu sync.Mutex
	// bases holds the tip of each branch of each repository, from Prepare.
	bases map[baseKey]string
}

// baseKey identifies the tip of a branch of a repository.
type baseKey struct {
	branch string
	repo   int
}

// NewMultiBackend returns a backend over repos. The backend of repository r
// serves the agents a with a modulo len(repos) equal to r.
func NewMultiBackend(repos []Backend) *MultiBackend {
	return &MultiBackend{repos: repos, bases: make(map[baseKey]string)}
}

// Prepare prepares cfg.Branch on every repository. It returns no base, since
// each repository has its own tip (see Commit). cfg.Repos must be the number of
// repositories, so that Config.TargetOf spreads the agents of every repository
// over every branch.
func (m *MultiBackend) Prepare(ctx context.Context, cfg Config) (string, error) {
	if cfg.RepoCount() != len(m.repos) {
		return "", fmt.Errorf("config is for %d repositories, the backend has %d", cfg.RepoCount(), len(m.repos))
	}

	for repo, be := range m.repos {
		base, err := be.Prepare(ctx, cfg)
		if err != nil {
			return "", fmt.Errorf("repository %d: %w", repo, err)
		}

		m.mu.Lock()
		m.bases[baseKey{branch: cfg.Branch, repo: repo}] = base
		m.mu.Unlock()
	}

	return "", nil
}

// Commit writes the change on top of the tip of the agent's repository. The
// base argument is ignored (see MultiBackend).
func (m *MultiBackend) Commit(ctx context.Context, c Change, _ string) (string, error) {
	repo := repoOfAgent(c.Agent, len(m.repos))

	m.mu.Lock()
	base, ok := m.bases[baseKey{branch: c.Target, repo: repo}]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("no tip of branch %s in repository %d: prepare it first", c.Target, repo)
	}

	return m.repos[repo].Commit(ctx, c, base)
}

// Land lands commit in the agent's repository.
func (m *MultiBackend) Land(ctx context.Context, cfg Config, c Change, commit string) (LandOutcome, error) {
	return m.repos[repoOfAgent(c.Agent, len(m.repos))].Land(ctx, cfg, c, commit)
}
