# Graft — phase 3 plan (BINDING, detailed)

Phases 1–2: doc/graft/PLAN.md, doc/graft/PLAN2.md (all "Environment and
commands" rules apply). Phase 3 = (A) batched landing, (B) derived-data
framework. Worktrees: WP21 … WP30. **Implement exactly the signatures below.
Do not invent alternative designs. If something in this spec is impossible,
stop and report it in `needs_outside_change` instead of improvising.**

Measured motivation (phase-2 graft-bench, 20 agents, 1 branch): 1,019
attempts for 100 changes, p99 26 s, failures at max_attempts=20. Goal:
~1 attempt per change, p99 an order of magnitude lower.

## Foundation (done by Opus before WPs start; read-only for WPs)

Proto (`proto/operations.proto`, `proto/changeset.proto`, generated):
- `UserLandResponse.batch_size` (int32, 9): number of requests in the batch
  this request landed in (1 when unbatched).
- `UserLandResponse.batch_position` (int32, 10): 1-based position.
- `message ChangesetMember { string original_commit_id = 1; string commit_id = 2;
  string actor = 3; bool rewritten = 4; repeated bytes resolved_paths = 5; }`
- `Changeset.members` (repeated ChangesetMember, 9): set for batched lands.

Config (`internal/gitaly/config/config_graft.go`):
- `Graft.Land.Batch GraftLandBatch{ Disabled bool; Window string; MaxSize int }`
  TOML `[graft.land.batch] disabled/window/max_size`; helpers
  `WindowOrDefault() (time.Duration, error)` (default 10ms),
  `MaxSizeOrDefault() int` (default 50); validated.
- `Graft.Derived.Workers int` (default 2, `WorkersOrDefault()`),
  `Graft.Derived.QueueSize int` (default 10000, `QueueSizeOrDefault()`).

## A. Batched landing

### WP21 — `internal/graft/landlane` (pure Go, no git, no gRPC)
Files: `internal/graft/landlane/{lanes.go,metrics.go,lanes_test.go}`.
```go
package landlane

// Key identifies a lane: one per branch of one repository.
type Key struct{ Storage, RelativePath, Branch string }

// Item is one queued request handed to the executor.
type Item[Req, Resp any] struct { /* unexported */ }
func (it *Item[Req, Resp]) Context() context.Context // the submitter's ctx
func (it *Item[Req, Resp]) Request() Req
// Complete delivers the result. Only the first call has an effect.
func (it *Item[Req, Resp]) Complete(resp Resp, err error)

// Executor runs one batch. It must call Complete on every item; items it
// leaves incomplete are completed with ErrNotCompleted after it returns.
// ctx is a lane context that is NOT cancelled when a submitter's ctx is.
type Executor[Req, Resp any] func(ctx context.Context, key Key, batch []*Item[Req, Resp])

type Config struct {
    Window  time.Duration // collect requests this long after the first one (0 = no wait)
    MaxSize int           // max items per batch (>= 1)
}

type Lanes[Req, Resp any] struct { /* unexported */ }
func New[Req, Resp any](cfg Config, exec Executor[Req, Resp]) *Lanes[Req, Resp]
// Submit enqueues req on key's lane and blocks until the item is completed
// or ctx is done. If ctx is done before the item was handed to the executor,
// the item is removed and ctx.Err() returned. If ctx is done after hand-off,
// ctx.Err() is returned immediately and the eventual result is discarded.
func (l *Lanes[Req, Resp]) Submit(ctx context.Context, key Key, req Req) (Resp, error)
// Close stops accepting (Submit returns ErrClosed), waits for running
// batches, and fails queued items with ErrClosed.
func (l *Lanes[Req, Resp]) Close()
// Describe/Collect implement prometheus.Collector:
// graft_land_lane_queue_depth (gauge, summed), graft_land_batch_size
// (histogram), graft_land_lane_wait_seconds (histogram, submit→hand-off).
var ErrNotCompleted, ErrClosed error
```
Semantics: FIFO per key; at most one batch per key executing at a time;
different keys fully parallel; a lane goroutine exists only while its key
has work (no leak; test with `goleak`-style counting or a running-lanes
counter exposed for tests via an unexported helper); batch formation =
wait `Window` after the first queued item or until `MaxSize` queued, then
take up to `MaxSize` items whose ctx is not done; executor panics are
recovered and every item of that batch gets an error wrapping the panic.
Tests (all with `-race`-safe code, though we cannot run -race): FIFO order,
max size split, window grouping (use small windows, no flaky sleeps: assert
on batch composition with a controllable executor), parallel keys,
cancellation before/after hand-off, Close, panic recovery, executor that
forgets items, no goroutine leak after idle, metrics registered.

### WP22 — batch executor in operations
Files: `internal/gitaly/service/operations/land_batch.go`,
`land_batch_test.go`, edit `land.go` (only `UserLand` entry dispatch),
`server.go` (add lanes field + construction), new
`internal/gitaly/hook/graft_policy_export.go`.
- `hook.EvaluateGraftPolicy(cfg config.Graft, actor string, reference, oldOID, newOID string) (allowed bool, message string, err error)`
  — exported wrapper over the existing `graftPolicyDenial` (same semantics).
- `Server` gets `lanes *landlane.Lanes[*landBatchRequest, *gitalypb.UserLandResponse]`,
  created in `NewServer` from `deps.GetCfg().Graft.Land.Batch` (nil when
  disabled). `landBatchRequest{in *gitalypb.UserLandRequest; committer git.Signature; actor string}`.
- `UserLand` dispatch: after validation, use the lane iff ALL hold:
  lanes != nil, `cfg.Graft.Standalone`, not MVCC (`mvcc.CacheFromContext(ctx) == nil`),
  the branch exists (resolve it; a missing branch goes unbatched). Key =
  {storage, relative path, branch}. Otherwise the existing retry loop.
- Executor `func (s *Server) executeLandBatch(ctx context.Context, key landlane.Key, batch []*landlane.Item[...])`:
  1. Drop items whose ctx is done (Complete with `structerr.NewCanceled`).
  2. One quarantine (`s.quarantinedRepo(ctx, batch[0].in.GetRepository())`).
  3. `tip` = current branch; `running := tip`; for each item in order:
     resolve `commitID` (same peeling as landAttempt; invalid → Complete
     InvalidArgument, continue); policy: `hook.EvaluateGraftPolicy(cfg, item.actor, ref, running, candidate)`
     evaluated on the candidate new tip (denied → Complete PermissionDenied
     with `UserLandError_AccessCheck`, continue); if commit reachable from
     `running` → member result already_landed (no change); if `running` is
     ancestor of commit → candidate = commit (fast-forward); else
     `s.computeLandedTip(ctx, item.in, quarantineRepo, running, commitID, item.committer)`;
     on merge conflict with `resolve_conflicts` → `s.resolveLandConflicts(...)`
     against `running`; any error → Complete(err) for that item only;
     success → `running = candidate`, record member {original commitID,
     landed candidate, actor, rewritten, resolved paths, old_tip = previous running}.
  4. If no member changed the branch: complete members (already_landed or
     unchanged) and return.
  5. One update: ctx2 = `changelog.WithBatch(changelog.WithOperation(ctx, "land_batch"), members)`
     (WP23 helper; until WP23 merges, call a local shim), rewritten =
     any member rewritten; `s.updateLandedReference(ctx2, batchIn, quarantineDir, quarantineRepo, ref, tip, running, rewritten)`
     where `batchIn` is the first item's request with `User` replaced by
     `&gitalypb.User{GlId: "graft-batch", GlUsername: "graft-batch", Name: []byte("Graft"), Email: []byte("graft@localhost")}`.
  6. landConflict (external writer moved the branch) → rebuild the whole
     batch from step 2 on the new tip, at most 10 times, then fail every
     remaining item with the existing `landKeptMovingError`.
  7. PermissionDenied from hooks for the combined update → land each member
     alone with the existing unbatched loop (`s.landWithRetries`, extract the
     current loop body of UserLand into this function) using the item's own
     ctx/user, completing each item with its own result.
  8. Success → for each member: `landResponse(member.oldTip, member.commit, ...)`
     + `BatchSize = len(members+already-landed items)`, `BatchPosition` =
     1-based index among the batch items, `Attempts` = number of batch
     rebuilds + 1, `ResolvedPaths`/`Resolver` from its resolution.
- Tests (`land_batch_test.go`, testserver standalone config with batching
  window 50ms): 30 goroutines landing disjoint files on one branch → all
  succeed, `max(batch_size) > 1`, total attempts ≤ 2×requests, linear
  history with all files; conflicting member fails alone while others land;
  resolve_conflicts member resolved inside a batch (fake resolver command);
  policy-denied member rejected alone; external writer moving the branch
  mid-batch (use a hook or a concurrent UserUpdateBranch) → batch rebuilt;
  custom pre-receive hook rejecting → fallback attributes the rejection;
  cancelled submitter before hand-off; batching disabled → old behaviour;
  non-standalone → unbatched; missing branch → unbatched create.

### WP23 — changeset members end to end
Files: `internal/graft/changelog/{context.go,log.go,members.go,members_test.go}`,
`internal/git/gitcmd/hooks_payload.go` (+ test), `internal/gitaly/hook/referencetransaction.go`,
`internal/gitaly/service/changeset/**`, `sdk/changesets*.go`, `cmd/graft` log/watch output.
- `changelog.Member{OriginalCommit, Commit, Actor string; Rewritten bool; ResolvedPaths []string}`
  (JSON keys `original_commit`, `commit`, `actor`, `rewritten`, `resolved_paths`).
- `changelog.WithBatch(ctx, []Member) context.Context`, `BatchFromContext(ctx) []Member`.
- `Entry.Members []Member json:"members,omitempty"`, `Meta.Members []Member`;
  `Append` stores Meta.Members on every entry of that call.
- `gitcmd.ChangelogMeta.Members []changelog.Member` filled from ctx where the
  meta is built; IMPORTANT: `ChangelogMeta` stops being comparable — replace
  every `== ChangelogMeta{}` / `!= (ChangelogMeta{})` with a new
  `func (m ChangelogMeta) IsZero() bool`. Cap the encoded members at 32 KiB:
  drop `ResolvedPaths` first, then truncate the list and set
  `ChangelogMeta.MembersTruncated bool` (and `Entry.MembersTruncated`).
- Reference-transaction hook: pass members to `changelog.Meta`; the graft
  policy check is SKIPPED for updates whose meta has `Operation == "land_batch"`
  and non-empty Members (members were checked in-process by WP22). Document
  why this is safe (meta is produced by the server, agents cannot set git
  config or env).
- ChangesetService maps members to `Changeset.Members`. SDK: nothing extra
  if `Changeset` is the proto alias (verify). CLI: `graft log` prints
  `batch of N` with `actor original→landed` lines; `--json` includes members.
- Tests: JSON round trip (incl. non-UTF-8 safe paths as in existing code),
  truncation, hook recording via a real batched land (after WP22 merges;
  until then a direct ctx test), policy skip only for land_batch with
  members, CLI output.

## B. Derived-data framework

### WP24 — commit kinds framework
Files: `internal/graft/derived/{kinds.go,warmer.go,kinds_test.go,warmer_test.go}`.
Do NOT change the existing lastmod/blame/path-history code paths or their
output (they stay as they are).
```go
// CommitKind is a value computed per commit from the commit and its parents' values.
type CommitKind struct {
    Name    string // e.g. "graph", "changed-paths"
    Version int    // bump to invalidate stored values
    Compute func(ctx context.Context, env *KindEnv, commit *catfile.Commit, parentValues [][]byte) ([]byte, error)
}
type KindEnv struct { /* repo, object reader, deriver */ }
func (e *KindEnv) ReadCommit(ctx context.Context, oid git.ObjectID) (*catfile.Commit, error)
func (e *KindEnv) ChangedPaths(ctx context.Context, parent, commit git.ObjectID) ([]string, error) // diff-tree, no renames, sorted
func RegisterCommitKind(k CommitKind) // panics on duplicate; call from init()
func CommitKinds() []CommitKind        // sorted by name
// CommitValue returns kind's value for commit, computing missing ancestors
// ITERATIVELY (explicit stack, parents before children), storing each value
// with Store key Key(name+"@v"+version, commit, ""). Honors d's cache flag.
func (d *Deriver) CommitValue(ctx context.Context, kind string, commit git.ObjectID) ([]byte, error)
```
Warmer (eager computation):
```go
type Warmer struct{ /* bounded queue + N workers */ }
func NewWarmer(logger log.Logger, workers, queueSize int, open func(ctx context.Context, repo storage.Repository) (*Deriver, func(), error)) *Warmer
func (w *Warmer) Enqueue(repo storage.Repository, commits []git.ObjectID) // non-blocking; drops + counts when full
func (w *Warmer) Close()
// metrics: graft_derived_warm_queue_depth, _dropped_total, _computed_total, _errors_total
```
Tests: a toy kind (e.g. "depth" = 1 + max parent depth) — values correct on
linear, merge and criss-cross DAGs built with gittest; 3,000-commit linear
history without recursion; stored values reused (count Compute calls);
version bump recomputes; warmer computes enqueued commits, drops when full,
Close drains/stops cleanly.

### WP25 — `graph` kind
Files: `internal/graft/derived/graph.go`, `graph_test.go`.
Value: binary, little-endian: `uint32 generation` (root = 1, else 1 + max
parent generation) followed by `uint8 n` and n object IDs (raw bytes of the
repo hash) = first-parent ancestors at distance 2^0..2^(n-1) (skip[0] =
first parent; skip[k] = skip list of skip[k-1] at index k-1 when present).
Registered via `RegisterCommitKind` (Name "graph", Version 1).
```go
func (d *Deriver) Generation(ctx context.Context, c git.ObjectID) (uint32, error)
func (d *Deriver) IsAncestor(ctx context.Context, a, b git.ObjectID) (bool, error) // a ancestor of or equal to b, all parents
func (d *Deriver) MergeBase(ctx context.Context, a, b git.ObjectID) (git.ObjectID, bool, error) // a best common ancestor
```
IsAncestor: if a == b true; prune nodes with generation < gen(a); BFS over
all parents from b with a visited set; use skip pointers to jump along the
first-parent chain while the jumped-to node's generation ≥ gen(a). MergeBase
must return an OID that `git merge-base --is-ancestor` accepts as a common
ancestor and that is a best one (not an ancestor of another common ancestor)
— on criss-cross histories any best base is acceptable; tests compare with
`git merge-base --all`. Tests: seeded random DAGs (≥ 200 commits, merges)
comparing every sampled pair against git; deep linear history (5,000).
Do NOT wire into UserLand in this WP (later decision).

### WP26 — eager triggers + commit-graph refresh
Files: `internal/gitaly/hook/graft_derived_trigger.go` (+ test), the call
site in `referencetransaction.go` (one call, committed state only), the
warmer construction in `internal/cli/gitaly/serve.go` (one block) and its
injection into the hook manager (constructor option; find how the manager
is built in serve.go and setup).
- When `cfg.Graft.Derived.Eager && !cfg.Graft.Derived.Disabled`, after a
  committed reference transaction, for every update whose new OID is a
  commit: list new commits with `git rev-list --max-count=1000 <new> ^<old>`
  (just `<new>` limited to 1000 when old is zero) and `Warmer.Enqueue` them.
  Never block or fail the hook; errors are logged.
- Debounced commit-graph refresh: per repository, at most once per 5 s after
  updates, run `git commit-graph write --reachable --split --changed-paths`
  through the command factory in the background (bounded concurrency 1 per
  repo, global 2). Off when `Derived.Disabled`.
- Tests: hook enqueues expected commits (fake warmer), never blocks when the
  queue is full, debounce collapses bursts (fake clock or injectable runner),
  disabled config does nothing, serve wiring builds the warmer.

### WP27 — `changed-paths` kind + indexed PathHistory
Files: `internal/graft/derived/changed_paths.go`, `path_history_indexed.go`, tests.
- Kind "changed-paths" v1: value = newline-joined sorted list of paths
  changed vs the FIRST parent (root: all paths), each path NUL-free; if more
  than 20,000 paths, the value is the single line `*` (meaning "everything").
- `func (d *Deriver) PathHistoryIndexed(ctx, start git.ObjectID, path string, limit int, fn func(*catfile.Commit) error) error`
  producing EXACTLY the same commits, in the same order, as the existing
  `(*Deriver).PathHistory` for the same arguments (equivalence tests on
  linear, merge, delete/re-add and rename histories, plus directory
  prefixes), using changed-paths to skip commits that cannot match (a path
  matches a changed path if equal or a directory prefix of it, or the value
  is `*`), then confirming candidates with the existing per-commit logic.
- DerivedDataService.PathHistory switches to the indexed version when
  `!cfg.Graft.Derived.Disabled` (edit only that call site in
  `internal/gitaly/service/derived`).
- Benchmark test (`BenchmarkPathHistory`) on a 2,000-commit history showing
  the indexed variant reads fewer trees (count via a hook/counter).

## C. Measurement, product, verification

### WP28 — graft-bench v2
Files: `cmd/graft-bench/**`, `examples/graft/demo/bench-batch.sh`.
- Flags: keep existing; add `--branches N` (agents spread round-robin over
  N branches, default 1), `--label NAME` (printed in reports), `--json-out FILE`,
  `--md-out FILE`. Latency percentiles are computed over LANDED changes
  only; failed changes are reported separately (count + error classes).
  Add per-change `attempts` histogram and server-reported `batch_size`
  distribution (from UserLandResponse) to the report.
- `bench-batch.sh`: builds binaries, starts two standalone gitaly servers
  (batching disabled vs enabled, otherwise identical config, temp dirs,
  unix sockets), seeds the repo, runs graft-bench against each with 20, 50
  and 100 agents × 5 changes (conflict-rate 0.2, seed 1), prints a combined
  Markdown table, and kills both servers (trap EXIT).
- Tests: percentile math (known inputs), report formatting, flag parsing.

### WP29 — SDK/CLI/docs
Files: `sdk/land*.go` (BatchSize/BatchPosition on the land result),
`cmd/graft` land output (`landed <oid> (batch 3/17, 1 attempt)`),
`doc/graft/LAND-BATCHING.md`, `doc/graft/DERIVED.md`, the `[graft.land.batch]`
and new `[graft.derived]` keys in `doc/graft/CONFIGURATION.md`,
`README.md` phase-3 section, `doc/graft/LIMITATIONS.md` update.
Docs must match the code exactly (read WP21–WP27 code before writing).

### WP30 — phase-3 e2e + load
Files: `internal/graft/e2e/phase3_test.go`.
Scenarios (in-process testserver, standalone, batching on): 100 concurrent
agents × 1 change on one branch all land, attempts ≤ 2 per change on
average, some batch_size > 1, changeset log has `land_batch` entries whose
members cover all 100 commits with correct actors; external writer racing
the lane; custom hook rejection fallback; cancellation; eager derived data
present for landed commits (graph value exists without a query having
computed it); indexed PathHistory equals the non-indexed one on the final
history.

## Black-box spec tests (parallel test writers)
For each WP an independent agent writes `*_spec_test.go` tests in the WP's
packages from THIS SPEC ONLY (it must not read the implementation beyond
exported signatures). They are merged together with the WP and must pass;
if a spec test fails, the implementation is wrong unless the gate decides
the test misreads the spec.

## RECONCILIATION WITH THE EARLY DRAFTS (BINDING — supersedes the sections above where they differ)

The drafts were written from a shorter plan. Opus reviewed them; these
decisions are final. Completion agents keep the draft code and adapt it to
exactly this contract.

### WP21 (landlane) — ADOPT the draft API, with these requirements
Keep `Registry[Req, Res]`, `NewRegistry(cfg, exec, obs)`, `Submit(ctx, key, req) (Response[Res], error)`,
`Executor func(ctx, key, reqs []Req) ([]Result[Res], error)`, `Response{Value, BatchSize, BatchPosition, Wait}`.
Must still satisfy every WP21 semantic above (FIFO, one batch per key at a
time, cancellation before/after hand-off exactly as specified, executor ctx
not cancelled by submitters, panic recovery, no goroutine leak, Close with
`ErrClosed`). Add `func (r *Registry[Req, Res]) Close()` if missing. Replace
the `Observer`-only metrics with a `PrometheusObserver` implementing
`prometheus.Collector` with the three metric names above (keep Observer as
the internal hook). `Config` gets `func ConfigFromGraft(c config.GraftLandBatch) (Config, error)`
using `WindowOrDefault`/`MaxSizeOrDefault`.

### WP22 — ADOPT the draft (stepper design), conform to WP21's real API
Delete the local `landLaneSubmitter` shim and use `landlane.Registry`.
Implement everything in the WP22 section that the draft lists as missing:
batch_size/batch_position in responses, lanes constructed in `NewServer`
from config, per-request `max_attempts` honoured for the fallback path,
per-actor policy admission via `hook.EvaluateGraftPolicy` BEFORE a request
enters the running tip, the synthetic `graft-batch` user for the combined
update, `changelog.WithBatch` members (WP23), standalone-only batching.
`BranchUpdate.CommitId` = the request's own landed commit (keep).

### WP23 — the draft follows the spec; finish exactly the WP23 section.

### WP24 — ADOPT the draft refactor (chain walker, cachedJSON, Registry, Pool)
on condition that the existing behaviour is byte-identical: the gate runs
`./internal/graft/derived`, `./internal/gitaly/service/derived`,
`./internal/gitaly/service/commit` and `./internal/graft/e2e` and they must
pass unchanged. INSTEAD of `CommitKind`/`CommitValue`/`Warmer`, the
extension contract for new per-commit kinds is:
```go
// KindStore is a typed view of the Store for one kind and version.
func (s *Store) Kind(name string, version int) KindStore
type KindStore struct{ /* store, name, version */ }
func (k KindStore) Get(commit git.ObjectID) ([]byte, bool, error)
func (k KindStore) Put(commit git.ObjectID, value []byte) error

// WarmEnv is what eager warmers get.
type WarmEnv struct {
    Logger log.Logger
    Repo   *localrepo.Repo
    Reader catfile.ObjectContentReader
    Store  *Store
}
// WarmFunc computes (and stores) a kind for the given commits.
type WarmFunc func(ctx context.Context, env WarmEnv, commits []git.ObjectID) error
// RegisterWarmer registers an eager warmer (called from init() of kind packages).
func RegisterWarmer(name string, fn WarmFunc)
func Warmers() []string // sorted
```
`Pool` (the draft's eager pool) = the Warmer: `NewPool(logger, workers, queueSize, open func(ctx, repo storage.Repository) (WarmEnv, func(), error))`,
`Enqueue(repo storage.Repository, commits []git.ObjectID)` (non-blocking,
counts drops), `Run(ctx)`/`Close()`, runs every registered warmer (filtered
by `[graft.derived] kinds` if set — skip; there is no kinds key: run all),
Prometheus metrics `graft_derived_warm_queue_depth`, `_dropped_total`,
`_computed_total`, `_errors_total`.

### WP25 — ADOPT the subpackage `internal/graft/derived/graph`
Keep `Index` (`Generation`, `IsAncestor`, `MergeBases`, `MergeBase`). Add
persistence: `NewIndex(source, store derived.KindStore)` (kind "graph",
version 1) storing each computed entry (binary encoding of generation +
skip pointers as in the WP25 section) and reading stored entries before
computing; memory map remains as an LRU-bounded cache (max 100k entries).
Register `derived.RegisterWarmer("graph", ...)` in `init()` computing
entries for the given commits. Equivalence tests vs git stay mandatory
(random DAGs ≥ 200 commits + the existing ones).

### WP26 — ADOPT the subpackage `internal/graft/eager`
Wire it: (1) `serve.go` constructs one `derived.Pool` (workers/queue from
config) and one `eager.Observer` (Eager = `cfg.Graft.Derived.Eager && !Disabled`,
CommitGraph = `!Disabled`) with the production `CommitGraphWriter` =
housekeeping's incremental commit-graph write
(`housekeeping.WriteCommitGraph` + `WriteCommitGraphConfigForRepository`
— verify the real names with git grep); (2) the hook manager receives the
observer through a constructor option or setter (follow how serve.go
builds the manager) and calls `ObserveCommitted` right after
`changelog.Append` in the Committed state of `referencetransaction.go`
(even when the changelog is disabled: call it independently of recording);
(3) Enqueue new commits = `git rev-list --max-count=1000 new ^old`
(not only the tips; adapt the draft). Tests: a real land through
testserver produces a commit-graph chain file and warmed graph entries.

### WP27 — ADOPT the subpackage `internal/graft/derived/changedpaths`
Persist summaries via `derived.KindStore` (kind "changed-paths", version 1);
register `derived.RegisterWarmer("changed-paths", ...)`. MANDATORY:
`DerivedDataService.PathHistory` must return exactly what it returns today.
Compare the draft's `PathHistory` with the existing
`(*derived.Deriver).PathHistory` (not only with git log) on the same
histories (linear, merges, delete/re-add, renames, directories); if their
semantics differ (TREESAME parent following vs strict first-parent), the
service must keep the existing semantics: use the changed-paths index only
as a pre-filter inside the existing algorithm (skip commits whose summary
proves the path unchanged vs first parent; confirm everything else with the
existing logic). Switch the service only when the equivalence tests pass.

### WP28, WP29, WP30 — the drafts follow the spec; finish them, replacing
every `TODO(graft-complete)` adapter with the real proto fields/APIs.
WP30 must not shell out to `git` via exec.Command directly: use
`gittest.Exec` / the test command factory like other e2e tests.

## WP22 SECURITY RULE: PER-REQUEST IDENTITY (BINDING, added by Opus after gate 1)

Root cause found by the gate: the lane executor ctx is derived from the FIRST
submitter's ctx (landlane uses context.WithoutCancel(batch[0].ctx)), so any
per-request work done with it runs with the wrong identity. Rules:

1. Every per-request step (commit resolution, policy admission, rebase/merge,
   conflict resolution and the resolver actor allow-list) MUST run with a ctx
   that carries THAT request's identity: build it as
   `reqCtx := identity.NewContext(laneCtx, id)` where `id` is taken from the
   request's own submitter ctx (`identity.FromContext(item ctx)`), or with no
   identity when the submitter had none. Never read identity from the lane ctx.
   Pass the actor explicitly to `resolveLandConflicts` if needed.
2. Policy admission for a request uses the same actor that the unbatched
   pre-receive would use: the agent identity name if present, otherwise the
   request's `User.GlId` (non-agent callers are NOT skipped).
3. The combined update ctx MUST NOT carry any submitter identity: strip it
   (e.g. a fresh ctx from `context.WithoutCancel` of a ctx built only with the
   needed values: feature flags, logger fields, changelog operation
   "land_batch" and members), use the synthetic graft-batch user, and record
   the changeset actor as "graft-batch" (members carry the real actors).
4. Unbatched responses report batch_size=1 and batch_position=1 (Foundation);
   update the existing exact-response test expectations accordingly.
5. Required regression tests (use testserver.WithInterceptors with an
   interceptor calling identity.NewContext, as the gate did): resolver
   actors enforced per request in a batch (both orders); non-agent gl_id
   denied by policy with batching on; a denied agent submitting first does not
   prevent the other members from landing together in one batch; changeset
   actor is graft-batch and members carry the real actors.
