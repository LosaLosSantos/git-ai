package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// branchBackend lands changes in memory, per target branch: the first shared
// change of a branch lands, and every later shared change of that branch
// conflicts. Changes that do not share the region always land. With batch set,
// every landed change reports a batch of four.
type branchBackend struct {
	mu       sync.Mutex
	prepared []string
	sharedOn map[string]bool
	landedOn map[string]int
	batch    bool
}

func newBranchBackend() *branchBackend {
	return &branchBackend{sharedOn: map[string]bool{}, landedOn: map[string]int{}}
}

func (b *branchBackend) Prepare(_ context.Context, cfg Config) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.prepared = append(b.prepared, cfg.Branch)
	return "base-" + cfg.Branch, nil
}

func (b *branchBackend) Commit(_ context.Context, c Change, base string) (string, error) {
	return base + "-" + c.Path(), nil
}

func (b *branchBackend) Land(_ context.Context, _ Config, c Change, commit string) (LandOutcome, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !strings.HasPrefix(commit, "base-"+c.Target+"-") {
		return LandOutcome{}, errors.New("commit is not based on " + c.Target)
	}
	b.landedOn[c.Target]++

	if c.Shared && b.sharedOn[c.Target] {
		return LandOutcome{Attempts: 1, Conflict: true}, errors.New("merge conflict in " + sharedFilePath)
	}

	out := LandOutcome{Attempts: 1}
	if c.Shared {
		b.sharedOn[c.Target] = true
		out.Attempts = 2
	}
	if b.batch {
		out.BatchSize = 4
		out.BatchPosition = c.Seq%4 + 1
	}

	return out, nil
}

func TestConfigBranchNames(t *testing.T) {
	t.Parallel()

	t.Run("several branches", func(t *testing.T) {
		t.Parallel()

		cfg := Config{Agents: 10, Branch: "bench", Branches: 3}
		require.Equal(t, 3, cfg.BranchCount())
		require.Equal(t, "bench-00", cfg.BranchName(0))
		require.Equal(t, "bench-02", cfg.BranchName(2))
		require.Equal(t, "bench-01", cfg.TargetOf(4), "agents are spread round robin")
		require.Equal(t, "bench-00", cfg.TargetOf(9))
	})

	t.Run("branches of the agents of each repository", func(t *testing.T) {
		t.Parallel()

		// Agent a works on repository a % 2. Spreading by agent alone would put
		// every agent of repository 0 on bench-00 and every agent of repository 1
		// on bench-01.
		cfg := Config{Agents: 8, Branch: "bench", Branches: 2, Repos: 2}
		var targets []string
		for agent := 0; agent < cfg.Agents; agent++ {
			targets = append(targets, cfg.TargetOf(agent))
		}
		require.Equal(t, []string{"bench-00", "bench-00", "bench-01", "bench-01", "bench-00", "bench-00", "bench-01", "bench-01"}, targets)
	})

	t.Run("zero branches means one", func(t *testing.T) {
		t.Parallel()

		cfg := Config{Agents: 10, Branch: "bench"}
		require.Equal(t, 1, cfg.BranchCount())
		require.Equal(t, "bench", cfg.BranchName(0))
		require.Equal(t, "bench", cfg.TargetOf(9))
	})
}

