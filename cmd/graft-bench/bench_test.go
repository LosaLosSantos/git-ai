package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/sdk"
)

// fakeBackend lands changes in memory. The first shared change lands; every
// later shared change conflicts, as it would with a rebase onto the new tip.
// It records how many lands ran at the same time.
type fakeBackend struct {
	mu           sync.Mutex
	sharedLanded bool
	commits      int
	lands        int

	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	landDelay   time.Duration
}

func (f *fakeBackend) Prepare(context.Context, Config) (string, error) {
	return "base-oid", nil
}

func (f *fakeBackend) Commit(_ context.Context, c Change, base string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits++
	return base + "-" + c.Path(), nil
}

func (f *fakeBackend) Land(_ context.Context, _ Config, c Change, _ string) (LandOutcome, error) {
	now := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		max := f.maxInFlight.Load()
		if now <= max || f.maxInFlight.CompareAndSwap(max, now) {
			break
		}
	}
	if f.landDelay > 0 {
		time.Sleep(f.landDelay)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.lands++

	if !c.Shared {
		return LandOutcome{Attempts: 1}, nil
	}
	if f.sharedLanded {
		return LandOutcome{Attempts: 1, Conflict: true}, errors.New("merge conflict in " + sharedFilePath)
	}
	f.sharedLanded = true
	return LandOutcome{Attempts: 2}, nil
}

func baseConfig() Config {
	return Config{
		Agents:          4,
		ChangesPerAgent: 3,
		Mode:            ModeLand,
		Branch:          DefaultBranch,
		Seed:            1,
	}
}

func TestRunCountsOutcomes(t *testing.T) {
	t.Parallel()

	t.Run("no conflicts", func(t *testing.T) {
		t.Parallel()

		cfg := baseConfig()
		res, err := Run(context.Background(), cfg, &fakeBackend{})
		require.NoError(t, err)

		require.Equal(t, 12, res.Changes)
		require.Equal(t, 12, res.Landed)
		require.Zero(t, res.Failed)
		require.Zero(t, res.Conflicts)
		require.Equal(t, int64(12), res.Attempts)
		require.Empty(t, res.Errors)
		require.Positive(t, res.LandsPerMinute)
		require.LessOrEqual(t, res.P50MS, res.P95MS)
		require.LessOrEqual(t, res.P95MS, res.P99MS)
	})

	t.Run("every change shared", func(t *testing.T) {
		t.Parallel()

		cfg := baseConfig()
		cfg.ConflictRate = 1
		res, err := Run(context.Background(), cfg, &fakeBackend{})
		require.NoError(t, err)

		require.Equal(t, 12, res.Changes)
		require.Equal(t, 1, res.Landed)
		require.Equal(t, 11, res.Failed)
		require.Equal(t, 11, res.Conflicts)
		require.NotEmpty(t, res.Errors)
		require.LessOrEqual(t, len(res.Errors), maxReportedErrors)
	})
}

func TestRunQueueModeSerializesLands(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Mode = ModeQueue
	cfg.TestDuration = time.Millisecond

	be := &fakeBackend{landDelay: time.Millisecond}
	res, err := Run(context.Background(), cfg, be)
	require.NoError(t, err)

	require.Equal(t, int32(1), be.maxInFlight.Load(), "queue mode must land one change at a time")
	require.Equal(t, 12, res.Landed)
	require.Equal(t, 1.0, res.TestDurationMS)
}

