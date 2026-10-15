package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mode selects how the agents land their changes.
type Mode string

const (
	// ModeLand lands every change with UserLand (rebase). A change that
	// conflicts with the tip fails.
	ModeLand Mode = "land"
	// ModeLandResolve lands with UserLand and asks the server to resolve
	// merge conflicts through the configured resolver.
	ModeLandResolve Mode = "land-resolve"
	// ModeLandBatch lands like ModeLand, but the server is expected to batch
	// the concurrent lands of one branch into a single reference update (the
	// land lanes of phase 3). The client sends the same requests; the batch
	// placement of every land is reported in the result.
	ModeLandBatch Mode = "land-batch"
	// ModeQueue simulates a serialized merge queue: one change at a time is
	// landed, then tested for TestDuration before the next one starts.
	ModeQueue Mode = "queue"
)

// Config describes one benchmark run.
type Config struct {
	// Agents is the number of concurrent agents (goroutines).
	Agents int
	// ChangesPerAgent is the number of changes each agent commits and lands.
	ChangesPerAgent int
	// ConflictRate is the fraction of changes that also touch the shared
	// region of the shared file. Such changes append to the same place and
	// conflict with each other.
	ConflictRate float64
	// Mode selects the landing mode.
	Mode Mode
	// TestDuration is the simulated test time per change in queue mode.
	TestDuration time.Duration
	// Branch is the branch every change lands on. With Branches greater than
	// one it is the prefix of the branch names (see BranchName).
	Branch string
	// Branches is the number of target branches. The agents are spread over
	// them round robin, so each branch has its own contention. Zero means one.
	Branches int
	// Repos is the number of repositories of a multi-node run (see
	// MultiBackend). Agent a works on repository a modulo Repos, and the agents
	// of each repository are spread over the branches round robin (see
	// TargetOf), so every repository lands on every branch. Zero means one.
	Repos int
	// Seed makes the change plan (which changes conflict) reproducible.
	Seed int64
	// MaxAttempts bounds the server-side compare-and-swap retries of a land.
	// Zero means the server default.
	MaxAttempts int32
}

// Defaults for the benchmark flags.
const (
	DefaultAgents          = 50
	DefaultChangesPerAgent = 10
	// DefaultBranch is not main: main is usually a protected branch (see
	// examples/graft/config.toml), and a run needs a branch of its own.
	DefaultBranch = "bench"
	// agentsDir holds the file of every change (Change.Path).
	agentsDir        = "agents"
	sharedFilePath   = "shared/log.txt"
	sharedFileHeader = "# graft-bench shared log\n"
)

// Validate reports the first invalid field of c.
func (c Config) Validate() error {
	switch {
	case c.Agents < 1:
		return errors.New("agents must be at least 1")
	case c.ChangesPerAgent < 1:
		return errors.New("changes per agent must be at least 1")
	case c.ConflictRate < 0 || c.ConflictRate > 1:
		return errors.New("conflict rate must be between 0 and 1")
	case c.TestDuration < 0:
		return errors.New("test duration must not be negative")
	case c.Branch == "":
		return errors.New("branch must not be empty")
	case c.Branches < 0:
		return errors.New("branches must not be negative")
	case c.BranchCount() > c.Agents:
		return fmt.Errorf("branches (%d) must not exceed agents (%d): a branch without agents is never landed on", c.BranchCount(), c.Agents)
	case c.Repos < 0:
		return errors.New("repos must not be negative")
	case c.BranchCount() > c.Agents/c.RepoCount():
		// The repository with the fewest agents has Agents/RepoCount of them.
		return fmt.Errorf("branches (%d) times repos (%d) must not exceed agents (%d): every repository needs an agent on every branch",
			c.BranchCount(), c.RepoCount(), c.Agents)
	case c.MaxAttempts < 0:
		return errors.New("max attempts must not be negative")
	}

	switch c.Mode {
	case ModeLand, ModeLandResolve, ModeLandBatch, ModeQueue:
	default:
		return fmt.Errorf("unknown mode %q, want land, land-resolve, land-batch or queue", c.Mode)
	}

	if c.TestDuration > 0 && c.Mode != ModeQueue {
		return errors.New("test duration is only used in queue mode")
	}

	return nil
}

