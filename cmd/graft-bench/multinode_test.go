package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTopologyRoundRobin(t *testing.T) {
	t.Parallel()

	t.Run("six repositories over three nodes", func(t *testing.T) {
		t.Parallel()

		top := Topology{Addrs: []string{"unix:/n0", "unix:/n1", "unix:/n2"}, Repos: 6}
		for repo := 0; repo < 6; repo++ {
			require.Equal(t, repo%3, top.NodeOfRepo(repo), "repo %d", repo)
		}
		for agent := 0; agent < 60; agent++ {
			require.Equal(t, agent%6%3, top.NodeOfAgent(agent), "agent %d", agent)
		}
		for node := 0; node < 3; node++ {
			require.Equal(t, 2, top.ReposOnNode(node), "repos on node %d", node)
			require.Equal(t, 20, top.AgentsOnNode(60, node), "agents on node %d", node)
		}
	})

	t.Run("uneven repositories", func(t *testing.T) {
		t.Parallel()

		// Repos 0..6 land on nodes 0,1,2,0,1,2,0. Agents 0..9 work on repos
		// 0..6 and then 0..2 again.
		top := Topology{Addrs: []string{"a", "b", "c"}, Repos: 7}
		require.Equal(t, []int{3, 2, 2}, []int{top.ReposOnNode(0), top.ReposOnNode(1), top.ReposOnNode(2)})
		require.Equal(t, []int{4, 3, 3}, []int{top.AgentsOnNode(10, 0), top.AgentsOnNode(10, 1), top.AgentsOnNode(10, 2)})
	})

	t.Run("repository of agent", func(t *testing.T) {
		t.Parallel()

		require.Equal(t, 0, repoOfAgent(0, 6))
		require.Equal(t, 5, repoOfAgent(5, 6))
		require.Equal(t, 0, repoOfAgent(6, 6))
		require.Equal(t, 1, repoOfAgent(7, 6))
	})
}

func TestTopologyRepoPath(t *testing.T) {
	t.Parallel()

	top := Topology{Addrs: []string{"a"}, Repos: 1}
	require.Equal(t, "bench-03.git", top.RepoPath("bench.git", 3))
	require.Equal(t, "bench-00.git", top.RepoPath("bench", 0))
	require.Equal(t, "bench-12.git", top.RepoPath("bench.git", 12))
}

func TestTopologyValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		top    Topology
		agents int
		want   string
	}{
		{name: "valid", top: Topology{Addrs: []string{"a", "b"}, Repos: 4}, agents: 8},
		{name: "one repository per node", top: Topology{Addrs: []string{"a", "b"}, Repos: 2}, agents: 2},
		{name: "no addresses", top: Topology{Repos: 1}, agents: 1, want: "at least one address"},
		{name: "empty address", top: Topology{Addrs: []string{"a", ""}, Repos: 2}, agents: 2, want: "must not be empty"},
		{name: "duplicate address", top: Topology{Addrs: []string{"a", "a"}, Repos: 2}, agents: 2, want: "listed twice"},
		{name: "fewer repositories than nodes", top: Topology{Addrs: []string{"a", "b", "c"}, Repos: 2}, agents: 9, want: "every node hosts a repository"},
		{name: "negative repositories", top: Topology{Addrs: []string{"a"}, Repos: -1}, agents: 1, want: "must be at least"},
		{name: "more repositories than agents", top: Topology{Addrs: []string{"a"}, Repos: 5}, agents: 4, want: "must not exceed agents"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.top.Validate(tt.agents)
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestResolvePlacement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		address   string
		addrs     string
		repos     int
		agents    int
		want      string
		wantMulti bool
		wantRepos int
		wantAddrs []string
	}{
		{name: "single node keeps the address", address: "unix:/a", agents: 4, wantAddrs: []string{"unix:/a"}},
		{name: "no address", agents: 4, want: "--address is required"},
		{name: "addrs and address", address: "unix:/a", addrs: "unix:/b", agents: 4, want: "mutually exclusive"},
		{name: "addrs with one repository per node", addrs: "a,b,c", agents: 10, wantMulti: true, wantRepos: 3, wantAddrs: []string{"a", "b", "c"}},
		{name: "addrs are trimmed", addrs: " a , b", agents: 4, wantMulti: true, wantRepos: 2, wantAddrs: []string{"a", "b"}},
		{name: "repos on one address", address: "unix:/a", repos: 4, agents: 8, wantMulti: true, wantRepos: 4, wantAddrs: []string{"unix:/a"}},
		{name: "repos without address", repos: 2, agents: 4, want: "--address or --addrs is required with --repos"},
		{name: "repos fewer than addrs", addrs: "a,b,c", repos: 2, agents: 9, want: "must be at least the number of addresses"},
		{name: "repos more than agents", addrs: "a,b", repos: 5, agents: 4, want: "must not exceed agents"},
		{name: "negative repos", addrs: "a", repos: -1, agents: 4, want: "must be at least"},
		{name: "duplicate address", addrs: "a,a", agents: 4, want: "listed twice"},
		{name: "empty entry", addrs: "a,", agents: 4, want: "must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			place, err := resolvePlacement(tt.address, tt.addrs, tt.repos, tt.agents)
			if tt.want != "" {
				require.ErrorContains(t, err, tt.want)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantMulti, place.multi)
			require.Equal(t, tt.wantAddrs, place.addrs)
			if tt.wantMulti {
				require.Equal(t, tt.wantRepos, place.repos)
			}
		})
	}
}