func TestConfigValidateBatchModeAndBranches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr string
	}{
		{"land-batch is a mode", func(c *Config) { c.Mode = ModeLandBatch }, ""},
		{"branches equal agents", func(c *Config) { c.Branches = c.Agents }, ""},
		{"negative branches", func(c *Config) { c.Branches = -1 }, "branches must not be negative"},
		{"more branches than agents", func(c *Config) { c.Branches = c.Agents + 1 }, "must not exceed agents"},
		{"negative repos", func(c *Config) { c.Repos = -1 }, "repos must not be negative"},
		{"every repository on every branch", func(c *Config) {
			c.Repos = 2
			c.Branches = 2
		}, ""},
		{"a repository without an agent on a branch", func(c *Config) {
			c.Repos = 3
			c.Branches = 2
		}, "every repository needs an agent on every branch"},
		{"test duration outside queue mode", func(c *Config) {
			c.Mode = ModeLandBatch
			c.TestDuration = time.Millisecond
		}, "only used in queue mode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := baseConfig()
			tt.modify(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestRunSpreadsAgentsOverBranches(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Agents:          6,
		ChangesPerAgent: 2,
		ConflictRate:    1,
		Mode:            ModeLand,
		Branch:          DefaultBranch,
		Branches:        3,
		Seed:            1,
	}
	be := newBranchBackend()
	res, err := Run(context.Background(), cfg, be)
	require.NoError(t, err)

	require.Equal(t, []string{"bench-00", "bench-01", "bench-02"}, be.prepared)
	require.Equal(t, map[string]int{"bench-00": 4, "bench-01": 4, "bench-02": 4}, be.landedOn)

	require.Equal(t, 3, res.Branches)
	require.Equal(t, 12, res.Changes)
	require.Equal(t, 3, res.Landed, "one change per branch lands")
	require.Equal(t, 9, res.Failed)
	require.Equal(t, 9, res.Conflicts, "every failure is a conflict, so no change landed on the wrong base")
	require.InDelta(t, 0.75, res.ObservedConflictRate, 1e-9)
	require.Equal(t, int64(15), res.Attempts, "3 landed changes of 2 attempts and 9 conflicts of 1")
	require.InDelta(t, 5, res.AttemptsPerLand, 1e-9)
	require.Equal(t, int32(2), res.MaxAttempts)
}

func TestRunHundredAgentsInBatchMode(t *testing.T) {
	t.Parallel()

	require.Equal(t, 50, DefaultAgents)

	cfg := baseConfig()
	cfg.Agents = 100
	cfg.ChangesPerAgent = 2
	cfg.Mode = ModeLandBatch

	be := newBranchBackend()
	be.batch = true
	res, err := Run(context.Background(), cfg, be)
	require.NoError(t, err)

	require.Equal(t, 200, res.Changes)
	require.Equal(t, 200, res.Landed)
	require.Zero(t, res.Failed)
	require.Zero(t, res.FailedP95MS)
	require.InDelta(t, 1, res.AttemptsPerLand, 1e-9)
	require.InDelta(t, 4, res.BatchSizeMean, 1e-9)
	require.Equal(t, 4, res.BatchSizeMax)
	require.Equal(t, ModeLandBatch, res.Mode)
}

func TestPercentileNearestRank(t *testing.T) {
	t.Parallel()

	hundred := make([]float64, 0, 100)
	for i := 1; i <= 100; i++ {
		hundred = append(hundred, float64(i))
	}
	ten := hundred[:10]
	twenty := hundred[:20]

	tests := []struct {
		name   string
		values []float64
		p      float64
		want   float64
	}{
		{"p50 of 100", hundred, 50, 50},
		{"p95 of 100", hundred, 95, 95},
		{"p99 of 100", hundred, 99, 99},
		{"p100 of 100", hundred, 100, 100},
		{"p1 of 100", hundred, 1, 1},
		{"p90 of 10", ten, 90, 9},
		{"p95 of 10", ten, 95, 10},
		{"p95 of 20", twenty, 95, 19},
		{"p50 of 3", []float64{1, 2, 3}, 50, 2},
		{"p33 of 3", []float64{1, 2, 3}, 33, 1},
		{"p34 of 3", []float64{1, 2, 3}, 34, 2},
	}

	for _, tt := range tests {
		require.Equal(t, tt.want, percentile(tt.values, tt.p), tt.name)
	}
}

func TestAggregateLatencyCoversLandedChangesOnly(t *testing.T) {
	t.Parallel()

	samples := []Sample{
		{Latency: 10 * time.Millisecond, Landed: true},
		{Latency: 20 * time.Millisecond, Landed: true},
		{Latency: 30 * time.Millisecond, Landed: true},
		{Latency: 1000 * time.Millisecond, Err: errors.New("merge conflict"), Conflict: true},
		{Latency: 2000 * time.Millisecond, Err: errors.New("hook rejected")},
	}

	res := aggregate(baseConfig(), samples, time.Second)

	require.Equal(t, 5, res.Changes)
	require.Equal(t, 3, res.Landed)
	require.Equal(t, 2, res.Failed)
	require.InDelta(t, 0.2, res.ObservedConflictRate, 1e-9)
	require.InDelta(t, 20, res.P50MS, 1e-9, "failed changes are not in the landed percentiles")
	require.InDelta(t, 30, res.P99MS, 1e-9)
	require.InDelta(t, 2000, res.FailedP95MS, 1e-9)
}

func TestAggregateBatchAndAttemptsStats(t *testing.T) {
	t.Parallel()

	t.Run("batches of landed changes", func(t *testing.T) {
		t.Parallel()

		samples := []Sample{
			{Landed: true, Outcome: LandOutcome{Attempts: 1, BatchSize: 2}},
			{Landed: true, Outcome: LandOutcome{Attempts: 3, BatchSize: 4}},
			{Err: errors.New("x"), Outcome: LandOutcome{Attempts: 5, BatchSize: 9}},
		}
		res := aggregate(baseConfig(), samples, time.Second)

		require.InDelta(t, 3, res.BatchSizeMean, 1e-9, "a failed change is not part of a landed batch")
		require.Equal(t, 4, res.BatchSizeMax)
		require.Equal(t, int64(9), res.Attempts)
		require.InDelta(t, 4.5, res.AttemptsPerLand, 1e-9, "attempts of failed changes count as server work, not per landed change")
		require.Equal(t, int32(5), res.MaxAttempts)
	})

	t.Run("nothing landed", func(t *testing.T) {
		t.Parallel()

		samples := []Sample{{Latency: time.Millisecond, Err: errors.New("x")}}
		res := aggregate(baseConfig(), samples, 0)

		require.Zero(t, res.AttemptsPerLand)
		require.Zero(t, res.P50MS)
		require.Zero(t, res.LandsPerMinute)
		require.InDelta(t, 1, res.FailedP95MS, 1e-9)
	})
}

func TestParseOptionsBranchesAndBatchMode(t *testing.T) {
	t.Parallel()

	opts, err := parseOptions([]string{
		"--address", "unix:/tmp/g.sock", "--repo", "r.git",
		"--mode", "land-batch", "--branches", "4", "--agents", "100",
	}, &bytes.Buffer{})
	require.NoError(t, err)
	require.Equal(t, ModeLandBatch, opts.config.Mode)
	require.Equal(t, 4, opts.config.Branches)
	require.Equal(t, 100, opts.config.Agents)
}

func TestWriteComparison(t *testing.T) {
	t.Parallel()

	results := []Result{
		{Mode: ModeLand, Agents: 50, Branches: 1, Changes: 500, Landed: 480, Failed: 20, P95MS: 40, FailedP95MS: 900},
		{Mode: ModeLandBatch, Agents: 50, Branches: 1, Changes: 500, Landed: 500, BatchSizeMean: 6.5, BatchSizeMax: 12, P95MS: 25},
	}
	labels := []string{"land.json", "land-batch.json"}

	t.Run("table", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		require.NoError(t, writeComparison(&out, "table", labels, results))
		text := out.String()
		require.Contains(t, text, "metric")
		require.Contains(t, text, "land-batch.json")
		require.Contains(t, text, "batch size mean")
		require.Contains(t, text, "6.50")
		require.Contains(t, text, "latency p95 (failed)")
	})

	t.Run("markdown", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		require.NoError(t, writeComparison(&out, "markdown", labels, results))
		lines := strings.Split(out.String(), "\n")
		require.Equal(t, "| metric | land.json | land-batch.json |", lines[0])
		require.Equal(t, "| --- | --- | --- |", lines[1])
		require.Contains(t, out.String(), "| landed | 480 | 500 |")
	})

	t.Run("errors", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		require.Error(t, writeComparison(&out, "table", labels[:1], results))
		require.Error(t, writeComparison(&out, "table", nil, nil))
		require.Error(t, writeComparison(&out, "json", labels, results))
	})
}

func TestRunCompareCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"land.json", "land-batch.json"} {
		data, err := json.Marshal(Result{Mode: ModeLand, Agents: 50, Branches: 1})
		require.NoError(t, err)
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, data, 0o600))
		paths = append(paths, path)
	}

	var out, errOut bytes.Buffer
	code := run(context.Background(), append([]string{"compare", "--format", "markdown"}, paths...), &out, &errOut)
	require.Zero(t, code, errOut.String())
	require.Contains(t, out.String(), "| metric | land.json | land-batch.json |")

	out.Reset()
	errOut.Reset()
	require.Equal(t, 2, run(context.Background(), []string{"compare"}, &out, &errOut), "compare needs a file")

	errOut.Reset()
	require.Equal(t, 1, run(context.Background(), []string{"compare", filepath.Join(dir, "missing.json")}, &out, &errOut))
	require.Contains(t, errOut.String(), "missing.json")
}

func TestAggregateRPCLatency(t *testing.T) {
	t.Parallel()

	samples := []Sample{
		{Landed: true, Committed: true, CommitLatency: 10 * time.Millisecond, LandLatency: 100 * time.Millisecond},
		{Landed: true, Committed: true, CommitLatency: 20 * time.Millisecond, LandLatency: 200 * time.Millisecond},
		{Committed: true, CommitLatency: 30 * time.Millisecond, LandLatency: 900 * time.Millisecond, Err: errors.New("merge conflict"), Conflict: true},
		{CommitLatency: 500 * time.Millisecond, Err: errors.New("commit: unavailable")},
	}

	res := aggregate(baseConfig(), samples, time.Second)

	commit := res.RPCs[rpcCommitFiles]
	require.Equal(t, 4, commit.Calls)
	require.Equal(t, 1, commit.Failed)
	require.InDelta(t, 20, commit.P50MS, 1e-9, "failed calls are not in the percentiles")
	require.InDelta(t, 30, commit.P99MS, 1e-9)

	land := res.RPCs[rpcLand]
	require.Equal(t, 3, land.Calls, "a change whose commit failed does not land")
	require.Equal(t, 1, land.Failed)
	require.InDelta(t, 100, land.P50MS, 1e-9)
	require.InDelta(t, 200, land.P99MS, 1e-9)
}