// BranchCount is the number of target branches of the run.
func (c Config) BranchCount() int {
	if c.Branches < 1 {
		return 1
	}
	return c.Branches
}

// BranchName is the name of target branch i, from 0 to BranchCount-1. A
// single branch keeps the name Branch.
func (c Config) BranchName(i int) string {
	if c.BranchCount() == 1 {
		return c.Branch
	}
	return fmt.Sprintf("%s-%02d", c.Branch, i)
}

// RepoCount is the number of repositories of the run.
func (c Config) RepoCount() int {
	if c.Repos < 1 {
		return 1
	}
	return c.Repos
}

// TargetOf is the branch the changes of agent land on. The agents of one
// repository are spread over the branches round robin by their index within
// the repository, agent / RepoCount. Spreading them by agent alone would
// correlate the branch with the repository whenever the numbers of
// repositories and branches share a factor: with 2 of each, every repository
// would only ever land on one branch.
func (c Config) TargetOf(agent int) string {
	return c.BranchName((agent / c.RepoCount()) % c.BranchCount())
}

// Change is one planned change of one agent.
type Change struct {
	Agent int
	Seq   int
	// Target is the branch the change lands on. It names the private branches
	// of the change, so runs on different branches never collide.
	Target string
	// Shared is true if the change also touches the shared file region.
	Shared bool
}

// Path is the file the change adds. Every change has its own path, so only
// the shared region can conflict.
func (c Change) Path() string {
	return fmt.Sprintf("%s/agent-%03d/change-%04d.txt", agentsDir, c.Agent, c.Seq)
}

// Content is the content of the file the change adds.
func (c Change) Content() []byte {
	return []byte(fmt.Sprintf("agent %d change %d\n", c.Agent, c.Seq))
}

// SharedContent is the content of the shared file after the change. It
// appends one line to the shared header, so two shared changes made from the
// same base conflict unless the server resolves them.
func (c Change) SharedContent() []byte {
	return []byte(fmt.Sprintf("%sagent %d change %d\n", sharedFileHeader, c.Agent, c.Seq))
}

// plan decides, deterministically from the seed, which changes are shared.
func plan(cfg Config, agent int) []Change {
	rng := rand.New(rand.NewSource(cfg.Seed + int64(agent)*7919))

	changes := make([]Change, 0, cfg.ChangesPerAgent)
	for seq := 1; seq <= cfg.ChangesPerAgent; seq++ {
		changes = append(changes, Change{
			Agent:  agent,
			Seq:    seq,
			Target: cfg.TargetOf(agent),
			Shared: rng.Float64() < cfg.ConflictRate,
		})
	}

	return changes
}

// Backend performs the server calls of a benchmark run. The sdk backend is
// used by the command; tests use fakes.
type Backend interface {
	// Prepare makes sure the branch cfg.Branch exists and returns its tip.
	Prepare(ctx context.Context, cfg Config) (base string, err error)
	// Commit writes the change on top of base and returns the commit OID.
	Commit(ctx context.Context, c Change, base string) (commit string, err error)
	// Land lands commit on the branch c.Target.
	Land(ctx context.Context, cfg Config, c Change, commit string) (LandOutcome, error)
}

// LandOutcome is what a successful or failed land reports back.
type LandOutcome struct {
	Attempts      int32
	Rewritten     bool
	AlreadyLanded bool
	// Conflict is true if the land failed on a merge conflict.
	Conflict bool
	// Resolved is the number of conflicting paths the server resolved.
	Resolved int
	// BatchSize is the number of lands in the batch of this land, and
	// BatchPosition its place in the batch (from 1). Both are zero when the
	// server does not batch.
	BatchSize     int
	BatchPosition int
}

// Sample is the measurement of one change.
type Sample struct {
	Latency  time.Duration
	Landed   bool
	Conflict bool
	Err      error
	Outcome  LandOutcome
	// Agent is the agent that made the change. Multi-node runs group the
	// samples by the node that serves the agent.
	Agent int
}