func TestParseOptionsMultiNode(t *testing.T) {
	t.Parallel()

	t.Run("addrs and repos", func(t *testing.T) {
		t.Parallel()

		opts, err := parseOptions([]string{
			"--addrs", "unix:/a,unix:/b,unix:/c", "--repo", "bench.git", "--repos", "6", "--agents", "60",
		}, &bytes.Buffer{})
		require.NoError(t, err)
		require.True(t, opts.placement.multi)
		require.Equal(t, []string{"unix:/a", "unix:/b", "unix:/c"}, opts.placement.addrs)
		require.Equal(t, 6, opts.placement.repos)
		require.Equal(t, 60, opts.config.Agents)
		require.Equal(t, 6, opts.config.Repos, "the run spreads the agents of each repository over the branches")
	})

	t.Run("every repository needs an agent on every branch", func(t *testing.T) {
		t.Parallel()

		opts, err := parseOptions([]string{
			"--addrs", "unix:/a,unix:/b", "--repo", "bench.git", "--repos", "6", "--agents", "12", "--branches", "2",
		}, &bytes.Buffer{})
		require.NoError(t, err)
		require.Equal(t, 6, opts.config.Repos)

		_, err = parseOptions([]string{
			"--addrs", "unix:/a,unix:/b", "--repo", "bench.git", "--repos", "6", "--agents", "11", "--branches", "2",
		}, &bytes.Buffer{})
		require.ErrorContains(t, err, "every repository needs an agent on every branch")
	})

	t.Run("addrs default to one repository per node", func(t *testing.T) {
		t.Parallel()

		opts, err := parseOptions([]string{"--addrs", "unix:/a,unix:/b", "--repo", "bench.git"}, &bytes.Buffer{})
		require.NoError(t, err)
		require.Equal(t, 2, opts.placement.repos)
	})

	t.Run("single node is unchanged", func(t *testing.T) {
		t.Parallel()

		opts, err := parseOptions([]string{"--address", "unix:/a", "--repo", "bench.git"}, &bytes.Buffer{})
		require.NoError(t, err)
		require.False(t, opts.placement.multi)
		require.Equal(t, "unix:/a", opts.address)
		require.Equal(t, []string{"unix:/a"}, opts.placement.addrs)
		require.Zero(t, opts.config.Repos)
	})

	t.Run("address and addrs conflict", func(t *testing.T) {
		t.Parallel()

		_, err := parseOptions([]string{"--address", "unix:/a", "--addrs", "unix:/b", "--repo", "bench.git"}, &bytes.Buffer{})
		require.ErrorContains(t, err, "mutually exclusive")
	})

	t.Run("repos fewer than addrs", func(t *testing.T) {
		t.Parallel()

		_, err := parseOptions([]string{"--addrs", "unix:/a,unix:/b,unix:/c", "--repo", "bench.git", "--repos", "2"}, &bytes.Buffer{})
		require.ErrorContains(t, err, "every node hosts a repository")
	})
}

