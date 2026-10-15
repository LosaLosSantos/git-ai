# Graft — phase 4a plan: batch latency (BINDING, detailed)

Phases 1–3: doc/graft/PLAN.md, PLAN2.md, PLAN3.md (all "Environment and
commands" rules apply). Worktrees: WP31 … WP40. **Implement exactly the
signatures below. Do not invent alternative designs. If something is
impossible, stop and report it in `needs_outside_change`.**

Module path: `github.com/feder-cr/open_scion` (NOT v16). Test helpers:
`testhelper.Context(t)`, `testcfg.Build(t)`, `testserver.RunGitalyServer(t, cfg, setup.RegisterAll, ...)`,
`gittest.CreateRepository`, `gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(...), gittest.WithTreeEntries(...))`,
`gittest.Exec(t, cfg, "-C", repoPath, ...)`, `gittest.DefaultObjectHash`. In
tests that call operation RPCs, use a ctx with
`featureflag.OutgoingCtxWithFeatureFlag(ctx, featureflag.GPGSigning, false)`.

## Why (measured, phase-3 final)
| metric | off-20 | on-20 | off-50 | on-50 | off-100 | on-100 |
|---|---|---|---|---|---|---|
| attempts/landed | 10.97 | 1.00 | 29.35 | 1.00 | 56.70 | 1.00 |
| p50 landed (ms) | 954 | 3320 | 5484 | 8152 | 5703 | 15950 |
| p99 landed (ms) | 5049 | 9868 | 17669 | 15317 | 16775 | 21808 |

Contention is solved; latency is not. Today every batch member is rebased
with `localrepo.Rebase`: `merge-base` + `rev-list` + per commit
`merge-tree` + `commit-tree` (+ `cat-file` reads) — 4+ git processes per
member, serially, while the lane runs one batch at a time.

## Acceptance (phase 4a)
`examples/graft/demo/bench-batch.sh` at 20/50/100 agents × 5 changes,
conflict rate 0.2, seed 1, batching on: attempts/landed = 1.00, failures =
planned conflicts only, and p99(on) ≤ p99(off) at every size (stretch:
p99(on-100) ≤ 0.5 × p99(off-100)). If not met, phase 4b (pipelined batches)
follows; it is out of scope here.

## Foundation (done by Opus; read-only)
Config (`internal/gitaly/config/config_graft.go`):
- `GraftLand.GraphAncestry bool` (TOML `[graft.land] graph_ancestry`): use
  the stored graph index for ancestry/merge-base in UserLand (single and
  batched). Default false.
- `GraftLandBatch.TargetLatency string` (TOML `target_latency`, Go
  duration): adaptive batch sizing target; empty = `DefaultGraftLandBatchTargetLatency`
  (500ms); `"0s"` disables adaptation. Helper `TargetLatencyOrDefault() (time.Duration, error)`.
- `GraftDerived.CommitGraph bool` (TOML `[graft.derived] commit_graph`):
  write the commit-graph after committed updates. Default false (opt-in).

## Work packages

### WP31 — batch phase instrumentation
Owns: new `internal/gitaly/service/operations/land_batch_metrics.go` (+ test),
the timing calls inside `land_batch_server.go` (`executeLandBatch`, and the
stepper's `Tip`, `Land`, `Update`), `examples/graft/demo/bench-batch.sh`
(metrics scrape + breakdown).
- Histogram `graft_land_batch_phase_seconds{phase}` with phases
  `quarantine` (newStepper), `tip`, `land_member` (one observation per
  member), `update` (CAS + hooks), `total` (whole executeLandBatch).
  Registered on the same registry as the other graft metrics (see how
  `landlane.Shared` is registered in serve.go and setup).
- Counter `graft_land_batch_members_total{outcome}` with outcome in
  `landed`, `already_landed`, `conflict`, `denied`, `error`.
- bench-batch.sh: give each server `prometheus_listen_addr = 127.0.0.1:<port>`;
  after each run, `curl` the metrics and append to the Markdown report a
  table "time per phase" (sum and mean of each phase, and mean land_member
  per batch).
- Tests: metrics observed for a 3-member batch through testserver (read
  the histogram via `prometheus/testutil`).

### WP32 — streaming merge-tree
Owns: new `internal/git/localrepo/merge_tree_stream.go` (+ test).
```go
// MergeTreeStream runs many merges through one long-lived
// `git merge-tree --stdin --write-tree -z --allow-unrelated-histories --name-only=false --messages`
// process (verify the exact flag set accepted with --stdin; drop flags it rejects).
type MergeTreeStream struct{ /* unexported */ }
func (repo *Repo) NewMergeTreeStream(ctx context.Context) (*MergeTreeStream, error)
// Merge merges theirs into ours with base as merge base ("" = let git compute it).
// A clean merge returns the tree. A conflicting merge returns the
// conflict as *MergeTreeConflictError (same type and same contents as
// (*Repo).MergeTree returns for the same inputs) and no error otherwise.
func (s *MergeTreeStream) Merge(ctx context.Context, base, ours, theirs git.ObjectID) (git.ObjectID, error)
func (s *MergeTreeStream) Close() error
```
Input line format with --stdin: `[<base> -- ]<ours> <theirs>`; parse the
NUL-separated output per merge exactly like `parseMergeTreeError` does
(reuse it). Merges must see objects written by other processes after the
stream started (loose objects): test it. Equivalence tests: for clean,
conflicting, unrelated-history, rename and mode-change merges, `Merge`
returns exactly what `MergeTree` returns (tree OID or conflict files and
info messages). Concurrency: one stream is used by one goroutine (document);
`Close` kills and reaps the process; ctx cancellation ends it.

### WP33 — streaming commit writer
Owns: new `internal/git/localrepo/commit_writer.go` (+ test).
```go
// CommitWriter writes commit objects through one long-lived
// `git hash-object -t commit -w --stdin-paths --literally` process (or
// `--stdin-paths` without --literally if it is rejected), formatting the
// commit object in Go.
type CommitWriter struct{ /* unexported */ }
func (repo *Repo) NewCommitWriter(ctx context.Context) (*CommitWriter, error)
// Write returns the same object ID that (*Repo).WriteCommit returns for the
// same WriteCommitConfig, for every config WriteCommit supports without
// signing. If cfg requires signing (GPG/SSH signing configured in the
// repository or the server, see how WriteCommit signs), Write returns
// ErrCommitWriterUnsupported and callers fall back to WriteCommit.
func (w *CommitWriter) Write(ctx context.Context, cfg WriteCommitConfig) (git.ObjectID, error)
func (w *CommitWriter) Close() error
var ErrCommitWriterUnsupported error
```
Formatting must match `git commit-tree` byte for byte: `tree`, `parent`
lines, `author Name <email> <unix> <±hhmm>`, `committer ...`, optional
`encoding`, blank line, message exactly as WriteCommit passes it (check
how WriteCommit handles trailing newlines and message cleanup — commit-tree
does NO cleanup). Temp files for --stdin-paths live in the repo's
quarantine/temp dir and are removed after each write. Equivalence tests: 50
random configs (unicode names, timezones ±, multiple parents, empty
message, message without trailing newline, trailers) → identical OIDs to
WriteCommit; objects readable via ReadCommit.

### WP34 — graft rebase engine
Owns: new package `internal/graft/rebase/**`. Depends on WP32, WP33.
```go
package rebase
// Engine rebases commits within one batch attempt using one merge-tree
// stream and one commit writer (started lazily, closed by Close).
type Engine struct{ /* unexported */ }
func New(repo *localrepo.Repo) *Engine
// Rebase has EXACTLY the semantics and errors of
// (*localrepo.Repo).Rebase(ctx, upstream.String(), branch.String(), opts...):
// fast-forward detection, todo list via `rev-list --cherry-pick --right-only
// --no-merges --topo-order --reverse upstream...branch` (unless
// RebaseWithReappliedCherryPicks), empty-commit rules, committer option,
// *localrepo.RebaseConflictError on conflicts, full commit messages.
func (e *Engine) Rebase(ctx context.Context, upstream, branch git.ObjectID, opts ...localrepo.RebaseOption) (git.ObjectID, error)
func (e *Engine) Close() error
```
Export from localrepo whatever option accessors are needed (a small new
file `internal/git/localrepo/rebase_options_export.go` owned by WP34).
Fallback: if `CommitWriter.Write` returns ErrCommitWriterUnsupported, use
`repo.WriteCommit` for that commit. Equivalence tests: on random histories
(seeded, with conflicts, empty commits, cherry-picks, unrelated
histories, merges in range), `Engine.Rebase` and `localrepo.Rebase` return
the same OID / the same error type and conflict contents, with a fixed
committer. Benchmark `BenchmarkRebaseEngine` vs `BenchmarkLocalrepoRebase`
on a 40-member chain: report process count and time.

### WP35 — executor integration + early completion + batch of one
Owns: `internal/gitaly/service/operations/land_batch.go`,
`land_batch_server.go` (except WP31's timing calls, keep them), their tests.
Depends on WP31, WP34, WP36, WP37.
- One `rebase.Engine` per batch attempt (created in `newStepper`, closed in
  its cleanup); `computeLandedTip`'s rebase path in the batch stepper uses
  `engine.Rebase` instead of `quarantineRepo.Rebase` (single-land path in
  land.go is NOT changed in this WP).
- Ancestry checks in the stepper use `ancestry.Checker` (WP36) built from
  config (`GraphAncestry`).
- Early completion: requests whose outcome is final before the CAS
  (InvalidArgument, policy denial, unresolved conflict, already landed
  **only if** the batch is not going to rewrite history before them — an
  already-landed answer is final immediately because it does not depend on
  the CAS) are completed with `landlane.CompleteEarly(ctx, index, result)`
  (WP37) as soon as they are decided.
- Batch of one: when exactly one member changes the branch, the update
  uses that member's own identity ctx (per-request identity rule of
  PLAN3), operation `land` (not `land_batch`), and no members, so the
  changeset is a normal land with the real actor. The policy skip of
  `land_batch` therefore does not apply and the member's policy is checked
  by the hooks as for an unbatched land (it was also admitted in-process).
- Tests: everything in land_batch_test.go keeps passing; new tests: engine
  used (count via a test hook), early completion returns before a slow
  update (inject a slow update via the stepper interface), batch-of-one
  changeset operation `land` with the member's actor, graph ancestry on/off
  identical outcomes.

### WP36 — ancestry checker
Owns: new package `internal/graft/ancestry/**`, the ancestry call sites in
`internal/gitaly/service/operations/land.go` (single-land path only).
```go
package ancestry
type Checker interface {
    IsAncestor(ctx context.Context, ancestor, descendant git.ObjectID) (bool, error)
    MergeBase(ctx context.Context, a, b git.ObjectID) (git.ObjectID, bool, error)
}
// Git uses the repository's git (current behaviour).
func Git(repo *localrepo.Repo) Checker
// Graph uses the stored graph index (internal/graft/derived/graph) with the
// derived store of repo; it must return exactly what Git returns.
func Graph(ctx context.Context, repo *localrepo.Repo, reader catfile.ObjectContentReader, store *derived.Store) (Checker, error)
// New returns Graph when enabled (falling back to Git on any construction
// error, logged), else Git.
func New(ctx context.Context, enabled bool, repo *localrepo.Repo, reader catfile.ObjectContentReader, store *derived.Store, logger log.Logger) Checker
```
land.go: `landAttempt` uses `ancestry.New(..., s.graft.Land.GraphAncestry, ...)`
for its IsAncestor checks. Tests: equivalence Git vs Graph on seeded random
DAGs (≥ 200 commits, merges, criss-cross) for every sampled pair; UserLand
outcomes identical with the flag on and off.

### WP37 — landlane: adaptive sizing + early completion
Owns: `internal/graft/landlane/**`.
- `Config.TargetLatency time.Duration` (0 = no adaptation), set by
  `ConfigFromGraft` from `TargetLatencyOrDefault()`. Per lane, the effective
  max size starts at `MaxSize`; after each batch: if its executor duration >
  TargetLatency, effective = max(1, effective/2); if < TargetLatency/2,
  effective = min(MaxSize, effective+max(1, effective/4)). Gauge
  `graft_land_lane_effective_max_size` (summed over lanes).
- Early completion:
```go
// CompleteEarly delivers the result of reqs[index] of the running batch now;
// the executor's later result for that index is ignored. It returns false if
// ctx is not an executor context of this package or index is out of range.
func CompleteEarly[Res any](ctx context.Context, index int, r Result[Res]) bool
```
  The submitter of an early-completed request returns immediately with
  `Response{Value, BatchSize, BatchPosition, Wait}` (BatchSize = size of the
  batch, as for the others).
- Tests: adaptation halves on slow batches and grows back on fast ones,
  bounded; disabled at 0; CompleteEarly delivers before the executor returns,
  later result ignored, wrong ctx/index returns false, concurrency-safe.

### WP38 — upstream defaults
Owns: the eager/commit-graph construction block in
`internal/cli/gitaly/serve.go`, `internal/graft/eager/**` (options only),
tests in `internal/cli/gitaly`.
- The commit-graph writer is enabled only when
  `cfg.Graft.Derived.CommitGraph && !cfg.Graft.Derived.Disabled`; eager
  warming only when `cfg.Graft.Derived.Eager && !Disabled` (unchanged).
  A configuration without any [graft] section therefore starts no eager
  observer work and writes no commit-graph outside housekeeping.
- Tests: plain config → observer disabled (no writes, no enqueues); config
  with commit_graph = true → writes; eager only → enqueues but no
  commit-graph writes.

### WP39 — phase-4 e2e + chaos harness
Owns: `internal/graft/e2e/phase4_test.go`, new `internal/graft/e2e/chaos_test.go`.
- Phase-4 e2e: batched lands with GraphAncestry on produce identical
  results to off; adaptive sizing visible (effective max size gauge
  changes under a slow update hook); early completion (a policy-denied
  request in a batch with a slow update returns before the others);
  batch-of-one changeset is `land` with the real actor.
- Chaos (`TestChaosLanding`, skipped with `-short`, seeded, ~60 s budget):
  N agents (20) land random changes (disjoint files, overlapping files with
  resolve on/off) on 2 protected and 1 open branch, while an external writer
  occasionally updates a branch with UserUpdateBranch and some requests are
  cancelled; then assert invariants: every success's landed commit is
  reachable from its branch tip; histories are linear on rebase lanes; no
  file change of a successful request is missing from the final tree; every
  failure has a correct code (conflict/denied/canceled/kept moving); the
  changeset log covers every successful land (via members or actor) exactly
  once; no request both succeeded and failed.

### WP40 — docs + acceptance run
Owns: `doc/graft/{LAND-BATCHING,DERIVED,BENCH,LIMITATIONS,CONFIGURATION}.md`,
README phase-4 section, new `examples/graft/demo/acceptance-phase4.sh`.
Depends on all.
- acceptance-phase4.sh runs bench-batch.sh, parses its JSON outputs, prints
  PASS/FAIL for each acceptance criterion above, exits non-zero on FAIL.
- Docs from the real code: new config keys (graph_ancestry,
  target_latency, commit_graph), metrics, phase breakdown, the measured
  results table (from the integration run), limitations.

## Spec-test writers
Write `*_spec_test.go` files in the same package as the code, module path
`github.com/feder-cr/open_scion`, using only exported identifiers named in
this spec plus the helpers listed at the top. Every test function starts
with `TestSpec`.

## WP33 RULE: FALL BACK INSTEAD OF APPROXIMATING (BINDING, added by Opus after gate 2)
CommitWriter must never return an OID different from WriteCommit. For any
input whose byte-exact equivalence it cannot guarantee — including (but not
limited to) i18n.commitEncoding / commit encoding settings different from
UTF-8, messages or names/emails containing invalid UTF-8, Unicode
noncharacters, NUL, CR, or any character WriteCommit or git commit-tree
transforms — Write returns ErrCommitWriterUnsupported BEFORE writing
anything, and the caller falls back to WriteCommit. Detection must be
conservative (when in doubt, fall back). Add a test table of such inputs
asserting ErrCommitWriterUnsupported, and keep the 50-config equivalence test
for ordinary inputs. A failed or rejected Write must leave the writer usable
for the next Write (restart the hash-object process if it died).

## WP35 RULE: EARLY COMPLETION ONLY FOR MEMBER-INDEPENDENT OUTCOMES (BINDING, added by Opus after gate 1)
An outcome may be delivered early (landlane.CompleteEarly) only if it cannot
change whatever happens to the other members of the batch (their landing,
the CAS, a rebuild, or the per-member fallback after a hook rejection):
1. Policy denial from in-process admission (depends only on actor and ref).
2. Request validation errors that do not depend on the tip (commit ID
   unparsable or not a commit).
3. Already landed relative to the batch's STARTING tip (reachable from the
   tip read before any member was applied) — never relative to a running tip
   that contains unpublished members.
4. A merge conflict or any tip-dependent error ONLY when no earlier member of
   the same attempt changed the running tip (running tip == starting tip).
   Otherwise the request is NOT final: it stays in the batch and is
   re-evaluated by later attempts, rebuilds and the fallback, exactly as
   before early completion existed.
Early-delivered responses report BatchSize/BatchPosition with the same
definition as every other response of that batch. Add regression tests for
the gate's repro (conflict against an unpublished member that is later
rejected → the request must land or get the correct final outcome, not an
early conflict).

## WP35 RULE: BATCH OF ONE KEEPS ITS MEMBER (BINDING, amends the WP35 section)
A batch in which exactly one member changes the branch is recorded with
operation `land` and the member's real actor (its own identity ctx), AND
carries that single member (original commit → landed commit), so the
original→landed mapping is never lost. The reference-transaction policy
skip still applies ONLY to `land_batch` with members, so this `land` is
policy-checked as any unbatched land. The e2e helper landBatchMembers is
updated by Opus to read members from `land_batch` changesets and from
`land` changesets carrying exactly one member.