func TestRunParallelLandsInLandMode(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	be := &fakeBackend{landDelay: 5 * time.Millisecond}
	_, err := Run(context.Background(), cfg, be)
	require.NoError(t, err)
	require.Greater(t, be.maxInFlight.Load(), int32(1))
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Agents = 0
	_, err := Run(context.Background(), cfg, &fakeBackend{})
	require.ErrorContains(t, err, "invalid config")
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	valid := baseConfig()
	require.NoError(t, valid.Validate())

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no agents", func(c *Config) { c.Agents = 0 }, "agents"},
		{"no changes", func(c *Config) { c.ChangesPerAgent = 0 }, "changes per agent"},
		{"rate below zero", func(c *Config) { c.ConflictRate = -0.1 }, "conflict rate"},
		{"rate above one", func(c *Config) { c.ConflictRate = 1.1 }, "conflict rate"},
		{"negative test duration", func(c *Config) { c.Mode = ModeQueue; c.TestDuration = -1 }, "must not be negative"},
		{"test duration outside queue", func(c *Config) { c.TestDuration = time.Second }, "only used in queue mode"},
		{"empty branch", func(c *Config) { c.Branch = "" }, "branch"},
		{"negative attempts", func(c *Config) { c.MaxAttempts = -1 }, "max attempts"},
		{"unknown mode", func(c *Config) { c.Mode = "fast" }, "unknown mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := baseConfig()
			tt.mutate(&cfg)
			require.ErrorContains(t, cfg.Validate(), tt.want)
		})
	}
}

func TestPlanIsDeterministicAndHonorsRate(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.ChangesPerAgent = 200
	cfg.ConflictRate = 0.25

	first := plan(cfg, 3)
	require.Equal(t, first, plan(cfg, 3))
	require.NotEqual(t, first, plan(cfg, 4), "agents get different plans")

	shared := 0
	for _, c := range first {
		if c.Shared {
			shared++
		}
	}
	require.InDelta(t, 0.25, float64(shared)/200, 0.1)

	cfg.ConflictRate = 0
	for _, c := range plan(cfg, 0) {
		require.False(t, c.Shared)
	}
	cfg.ConflictRate = 1
	for _, c := range plan(cfg, 0) {
		require.True(t, c.Shared)
	}
}

func TestChangePathsAreUnique(t *testing.T) {
	t.Parallel()

	require.NotEqual(t, Change{Agent: 1, Seq: 2}.Path(), Change{Agent: 2, Seq: 1}.Path())
	require.Contains(t, string(Change{Agent: 1, Seq: 2, Shared: true}.SharedContent()), sharedFileHeader)
}

func TestPercentile(t *testing.T) {
	t.Parallel()

	values := []float64{5, 1, 4, 2, 3}
	require.Equal(t, 3.0, percentile(values, 50))
	require.Equal(t, 5.0, percentile(values, 95))
	require.Equal(t, 1.0, percentile(values, 0))
	require.Equal(t, 5.0, percentile(values, 100))
	require.Equal(t, []float64{5, 1, 4, 2, 3}, values, "input must not be reordered")
	require.Zero(t, percentile(nil, 50))
}

func TestLandOptions(t *testing.T) {
	t.Parallel()

	t.Run("land rebases without resolution", func(t *testing.T) {
		t.Parallel()

		cfg := baseConfig()
		cfg.MaxAttempts = 5
		c := Change{Agent: 7, Seq: 2, Target: cfg.Branch}
		opts := landOptions(cfg, c, "oid")
		require.Equal(t, cfg.Branch, opts.Branch)
		require.Equal(t, "oid", opts.Commit)
		require.Equal(t, sdk.LandRebase, opts.Strategy)
		require.False(t, opts.ResolveConflicts)
		require.Equal(t, int32(5), opts.MaxAttempts)
		require.Equal(t, "agent-007", opts.User.ID)
	})

	t.Run("land-resolve asks for resolution", func(t *testing.T) {
		t.Parallel()

		cfg := baseConfig()
		cfg.Mode = ModeLandResolve
		c := Change{Agent: 7, Seq: 2, Target: cfg.Branch}
		require.True(t, landOptions(cfg, c, "oid").ResolveConflicts)
	})
}

func TestLandOutcome(t *testing.T) {
	t.Parallel()

	out := landOutcome(&sdk.LandResult{
		Attempts:      3,
		Rewritten:     true,
		AlreadyLanded: false,
		ResolvedPaths: []string{"shared/log.txt"},
	})
	require.Equal(t, LandOutcome{Attempts: 3, Rewritten: true, Resolved: 1}, out)
	require.Zero(t, landOutcome(&sdk.LandResult{}).Resolved)

	batched := landOutcome(&sdk.LandResult{Batch: sdk.BatchInfo{Size: 7, Position: 3}})
	require.Equal(t, 7, batched.BatchSize)
	require.Equal(t, 3, batched.BatchPosition)
}