// TestAggregateNodes checks that the per-node results add up to the run and
// that each node aggregates the samples of its own agents.
func TestAggregateNodes(t *testing.T) {
	t.Parallel()

	top := Topology{Addrs: []string{"unix:/n0", "unix:/n1"}, Repos: 2}
	cfg := Config{Agents: 4, ChangesPerAgent: 1, Mode: ModeLand, Branch: DefaultBranch, Seed: 1}
	samples := []Sample{
		// Agents 0 and 2 are on node 0 (repo 0), agents 1 and 3 on node 1.
		{Agent: 0, Landed: true, Latency: 10 * time.Millisecond},
		{Agent: 1, Landed: true, Latency: 20 * time.Millisecond},
		{Agent: 2, Latency: 5 * time.Millisecond, Err: errors.New("land: merge conflict"), Conflict: true},
		{Agent: 3, Landed: true, Latency: 40 * time.Millisecond},
	}
	elapsed := time.Minute

	nodes := aggregateNodes(cfg, top, samples, elapsed)
	require.Len(t, nodes, 2)

	require.Equal(t, "unix:/n0", nodes[0].Address)
	require.Equal(t, 1, nodes[0].Repos)
	require.Equal(t, 2, nodes[0].Agents)
	require.Equal(t, 1, nodes[0].Result.Landed)
	require.Equal(t, 1, nodes[0].Result.Failed)
	require.Equal(t, 1, nodes[0].Result.Conflicts)
	require.InDelta(t, 1.0, nodes[0].Result.LandsPerMinute, 1e-9)
	require.Equal(t, 10.0, nodes[0].Result.P50MS)
	require.Equal(t, 5.0, nodes[0].Result.FailedP95MS)

	require.Equal(t, "unix:/n1", nodes[1].Address)
	require.Equal(t, 2, nodes[1].Agents)
	require.Equal(t, 2, nodes[1].Result.Landed)
	require.InDelta(t, 2.0, nodes[1].Result.LandsPerMinute, 1e-9)
	require.Equal(t, 20.0, nodes[1].Result.P50MS)
	require.Equal(t, 40.0, nodes[1].Result.P95MS)

	total := aggregate(cfg, samples, elapsed)
	require.Equal(t, total.Landed, nodes[0].Result.Landed+nodes[1].Result.Landed)
	require.Equal(t, total.Failed, nodes[0].Result.Failed+nodes[1].Result.Failed)
	require.InDelta(t, total.LandsPerMinute, nodes[0].Result.LandsPerMinute+nodes[1].Result.LandsPerMinute, 1e-9)
}

// recordingRepo is a repository backend that records the commits of each
// agent. Its tip is named after the branch, and a commit is only accepted on
// that tip.
type recordingRepo struct {
	name string

	mu      sync.Mutex
	commits map[int]int
	// targets counts the lands by target branch.
	targets map[string]int
}

func newRecordingRepo(name string) *recordingRepo {
	return &recordingRepo{name: name, commits: map[int]int{}, targets: map[string]int{}}
}

func (r *recordingRepo) Prepare(_ context.Context, cfg Config) (string, error) {
	return r.name + "@" + cfg.Branch, nil
}

