package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Multi-node placement. Repository r is hosted by node r modulo the number of
// nodes, and agent a works on repository a modulo the number of repositories.
// Both rules are round robin, so the repositories, and with them the lands, are
// spread evenly over the nodes. With several branches, the agents of each
// repository are spread over the branches by their index within the repository
// (see Config.TargetOf), so every repository lands on every branch.

// repoOfAgent returns the repository that agent works on, out of repos.
func repoOfAgent(agent, repos int) int {
	return agent % repos
}

// Topology places the repositories of a run over the nodes.
type Topology struct {
	// Addrs holds one server address per node.
	Addrs []string
	// Repos is the number of repositories.
	Repos int
}

// Validate reports the first invalid field of t for a run of agents agents.
func (t Topology) Validate(agents int) error {
	if len(t.Addrs) == 0 {
		return errors.New("at least one address is required")
	}

	seen := make(map[string]bool, len(t.Addrs))
	for _, addr := range t.Addrs {
		if addr == "" {
			return errors.New("addresses must not be empty")
		}
		if seen[addr] {
			return fmt.Errorf("address %s is listed twice", addr)
		}
		seen[addr] = true
	}

	switch {
	case t.Repos < len(t.Addrs):
		return fmt.Errorf("repos (%d) must be at least the number of addresses (%d): every node hosts a repository", t.Repos, len(t.Addrs))
	case t.Repos > agents:
		return fmt.Errorf("repos (%d) must not exceed agents (%d): a repository without agents is never landed on", t.Repos, agents)
	}

	return nil
}

// NodeOfRepo returns the index of the node that hosts repo.
func (t Topology) NodeOfRepo(repo int) int {
	return repo % len(t.Addrs)
}

// NodeOfAgent returns the index of the node that serves agent.
func (t Topology) NodeOfAgent(agent int) int {
	return t.NodeOfRepo(repoOfAgent(agent, t.Repos))
}

// ReposOnNode returns the number of repositories that node hosts.
func (t Topology) ReposOnNode(node int) int {
	count := 0
	for repo := 0; repo < t.Repos; repo++ {
		if t.NodeOfRepo(repo) == node {
			count++
		}
	}

	return count
}

// AgentsOnNode returns how many of the agents agents-long run are served by
// node.
func (t Topology) AgentsOnNode(agents, node int) int {
	count := 0
	for agent := 0; agent < agents; agent++ {
		if t.NodeOfAgent(agent) == node {
			count++
		}
	}

	return count
}

// RepoPath returns the relative path of repository repo. The repositories are
// named after base, without its .git suffix, and numbered: bench.git gives
// bench-00.git, bench-01.git and so on.
func (t Topology) RepoPath(base string, repo int) string {
	return fmt.Sprintf("%s-%02d.git", strings.TrimSuffix(base, ".git"), repo)
}

// placement is where the repositories of a run are. A run without --addrs and
// without --repos is a single-node run on --address, the original benchmark,
// and has no placement beyond that address.
type placement struct {
	// addrs holds one address per node.
	addrs []string
	// repos is the number of repositories. It is zero for a single-node run.
	repos int
	// multi is true when --addrs or --repos is given.
	multi bool
}

// parseAddrs splits the comma-separated list of --addrs. The entries are
// trimmed; Topology.Validate rejects the empty ones.
func parseAddrs(list string) []string {
	parts := strings.Split(list, ",")
	addrs := make([]string, 0, len(parts))
	for _, part := range parts {
		addrs = append(addrs, strings.TrimSpace(part))
	}

	return addrs
}

// resolvePlacement resolves --address, --addrs and --repos into the placement
// of the run. Without --addrs and --repos the run is single-node on address.
// With --addrs and no --repos there is one repository per address. --address
// and --addrs are mutually exclusive.
func resolvePlacement(address, addrsFlag string, repos, agents int) (placement, error) {
	if addrsFlag == "" && repos == 0 {
		if address == "" {
			return placement{}, errors.New("--address is required")
		}

		return placement{addrs: []string{address}}, nil
	}

	switch {
	case addrsFlag != "" && address != "":
		return placement{}, errors.New("--addrs and --address are mutually exclusive")
	case addrsFlag == "" && address == "":
		return placement{}, errors.New("--address or --addrs is required with --repos")
	}

	addrs := []string{address}
	if addrsFlag != "" {
		addrs = parseAddrs(addrsFlag)
	}
	if repos == 0 {
		repos = len(addrs)
	}

	top := Topology{Addrs: addrs, Repos: repos}
	if err := top.Validate(agents); err != nil {
		return placement{}, err
	}

	return placement{addrs: addrs, repos: repos, multi: true}, nil
}