// Result is the aggregated outcome of a run. Its JSON form is the output of
// --format json.
//
// The latency percentiles cover the landed changes only: the time a failed
// change spent before it failed is not the time a landed change takes. The
// failed changes have their own percentile, FailedP95MS.
type Result struct {
	// Label names the run in reports, such as "batching-on-50". Empty if the
	// run had no --label.
	Label           string  `json:"label,omitempty"`
	Mode            Mode    `json:"mode"`
	Agents          int     `json:"agents"`
	ChangesPerAgent int     `json:"changes_per_agent"`
	Branches        int     `json:"branches"`
	ConflictRate    float64 `json:"conflict_rate"`
	TestDurationMS  float64 `json:"test_duration_ms"`

	Changes       int   `json:"changes"`
	Landed        int   `json:"landed"`
	AlreadyLanded int   `json:"already_landed"`
	Failed        int   `json:"failed"`
	Conflicts     int   `json:"conflicts"`
	Resolutions   int   `json:"resolutions"`
	Attempts      int64 `json:"attempts"`

	// Contention. ObservedConflictRate is the share of the changes that
	// failed on a merge conflict.
	//
	// Attempts counts the server attempts (compare-and-swap rounds) of every
	// change, landed or failed: a land that gave up because the branch kept
	// moving reports the attempts it made (see failedLandOutcome). Other
	// failed lands report none, since the server does not say. AttemptsPerLand
	// is Attempts divided by the landed changes, so the attempts wasted on
	// failed changes raise it. MaxAttempts is the largest number of attempts
	// of any change, landed or failed.
	ObservedConflictRate float64 `json:"observed_conflict_rate"`
	AttemptsPerLand      float64 `json:"attempts_per_land"`
	MaxAttempts          int32   `json:"max_attempts"`
	// AttemptsHistogram counts the landed changes by their number of server
	// attempts (key: attempts, value: changes). Landed changes that report no
	// attempts are not counted.
	AttemptsHistogram map[int]int `json:"attempts_histogram,omitempty"`
	// FailedAttemptsHistogram counts the failed changes that report attempts
	// by their number of attempts, such as the lands that gave up after
	// max_attempts rounds.
	FailedAttemptsHistogram map[int]int `json:"failed_attempts_histogram,omitempty"`

	// Batching, over the landed changes that report a batch. BatchSizeMax is
	// zero when no land was batched. BatchSizeHistogram counts the landed
	// changes by the size of the batch they landed in (key: size, value:
	// changes); an unbatched server reports size 1.
	BatchSizeMean      float64     `json:"batch_size_mean"`
	BatchSizeMax       int         `json:"batch_size_max"`
	BatchSizeHistogram map[int]int `json:"batch_size_histogram,omitempty"`

	DurationSeconds float64 `json:"duration_seconds"`
	LandsPerMinute  float64 `json:"lands_per_minute"`
	// P50MS, P95MS and P99MS are the latency percentiles of the landed changes.
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
	// FailedP95MS is the p95 latency of the failed changes, zero if none failed.
	FailedP95MS float64 `json:"failed_p95_ms"`

	// FailureClasses counts the failed changes by the kind of their failure
	// message, with the object IDs removed (see errorKind).
	FailureClasses map[string]int `json:"failure_classes,omitempty"`
	// Errors holds the first distinct failure messages, for diagnosis.
	Errors []string `json:"errors,omitempty"`

	// Repos is the number of repositories of a multi-node run and Nodes the
	// result of each node (see runMultiNode). Both are empty for a single-node run.
	Repos int          `json:"repos,omitempty"`
	Nodes []NodeResult `json:"nodes,omitempty"`
}

// maxReportedErrors bounds Result.Errors.
const maxReportedErrors = 5

// Run executes the benchmark against be and aggregates the samples.
// Changes are committed concurrently by every agent. Latency runs from the
// start of the commit to the end of the land, so queue waiting is included.
func Run(ctx context.Context, cfg Config, be Backend) (Result, error) {
	if err := cfg.Validate(); err != nil {
		return Result{}, fmt.Errorf("invalid config: %w", err)
	}

	ex, err := execute(ctx, cfg, be)
	if err != nil {
		return Result{}, err
	}

	return ex.summarize(ctx, cfg)
}