func TestParseOptions(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()

		opts, err := parseOptions([]string{"--address", "unix:/tmp/g.sock", "--repo", "r.git"}, &bytes.Buffer{})
		require.NoError(t, err)
		require.Equal(t, DefaultAgents, opts.config.Agents)
		require.Equal(t, DefaultChangesPerAgent, opts.config.ChangesPerAgent)
		require.Equal(t, ModeLand, opts.config.Mode)
		require.Equal(t, DefaultBranch, opts.config.Branch)
		// main is the branch a policy usually protects.
		require.NotEqual(t, "main", opts.config.Branch)
		require.Equal(t, "table", opts.format)
	})

	t.Run("queue with test duration", func(t *testing.T) {
		t.Parallel()

		opts, err := parseOptions([]string{
			"--address", "unix:/tmp/g.sock", "--repo", "r.git",
			"--mode", "queue", "--test-duration", "250ms", "--agents", "50",
		}, &bytes.Buffer{})
		require.NoError(t, err)
		require.Equal(t, ModeQueue, opts.config.Mode)
		require.Equal(t, 250*time.Millisecond, opts.config.TestDuration)
		require.Equal(t, 50, opts.config.Agents)
	})

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no address", []string{"--repo", "r.git"}, "--address is required"},
		{"no repo", []string{"--address", "unix:/a"}, "--repo is required"},
		{"both tokens", []string{"--address", "unix:/a", "--repo", "r", "--token", "a", "--agent-token", "b"}, "mutually exclusive"},
		{"bad format", []string{"--address", "unix:/a", "--repo", "r", "--format", "xml"}, "unknown format"},
		{"bad mode", []string{"--address", "unix:/a", "--repo", "r", "--mode", "x"}, "unknown mode"},
		{"positional", []string{"--address", "unix:/a", "--repo", "r", "extra"}, "unexpected arguments"},
		{"zero timeout", []string{"--address", "unix:/a", "--repo", "r", "--timeout", "0s"}, "--timeout must be positive"},
		{"create repo without the shared token", []string{"--address", "unix:/a", "--repo", "r", "--agent-token", "a", "--create-repo"}, "needs the shared --token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseOptions(tt.args, &bytes.Buffer{})
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestWriteResult(t *testing.T) {
	t.Parallel()

	res := Result{Mode: ModeLand, Agents: 2, ChangesPerAgent: 1, Changes: 2, Landed: 1, Failed: 1, Conflicts: 1, P50MS: 1.5, Errors: []string{"land: merge conflict"}}

	var table bytes.Buffer
	require.NoError(t, writeResult(&table, "table", res))
	require.Contains(t, table.String(), "landed")
	require.Contains(t, table.String(), "latency p99")
	require.Contains(t, table.String(), "merge conflict")

	var js bytes.Buffer
	require.NoError(t, writeResult(&js, "json", res))
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(js.Bytes(), &decoded))
	require.Equal(t, "land", decoded["mode"])
	require.Equal(t, float64(1), decoded["landed"])
	require.True(t, strings.Contains(js.String(), "\"p50_ms\""))
}

func TestRunCommandUsageErrors(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--repo", "r.git"}, &stdout, &stderr)
	require.Equal(t, 2, code)
	require.Contains(t, stderr.String(), "--address is required")
	require.Empty(t, stdout.String())
}

func TestResolveTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                      string
		explicit                  []string
		flagToken, flagAgentToken string
		envToken, envAgentToken   string
		wantToken, wantAgentToken string
		wantErr                   bool
	}{
		{name: "nothing set"},
		{name: "shared token from the environment", envToken: "s", wantToken: "s"},
		{name: "agent token from the environment", envAgentToken: "a", wantAgentToken: "a"},
		{name: "both from the environment prefer the shared token", envToken: "s", envAgentToken: "a", wantToken: "s"},
		{
			name: "token flag overrides the environment agent token", explicit: []string{"token"},
			flagToken: "f", envAgentToken: "a", wantToken: "f",
		},
		{
			name: "agent flag overrides the environment shared token", explicit: []string{"agent-token"},
			flagAgentToken: "a", envToken: "s", wantAgentToken: "a",
		},
		{
			name: "both flags", explicit: []string{"token", "agent-token"},
			flagToken: "f", flagAgentToken: "a", wantErr: true,
		},
		{
			name: "both flags with one empty", explicit: []string{"token", "agent-token"},
			flagToken: "f", envToken: "s", envAgentToken: "a", wantToken: "f",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			explicit := map[string]bool{}
			for _, name := range tt.explicit {
				explicit[name] = true
			}
			token, agentToken, err := resolveTokens(explicit, tt.flagToken, tt.flagAgentToken, tt.envToken, tt.envAgentToken)
			if tt.wantErr {
				require.ErrorContains(t, err, "mutually exclusive")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantToken, token)
			require.Equal(t, tt.wantAgentToken, agentToken)
		})
	}
}

// TestParseOptionsTokenEnvironment checks that the environment variables do
// not make an invocation fail when no token flag is passed, and that a flag
// overrides the other variable. It sets the environment, so it does not run
// in parallel.
func TestParseOptionsTokenEnvironment(t *testing.T) {
	t.Setenv("GRAFT_TOKEN", "shared")
	t.Setenv("GRAFT_AGENT_TOKEN", "agent")

	opts, err := parseOptions([]string{"--address", "unix:/a", "--repo", "r.git"}, &bytes.Buffer{})
	require.NoError(t, err)
	require.Equal(t, "shared", opts.token)
	require.Empty(t, opts.agentToken)

	opts, err = parseOptions([]string{"--address", "unix:/a", "--repo", "r.git", "--agent-token", "flag"}, &bytes.Buffer{})
	require.NoError(t, err)
	require.Empty(t, opts.token)
	require.Equal(t, "flag", opts.agentToken)
}

// TestAggregateReportsDistinctFailureKinds checks that the same failure on
// different commits is reported once, and that a different failure is still
// reported.
func TestAggregateReportsDistinctFailureKinds(t *testing.T) {
	t.Parallel()

	conflict := func(oid string) error {
		return errors.New(`land: rebase: commit "` + oid + `": there are conflicting files`)
	}
	samples := []Sample{
		{Err: conflict(strings.Repeat("a", 40)), Conflict: true},
		{Err: conflict(strings.Repeat("b", 64)), Conflict: true},
		{Err: errors.New("land: permission denied")},
	}
	res := aggregate(Config{Agents: 1, ChangesPerAgent: 3, Mode: ModeLand}, samples, time.Second)

	require.Equal(t, 3, res.Failed)
	require.Equal(t, 2, res.Conflicts)
	require.Len(t, res.Errors, 2, "errors: %v", res.Errors)
	require.Equal(t, errorKind(conflict(strings.Repeat("a", 40)).Error()), errorKind(conflict(strings.Repeat("b", 64)).Error()))
}

// TestRunStopsOnCancel checks that a run cut short by its context is an
// error, not a complete measurement.
func TestRunStopsOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := Run(ctx, baseConfig(), &fakeBackend{})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, res.Changes)
}

// cancelingBackend cancels the run while it commits, then fails the commit
// with the context error, as a canceled call does.
type cancelingBackend struct {
	fakeBackend
	cancel context.CancelFunc
}

func (b *cancelingBackend) Commit(ctx context.Context, _ Change, _ string) (string, error) {
	b.cancel()
	return "", ctx.Err()
}

// TestRunInterruptDuringLastChangeIsAnError checks that an interrupt that
// arrives while every agent is on its last change is still reported. Every
// change then has a sample, but the samples are failures caused by the
// interrupt, so the run must not look complete.
func TestRunInterruptDuringLastChangeIsAnError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := baseConfig()
	cfg.Agents = 1
	cfg.ChangesPerAgent = 1

	_, err := Run(ctx, cfg, &cancelingBackend{cancel: cancel})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "1 of 1 changes")
}