func (r *recordingRepo) Commit(_ context.Context, c Change, base string) (string, error) {
	if want := r.name + "@" + c.Target; base != want {
		return "", fmt.Errorf("commit on base %q, want %q", base, want)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.commits[c.Agent]++

	return r.name + "-commit", nil
}

func (r *recordingRepo) Land(_ context.Context, _ Config, c Change, commit string) (LandOutcome, error) {
	if commit != r.name+"-commit" {
		return LandOutcome{}, fmt.Errorf("land of %q in %s", commit, r.name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets[c.Target]++

	return LandOutcome{Attempts: 1}, nil
}

// recordingBackends returns n recording repositories and the same repositories
// as backends.
func recordingBackends(n int) ([]*recordingRepo, []Backend) {
	repos := make([]*recordingRepo, n)
	backends := make([]Backend, n)
	for i := range repos {
		repos[i] = newRecordingRepo(fmt.Sprintf("r%d", i))
		backends[i] = repos[i]
	}
	return repos, backends
}

// TestMultiBackendLandsEveryRepoOnEveryBranch checks that a multi-node run with
// several branches exercises every branch of every repository, also when the
// numbers of repositories and branches share a factor.
func TestMultiBackendLandsEveryRepoOnEveryBranch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		agents, repos, branches int
	}{
		{agents: 4, repos: 2, branches: 2},
		{agents: 12, repos: 6, branches: 2},
		{agents: 60, repos: 6, branches: 2},
		{agents: 6, repos: 2, branches: 3},
		{agents: 13, repos: 3, branches: 4},
	} {
		t.Run(fmt.Sprintf("%d agents %d repos %d branches", tc.agents, tc.repos, tc.branches), func(t *testing.T) {
			t.Parallel()

			repos, backends := recordingBackends(tc.repos)
			cfg := Config{Agents: tc.agents, ChangesPerAgent: 1, Mode: ModeLand, Branch: "b", Branches: tc.branches, Repos: tc.repos}
			res, err := Run(context.Background(), cfg, NewMultiBackend(backends))
			require.NoError(t, err)
			require.Equal(t, tc.agents, res.Landed)
			require.Equal(t, tc.branches, res.Branches)

			for _, r := range repos {
				for branch := 0; branch < tc.branches; branch++ {
					require.Positive(t, r.targets[cfg.BranchName(branch)], "repository %s never landed on %s: %v", r.name, cfg.BranchName(branch), r.targets)
				}
				require.Len(t, r.targets, tc.branches, "repository %s", r.name)
			}
		})
	}
}

func TestMultiBackendRejectsAConfigForOtherRepos(t *testing.T) {
	t.Parallel()

	_, backends := recordingBackends(2)
	cfg := Config{Agents: 4, ChangesPerAgent: 1, Mode: ModeLand, Branch: "b", Branches: 2}
	_, err := Run(context.Background(), cfg, NewMultiBackend(backends))
	require.ErrorContains(t, err, "config is for 1 repositories, the backend has 2")
}

// TestMultiBackendRoutesAgentsRoundRobin checks that every agent commits and
// lands in its own repository only, on that repository's tip, and that the run
// covers every change.
func TestMultiBackendRoutesAgentsRoundRobin(t *testing.T) {
	t.Parallel()

	repos := []*recordingRepo{newRecordingRepo("r0"), newRecordingRepo("r1"), newRecordingRepo("r2")}
	backends := make([]Backend, len(repos))
	for i, r := range repos {
		backends[i] = r
	}

	cfg := Config{Agents: 6, ChangesPerAgent: 2, Mode: ModeLand, Branch: DefaultBranch, Seed: 1, Repos: len(repos)}
	res, err := Run(context.Background(), cfg, NewMultiBackend(backends))
	require.NoError(t, err)
	require.Equal(t, 12, res.Landed)
	require.Zero(t, res.Failed)
	require.Empty(t, res.Errors)

	for i, r := range repos {
		want := map[int]int{}
		for agent := 0; agent < cfg.Agents; agent++ {
			if agent%len(repos) == i {
				want[agent] = cfg.ChangesPerAgent
			}
		}
		require.Equal(t, want, r.commits, "repository %s", r.name)
	}
}

func TestMultiBackendCommitNeedsPrepare(t *testing.T) {
	t.Parallel()

	be := NewMultiBackend([]Backend{newRecordingRepo("r0")})
	_, err := be.Commit(context.Background(), Change{Agent: 0, Target: DefaultBranch}, "")
	require.ErrorContains(t, err, "prepare it first")
}

func TestWriteNodeRows(t *testing.T) {
	t.Parallel()

	t.Run("single node writes nothing", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		writeNodeRows(&buf, Result{Landed: 3})
		require.Empty(t, buf.String())
	})

	t.Run("multi node lists the placement and every node", func(t *testing.T) {
		t.Parallel()

		res := Result{
			Repos: 2,
			Nodes: []NodeResult{{Address: "unix:/a", Repos: 1, Agents: 2, Result: Result{Landed: 3, LandsPerMinute: 6, P50MS: 1.5}}},
		}
		var buf bytes.Buffer
		writeNodeRows(&buf, res)
		require.Contains(t, buf.String(), "repositories\t2 on 1 nodes")
		require.Contains(t, buf.String(), "node unix:/a\t1 repos, 2 agents, landed 3, failed 0, lands/min 6.0")
	})
}

func TestWriteNodeMarkdown(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, writeNodeMarkdown(&buf, Result{}))
	require.Empty(t, buf.String())

	res := Result{
		Repos: 2,
		Nodes: []NodeResult{{Address: "unix:/a|b", Repos: 1, Agents: 2, Result: Result{Landed: 3}}},
	}
	require.NoError(t, writeNodeMarkdown(&buf, res))
	require.Contains(t, buf.String(), "Per node (2 repositories):")
	require.Contains(t, buf.String(), "| unix:/a\\|b | 1 | 2 | 3 | 0 |")
}

func TestWriteResultMultiNodeJSON(t *testing.T) {
	t.Parallel()

	res := Result{Repos: 2, Nodes: []NodeResult{{Address: "unix:/a", Repos: 2, Agents: 4}}}
	var js bytes.Buffer
	require.NoError(t, writeResult(&js, "json", res))
	require.True(t, strings.Contains(js.String(), `"nodes"`))
	require.True(t, strings.Contains(js.String(), `"repos": 2`))

	var single bytes.Buffer
	require.NoError(t, writeResult(&single, "json", Result{Landed: 1}))
	require.NotContains(t, single.String(), `"nodes"`, "a single-node result keeps its JSON")
}