// NodeResult is the outcome of the repositories of one node.
type NodeResult struct {
	Address string `json:"address"`
	Repos   int    `json:"repos"`
	Agents  int    `json:"agents"`
	// Result aggregates the samples of the agents that the node serves. Its
	// lands per minute are over the wall time of the whole run, so the nodes
	// add up to the run.
	Result Result `json:"result"`
}

// aggregateNodes returns the result of every node of top. The samples of an
// agent belong to the node that serves the agent. The config is the one of the
// whole run.
func aggregateNodes(cfg Config, top Topology, samples []Sample, elapsed time.Duration) []NodeResult {
	perNode := make([][]Sample, len(top.Addrs))
	for _, s := range samples {
		node := top.NodeOfAgent(s.Agent)
		perNode[node] = append(perNode[node], s)
	}

	nodes := make([]NodeResult, len(top.Addrs))
	for node, addr := range top.Addrs {
		nodeCfg := cfg
		nodeCfg.Agents = top.AgentsOnNode(cfg.Agents, node)
		nodes[node] = NodeResult{
			Address: addr,
			Repos:   top.ReposOnNode(node),
			Agents:  nodeCfg.Agents,
			Result:  aggregate(nodeCfg, perNode[node], elapsed),
		}
	}

	return nodes
}

// nodeSummary describes the outcome of one node on one line.
func nodeSummary(n NodeResult) string {
	return fmt.Sprintf("%d repos, %d agents, landed %d, failed %d, lands/min %.1f, latency p50 %.1fms p95 %.1fms p99 %.1fms",
		n.Repos, n.Agents, n.Result.Landed, n.Result.Failed, n.Result.LandsPerMinute,
		n.Result.P50MS, n.Result.P95MS, n.Result.P99MS)
}

// writeNodeRows writes the placement and the per-node lines of the table
// report. A single-node result has none, so its table is unchanged.
func writeNodeRows(w io.Writer, res Result) {
	if len(res.Nodes) == 0 {
		return
	}

	fmt.Fprintf(w, "repositories\t%d on %d nodes\n", res.Repos, len(res.Nodes))
	for _, n := range res.Nodes {
		fmt.Fprintf(w, "node %s\t%s\n", n.Address, nodeSummary(n))
	}
}

// writeNodeMarkdown writes the per-node table of a Markdown report. A
// single-node result writes nothing.
func writeNodeMarkdown(w io.Writer, res Result) error {
	if len(res.Nodes) == 0 {
		return nil
	}

	header := []string{"node", "repos", "agents", "landed", "failed", "lands/min", "latency p50 (landed)", "latency p95 (landed)", "latency p99 (landed)"}
	rows := make([][]string, 0, len(res.Nodes))
	for _, n := range res.Nodes {
		rows = append(rows, []string{
			// A | would end the cell of the address in other renderers.
			strings.ReplaceAll(n.Address, "|", `\|`),
			fmt.Sprint(n.Repos),
			fmt.Sprint(n.Agents),
			fmt.Sprint(n.Result.Landed),
			fmt.Sprint(n.Result.Failed),
			fmt.Sprintf("%.1f", n.Result.LandsPerMinute),
			fmt.Sprintf("%.1fms", n.Result.P50MS),
			fmt.Sprintf("%.1fms", n.Result.P95MS),
			fmt.Sprintf("%.1fms", n.Result.P99MS),
		})
	}

	if _, err := fmt.Fprintf(w, "\nPer node (%d repositories):\n\n", res.Repos); err != nil {
		return err
	}

	return writeMarkdownRows(w, header, rows)
}