// execution is the raw outcome of a run: every sample and the wall time of the
// run.
type execution struct {
	samples []Sample
	elapsed time.Duration
}

// summarize aggregates the samples of the run. It returns an error when ctx
// ended before every change had run.
func (ex execution) summarize(ctx context.Context, cfg Config) (Result, error) {
	res := aggregate(cfg, ex.samples, ex.elapsed)
	if ctx.Err() != nil {
		// The run was interrupted or timed out. The samples are returned, but
		// the run is not a complete measurement even when every change has a
		// sample: a change in flight when the context ended is recorded as a
		// failure, so the samples may be cut short.
		total := cfg.Agents * cfg.ChangesPerAgent
		return res, fmt.Errorf("run stopped after %d of %d changes: %w", len(ex.samples), total, ctx.Err())
	}

	return res, nil
}

// execute prepares the branches of cfg on be and runs every agent of cfg. The
// config must be valid. It fails only when a branch cannot be prepared.
func execute(ctx context.Context, cfg Config, be Backend) (execution, error) {
	bases := make(map[string]string, cfg.BranchCount())
	for i := 0; i < cfg.BranchCount(); i++ {
		branch := cfg.BranchName(i)
		branchCfg := cfg
		branchCfg.Branch = branch
		base, err := be.Prepare(ctx, branchCfg)
		if err != nil {
			return execution{}, fmt.Errorf("prepare branch %s: %w", branch, err)
		}
		bases[branch] = base
	}

	var (
		mu      sync.Mutex
		samples = make([]Sample, 0, cfg.Agents*cfg.ChangesPerAgent)
		// queueMu serializes landing in queue mode, across all branches.
		queueMu sync.Mutex
		wg      sync.WaitGroup
	)

	start := time.Now()
	for agent := 0; agent < cfg.Agents; agent++ {
		wg.Add(1)
		go func(agent int) {
			defer wg.Done()
			for _, c := range plan(cfg, agent) {
				if ctx.Err() != nil {
					return
				}
				s := runChange(ctx, cfg, be, c, bases[c.Target], &queueMu)

				mu.Lock()
				samples = append(samples, s)
				mu.Unlock()
			}
		}(agent)
	}
	wg.Wait()

	return execution{samples: samples, elapsed: time.Since(start)}, nil
}

func runChange(ctx context.Context, cfg Config, be Backend, c Change, base string, queueMu *sync.Mutex) Sample {
	began := time.Now()

	commit, err := be.Commit(ctx, c, base)
	if err != nil {
		return Sample{Latency: time.Since(began), Err: fmt.Errorf("commit: %w", err), Agent: c.Agent}
	}

	if cfg.Mode == ModeQueue {
		queueMu.Lock()
		defer queueMu.Unlock()
	}

	outcome, err := be.Land(ctx, cfg, c, commit)
	if cfg.Mode == ModeQueue && cfg.TestDuration > 0 && err == nil {
		// The simulated test runs while the queue is still held.
		timer := time.NewTimer(cfg.TestDuration)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}

	s := Sample{Latency: time.Since(began), Outcome: outcome, Agent: c.Agent}
	switch {
	case err != nil:
		s.Err = fmt.Errorf("land: %w", err)
		s.Conflict = outcome.Conflict
	default:
		s.Landed = true
	}

	return s
}