// slowBackend takes a fixed time for each call.
type slowBackend struct {
	commit, land time.Duration
}

func (b slowBackend) Prepare(context.Context, Config) (string, error) { return "base", nil }

func (b slowBackend) Commit(context.Context, Change, string) (string, error) {
	time.Sleep(b.commit)
	return "commit", nil
}

func (b slowBackend) Land(context.Context, Config, Change, string) (LandOutcome, error) {
	time.Sleep(b.land)
	return LandOutcome{Attempts: 1}, nil
}

// TestRunMeasuresEachRPC times the commit and the land of a change separately: the latency of the
// change covers both, each RPC only its own call.
func TestRunMeasuresEachRPC(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Agents, cfg.ChangesPerAgent = 2, 2
	res, err := Run(context.Background(), cfg, slowBackend{commit: 20 * time.Millisecond, land: 60 * time.Millisecond})
	require.NoError(t, err)

	commit, land := res.RPCs[rpcCommitFiles], res.RPCs[rpcLand]
	require.Equal(t, 4, commit.Calls)
	require.Equal(t, 4, land.Calls)
	require.GreaterOrEqual(t, commit.P50MS, 20.0)
	require.Less(t, commit.P50MS, 60.0, "the commit does not include the land")
	require.GreaterOrEqual(t, land.P50MS, 60.0)
	require.GreaterOrEqual(t, res.P50MS, 80.0, "a change takes its commit and its land")
}