func aggregate(cfg Config, samples []Sample, elapsed time.Duration) Result {
	res := Result{
		Mode:            cfg.Mode,
		Agents:          cfg.Agents,
		ChangesPerAgent: cfg.ChangesPerAgent,
		Branches:        cfg.BranchCount(),
		ConflictRate:    cfg.ConflictRate,
		TestDurationMS:  float64(cfg.TestDuration) / float64(time.Millisecond),
		Changes:         len(samples),
		DurationSeconds: elapsed.Seconds(),
	}

	var landedMS, failedMS []float64
	var batchSum, batchCount int
	seen := map[string]bool{}
	for _, s := range samples {
		ms := float64(s.Latency) / float64(time.Millisecond)
		res.Attempts += int64(s.Outcome.Attempts)
		res.Resolutions += s.Outcome.Resolved
		if s.Outcome.Attempts > res.MaxAttempts {
			res.MaxAttempts = s.Outcome.Attempts
		}

		if s.Landed {
			res.Landed++
			if s.Outcome.AlreadyLanded {
				res.AlreadyLanded++
			}
			landedMS = append(landedMS, ms)
			if n := s.Outcome.Attempts; n > 0 {
				addCount(&res.AttemptsHistogram, int(n))
			}
			if size := s.Outcome.BatchSize; size > 0 {
				batchSum += size
				batchCount++
				addCount(&res.BatchSizeHistogram, size)
				if size > res.BatchSizeMax {
					res.BatchSizeMax = size
				}
			}
			continue
		}

		res.Failed++
		failedMS = append(failedMS, ms)
		if n := s.Outcome.Attempts; n > 0 {
			addCount(&res.FailedAttemptsHistogram, int(n))
		}
		if s.Conflict {
			res.Conflicts++
		}
		if s.Err != nil {
			// Messages differ by commit ID, so distinct kinds are compared
			// with the IDs removed.
			key := errorKind(s.Err.Error())
			addStringCount(&res.FailureClasses, key)
			if len(res.Errors) < maxReportedErrors && !seen[key] {
				seen[key] = true
				res.Errors = append(res.Errors, s.Err.Error())
			}
		}
	}

	if elapsed > 0 {
		res.LandsPerMinute = float64(res.Landed) / elapsed.Minutes()
	}
	if res.Changes > 0 {
		res.ObservedConflictRate = float64(res.Conflicts) / float64(res.Changes)
	}
	if res.Landed > 0 {
		res.AttemptsPerLand = float64(res.Attempts) / float64(res.Landed)
	}
	if batchCount > 0 {
		res.BatchSizeMean = float64(batchSum) / float64(batchCount)
	}
	res.P50MS = percentile(landedMS, 50)
	res.P95MS = percentile(landedMS, 95)
	res.P99MS = percentile(landedMS, 99)
	res.FailedP95MS = percentile(failedMS, 95)

	return res
}

// objectIDPattern matches a SHA-1 or SHA-256 object ID as a whole word.
var objectIDPattern = regexp.MustCompile(`\b[0-9a-f]{40}(?:[0-9a-f]{24})?\b`)

// errorKind returns msg without the object IDs it names, so that the same
// failure on different commits has one kind.
func errorKind(msg string) string {
	return objectIDPattern.ReplaceAllString(msg, "<oid>")
}

// addCount increments the count of key in *counts, creating the map if needed.
func addCount(counts *map[int]int, key int) {
	if *counts == nil {
		*counts = map[int]int{}
	}
	(*counts)[key]++
}

// addStringCount is addCount for string keys.
func addStringCount(counts *map[string]int, key string) {
	if *counts == nil {
		*counts = map[string]int{}
	}
	(*counts)[key]++
}

// formatHistogram renders a histogram as "key: count" pairs in key order, or
// "-" if it is empty.
func formatHistogram(h map[int]int) string {
	if len(h) == 0 {
		return "-"
	}

	parts := make([]string, 0, len(h))
	for _, k := range sortedKeys(h) {
		parts = append(parts, fmt.Sprintf("%d: %d", k, h[k]))
	}

	return strings.Join(parts, ", ")
}

// sortedKeys returns the keys of m in ascending order.
func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	return keys
}

// percentile returns the nearest-rank p-th percentile of values, or 0 if
// there are none. values is not modified.
//
// The nearest rank of p percent of n values is ceil(p*n/100): the smallest
// value such that at least p percent of the values are less than or equal to
// it. For whole percentages p*n is exact, so the ceiling has no rounding error.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	rank := int(math.Ceil(p*float64(len(sorted))/100)) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}

	return sorted[rank]
}
