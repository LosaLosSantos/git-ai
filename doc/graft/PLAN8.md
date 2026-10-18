# Graft phase 4b: land batch latency, root causes first (binding plan)

Status: proposal of the p4b workstream, waiting for the PM's approval. Nothing in
this plan is implemented. Owner: p4b (Opus). Branch `p4b-pipelined-batches`.

Phase 4a is in [PLAN4.md](PLAN4.md); its acceptance and the measured misses are in
[BENCH.md](BENCH.md#measured-phase-4a-results) and
[LIMITATIONS.md](LIMITATIONS.md#phase-4a-limitations). The early completion rules
are in [LAND-BATCHING.md](LAND-BATCHING.md#early-completion). The storage core
write path is in [STORAGE.md](STORAGE.md) and [PLAN6.md](PLAN6.md).

## 1. Goal and acceptance

Meet the phase 4a acceptance that the final phase 4a tree misses
(`examples/graft/demo/acceptance-phase4.sh`): with batching on, `bench-batch.sh` at
20, 50 and 100 agents of 5 changes, conflict rate 0.2, seed 1:

* A1: attempts/landed = 1.00 at every size;
* A2: failures = planned conflicts only (`failed == conflicts`);
* A3: p99(on) <= p99(off) at every size;
* stretch S: p99(on-100) <= 0.5 x p99(off-100);
* T: lands per minute with batching on do not drop below the phase 4a run of
  record at any size (250.9 at 50 agents, 257.6 at 100), because the README claim
  (2x at 50 agents, 2.9x at 100) rests on them.

The plan started as "pipelined batches". The profile below shows that the latency
does not come from the lane running batches one after another, but from two
structural defects inside each batch. This plan fixes those. Pipelining is kept as
a gated second stage (section 9), decided by the measurement after stage 1.

## 2. Profiling evidence

### 2.1 Method

* Temporary probes (histograms `graft_prof_*`), never committed, around every step
  of a batch attempt: the lane (collect, gap between batches, execute, deliver),
  each member (peel, the ancestry checks, the merge-commit check, the merge base,
  the rebase engine's resolve, merge base, todo list, commit reads, merge-tree,
  commit write), the publish (pre-receive, quarantine migrate, update hook,
  update-ref start, prepare, commit, every reference-transaction phase,
  post-receive, the changeset-log ticket wait and append), and the number of files
  each migrate moves. Updates of the benchmark's private branches
  (`UserCommitFiles`) are labelled apart from the batch updates.
* A driver runs only the batching-on server (config of `bench-batch.sh`, port 9252)
  with 50 then 100 agents x 5 changes, conflict rate 0.2, seed 1, scrapes
  `/metrics` before, after and every 5 s, and samples the load average.
* Machine: Intel Core Ultra 7 255H, 16 logical CPUs, WSL2 kernel 6.6, ext4 on a
  virtual disk, bundled git 2.56.0-rc1. Shared with the m3 workstream: the load
  average is given with every run. These are diagnostic runs (one each, batching
  on only). They do not replace the acceptance run, which needs a benchmark window.
* Storage core off, as in `bench-batch.sh`.

### 2.2 Runs (batching on, diagnostic)

| run | tree | load (1 min) | 50 agents: p50 / p99 ms, lands/min | 100 agents: p50 / p99 ms, lands/min |
|---|---|---|---|---|
| 4a record (BENCH.md) | phase 4a final | 8-38 | 8959 / 12658, 251 | 17292 / 23253, 258 |
| run1 | 4a + probes | 4.4-6.5 | 11663 / 19786, 178 | 14843 / 21261, 287 |
| run2 | 4a + finer probes | 2-3 | 5883 / 8768, 378 | 16074 / 26312, 256 |
| run4 | experiment E (2.6) | 1.8 (50), 8-10 (100) | 2510 / 5118, 716 | 10453 / 19567, 410 |
| run5 | experiment E (2.6) | 5-7.5 | 2317 / 3100, 901 | 5645 / 14847, 580 |

In every run A1 and A2 held (failures = 50 and 108 planned conflicts), except run3,
the first version of experiment E (see 2.6).

### 2.3 Where a batch spends its time (run2, 100 agents)

* The lane is busy all the time: the batches executed for 91.4 s of a 91.9 s run.
  The gap between two batches summed to 16 ms, the collect wait to 13 ms. So the
  latency is queueing behind earlier batches, as phase 4a found.
* A batch already publishes with **one** reference transaction: 179 updates for
  197 batch executions (the others changed nothing). Members do not run their own
  reference transaction or hooks. The "about 100 ms per member" of phase 4a is
  the work in the quarantine before the publish, not a reference update.
* Mean batch: 464 ms = members 285 ms (2.5 members of 111 ms) + publish 195 ms
  (+ tip 3.7 ms, quarantine 0.6 ms, cleanup 1.4 ms).

One member, 111 ms:

| step | ms | what runs |
|---|---:|---|
| peel the commit ID | 3.9 | `git rev-parse --verify <id>^{commit}` |
| already landed? | 8.0 | `git merge-base --is-ancestor commit tip` |
| fast-forward? | 5.8 | `git merge-base --is-ancestor tip commit` |
| merges in range? | 6.9 | `git rev-list --merges tip..commit` |
| related? | 6.6 | `git merge-base --all tip commit` |
| engine: resolve both again | 5.2 | 2 x `git rev-parse` |
| engine: merge base again | 6.2 | `git merge-base tip commit` |
| engine: todo list | 6.3 | `git rev-list --right-only ... tip...commit` |
| engine: read commits | 1.4 | long-lived `cat-file` |
| engine: merge | 46.2 | long-lived `merge-tree --stdin` |
| engine: write the commit | 18.2 (x 0.78) | long-lived `hash-object --stdin-paths` |

Nine short git processes (49 ms) answer questions that one range query answers,
and four of them repeat questions the stepper has already answered (the engine
re-resolves, re-computes the merge base and re-lists the range because its
`Rebase` keeps the full semantics of `localrepo.Rebase`). The long-lived helpers,
which phase 4a introduced to avoid process starts, still cost 60 ms per member.

Publish of a batch, 195 ms (lane updates only):

| step | ms |
|---|---:|
| quarantine migrate | 170.2 (9.2 files per migrate) |
| reference-transaction committed (changeset-log append 12 ms, ticket wait) | 10.0 |
| update-ref prepare (lock and fsync) | 8.2 |
| update-ref start | 3.5 |
| pre-receive, update, prepared, commit, post-receive | 1.6 |

### 2.4 Micro-benchmarks (copy of the run2 repository, 393 commits on the branch, load 2.6)

| measurement | result |
|---|---|
| write + fsync of a 300-byte file | 6.4 ms mean (p90 9.5) |
| `git rev-parse --verify <id>^{commit}` (process start) | 4 ms |
| `git merge-base --is-ancestor agent tip` | 7 ms |
| `git rev-list --boundary --parents tip..agent` | 5 ms |
| `merge-tree --stdin`, 100 merges, `core.fsync=objects` | 10.4 ms per merge |
| same with `core.fsync=none` | 0.5 ms per merge |
| `hash-object -w --stdin-paths`, 100 commits, `core.fsync=objects` | 9.2 ms per commit |
| same with `core.fsync=none` | 0.2 ms per commit |

Gitaly runs every git command with `core.fsync=objects,derived-metadata,reference`
and `core.fsyncMethod=fsync`. A batch writes every new object (2-3 trees from
merge-tree and 1 commit per member) as a loose object, fsynced when written; then
`quarantine.Migrate` hard-links each one into the repository and fsyncs the file
again and its fan-out directory (`internal/git/quarantine/quarantine.go`,
`migrate`). About three fsyncs per object, about 12 per member.

### 2.5 Other observations

* `UserCommitFiles` (the agents' commits, outside the lane) takes 0.3-0.86 s per
  request. Its time is the same migrate and the changeset-log ticket wait (68-106
  ms): every reference update of the repository, the benchmark's private branches
  included, appends to one changeset log with an fsync, in order. That is in the
  agent latency of both columns (on and off) and is not lane work.
* Adaptive sizing keeps batches small (2.5 members per attempt at 100 agents)
  because every batch takes longer than `target_latency` (500 ms). That is a
  consequence of the slow members, not a cause. This plan does not tune it.
* run3 (first version of experiment E) failed 16 lands with "migrating quarantined
  objects: sync directory: ... objects/0e: no such file or directory": it left the
  emptied fan-out directories in the quarantine, `Migrate` re-created them in the
  repository and fsynced them, and the housekeeping of the same repository removed
  the empty directories in between. The design below never leaves an empty fan-out
  directory in a quarantine, so a batch migrate creates no fan-out directory at all
  (it moves only `pack/` files). The race window in `Migrate` itself is upstream
  code outside p4b; it is reported to the PM (section 14).

### 2.6 Experiment E (temporary, not committed)

Two diagnostic changes, switched by environment variables of the server: the
merge-tree and hash-object helpers of a batch ran with `core.fsync=none`, and
before the publish the stepper packed every loose object of the quarantine into
one pack (`git pack-objects`) and removed the loose files and their emptied
fan-out directories. The nine short processes per member were not changed.

| | run2 (4a) | run5 (E), load 5-7.5 |
|---|---:|---:|
| member, ms | 111 | 42 (merge 2.1, write 2.8; the nine processes ~37) |
| publish, ms | 195 | 202 at 100 agents (pack 84, migrate 67, committed 26, prepare 17), 85 at 50 |
| migrate files per batch | 9.2 | 3 (pack, idx, rev) |
| p99 50 / 100 agents, ms | 8768 / 26312 | 3100 / 14847 |
| lands/min 50 / 100 | 378 / 256 | 901 / 580 |

The member cost fell by the fsync share exactly as the micro-benchmarks predict, and
the migrate went from scaling with the members to three files. What remains per
member is the nine process starts, and per batch the publish (pack, migrate,
update-ref, changeset log), whose cost is fsync-bound and rises with the load of the
machine.

### 2.7 Root causes

* **R1, fsync amplification of the batch's objects.** Objects that live in a
  quarantine until the publish are written loose and fsynced one by one, then
  fsynced again one by one (plus a directory each) by the migrate. About 12 fsyncs
  per member; with 6.4 ms per fsync that is 60 ms of member time and 68 ms of
  publish time per member. The durability that matters (objects on disk before the
  reference points to them) needs one durable write of the batch's objects, not
  three per object.
* **R2, the land decision is computed piecewise and twice.** The stepper decides
  "already landed", "fast-forward", "merges in range" and "related" with four
  separate history walks, then the engine walks again to get the merge base and the
  todo list. One `rev-list --parents tip..commit` answers all of them (proof in
  3.1). The duplication exists because the engine's only entry point re-derives what
  its caller knows.
* Not a cause: the lane's serial order. Member i is replayed onto the tip that holds
  members 1..i-1, so the member work of one batch has a real dependency chain. The
  idle time of the lane is ~0. The one thing that could overlap without breaking
  that dependency is the publish of batch N with the member work of batch N+1
  (section 9).

## 3. Design (stage 1)

### 3.1 One range query per member (fixes R2)

For a request on an existing branch, the stepper:

1. Peels the commit ID through the object info reader of the attempt
   (`catfile.ObjectInfoReader.Info(ctx, "<id>^{commit}")`), instead of
   `rev-parse --verify`. The ID is full hex (it was parsed with `FromHex`), so
   there is no abbreviation to resolve. Missing or not peelable to a commit gives
   the same `InvalidArgument` ("cannot resolve commit ID", `tipIndependent`).
2. Lists the range once: `Range(ctx, tip, commit)` = the commits reachable from
   `commit` and not from `tip`, each with all of its parents (`git rev-list
   --parents tip..commit`, one process).
3. Decides with a pure function `rebase.PlanLanding(tip, commit, rng)`:
   * range empty <=> `commit` is reachable from `tip` => **already landed**;
   * `tip` is a parent of a commit of the range <=> `tip` is reachable from
     `commit` => **fast-forward**. (If `tip` is reachable from `commit`, the commit
     just above `tip` on a path from `commit` is in the range and has `tip` as a
     parent; the converse is immediate.) Merge commits in the range are allowed
     here, as today;
   * otherwise **diverged**. For the rebase strategy, `plan.Replay()` returns:
     * `ErrMergesInRange` when a commit of the range has two or more parents
       (= `rev-list --merges tip..commit` non-empty), checked first, as today;
     * else the range is a chain (every commit has at most one parent). Its bottom
       commit either has a parent, which is outside the range and so reachable
       from `tip` (a common ancestor exists), or is a root, in which case no
       ancestor of `commit` is reachable from `tip`: `ErrUnrelated` (= no merge
       base);
     * else the todo list: the chain from its bottom to `commit` (= `rev-list
       --right-only --no-merges --topo-order --reverse tip...commit` without
       `--cherry-pick`, which `RebaseWithReappliedCherryPicks` already drops).
   * The other strategies use only already landed / fast-forward / diverged, then
     `computeLandedTip` as today (merge, `ff_only`).
4. Replays the todo list with `Engine.Replay` (3.3). The engine no longer resolves,
   recomputes the merge base or lists the range.

The early completion rules, the empty-commit rule, the policy admission, conflict
resolution and every error message and code stay as they are; only how the facts
are obtained changes. Grafts and replace references are honoured by `rev-list`
exactly as by `merge-base` and the old `rev-list` calls.

`[graft.land] graph_ancestry`: the checker of the attempt gains `Range`. The git
checker runs the `rev-list`; the graph checker walks the stored graph index in
process (from `commit`, down the parents, stopping at commits reachable from `tip`
by the index's `IsAncestor`), and keeps its rule of refusing shallow, grafted or
replaced repositories (fallback to git, logged, as today). Both must return the same
set (equivalence test). The single-land path keeps `IsAncestor` and `MergeBase`
(see question Q2).

### 3.2 Durable once: the batch publishes its objects as one pack (fixes R1)

* The merge-tree stream and the commit writer of a batch attempt write loose
  objects **without fsync** (`core.fsync=none` on these two helper processes only,
  section 6). They write nothing but loose objects into the attempt's quarantine.
  `core.fsync` cannot be narrowed relative to Gitaly's value: verified with the
  bundled git, a later `-c core.fsync=-loose-object` replaces the whole value
  instead of subtracting from it, and `objects,...,-loose-object` still fsyncs
  loose objects. So the value for these processes is stated, with the reason
  above, not derived.
* Before the compare-and-swap of a batch (`Update` and `UpdateSolo` call one shared
  `publish` helper), the stepper packs **every loose object found in the
  quarantine's object directory** into one pack inside the quarantine
  (`git pack-objects <quarantine>/pack/pack` with the object IDs on stdin, run
  with Gitaly's normal fsync configuration, so the pack and its index are
  fsynced), then removes those loose files and the emptied fan-out directories.
  A batch whose quarantine has no loose object (fast-forwards only) packs nothing.
* `quarantine.Migrate` (unchanged) then moves only the `pack/` files into the
  repository, fsyncing them and the directories, before `update-ref` runs. The
  reference transaction, its hooks, the voter and the database CAS see exactly the
  same state as today.

Durability invariant D (section 4): every object reachable from the new tip and
not from the old one is in the repository's object store, durable, before the
reference transaction prepares. Proof: git writes every new object of the attempt
into the quarantine's object directory (its primary object directory), including
the objects of the resolver; the pack holds every loose object of that directory;
`pack-objects` exits 0 only after writing and fsyncing pack and index; the loose
files are removed only after that; anything in the quarantine that is not a loose
object is migrated by `Migrate` with its own fsync, as today.

A failure to pack fails the publish of that attempt with `Internal` (the
`landUpdateFailed` outcome: members fail with the error, the requests after the
first member that are not members are landed again on the real tip, as today). The
loose objects stay untouched in that case and the quarantine is discarded with the
attempt. There is no fallback to migrating loose objects: a pack-objects failure
is an I/O or resource fault that the loose path would not fix.

### 3.3 The engine: one replay loop, no second derivation

* `Engine.Replay(ctx, upstream, todo, opts...)` is the replay loop that
  `Engine.rebaseCommits` is today (empty-commit rules, committer option, full
  commit messages, `*localrepo.RebaseConflictError`), taking the todo list from the
  caller.
* `Engine.Rebase` is deleted: after this plan nothing in production calls it. The
  equivalence tests compare `Range` + `PlanLanding` + `Replay` with
  `localrepo.Rebase` instead.
* `rebase.New(repo, rebase.WithDeferredDurability())` starts the two helpers with
  `localrepo.WithoutObjectFsync()`. The option says what it means: the caller makes
  the objects durable before a reference points to them. The batch stepper is the
  only caller that passes it.

### 3.4 What does not change

The lane (`internal/graft/landlane`), batch formation, adaptive sizing, the
`max_rebuilds` loop, the fallback after a hook rejection, early completion,
a batch of one as `land`, changelog operations, actors and members, the policy
(admission in process and the hooks), the synthetic `graft-batch` identity, the
reference update path (`UpdaterWithHooks`, untouched), the storage core (voter
Preparing/Prepared, database CAS, `ensureStorageFresh` before rebuilds), the
single-land path (unless Q2 says otherwise) and every response field.

## 4. Invariants (each has a test, section 10)

* I1 Per-reference CAS and order: a batch publishes with one CAS from the tip it
  read; requests land in FIFO order; a lost CAS rebuilds the batch.
* I2 Early completion rules of LAND-BATCHING.md, unchanged: no request is reported
  landed unless its commit is on the branch.
* I3 Same decisions: for any (tip, commit) the plan gives the same outcome as the
  phase 4a sequence (already landed, fast-forward, merges error, unrelated error,
  todo list, conflict) and the same landed commit IDs (byte-identical commits).
* I4 Changeset log: `land_batch` with `graft-batch` and members, a batch of one as
  `land` with its actor and single member; no change in meaning.
* I5 Policy and hooks: same admission, same hook runs, same skip rule.
* I6 Storage core: every batch update goes through the voter and the database CAS;
  `TestStorage*` pass in shadow and authoritative mode.
* D Durability: section 3.2. Plus: after every publish the quarantine holds no
  loose object and no empty fan-out directory; `git fsck --connectivity-only` of
  the repository is clean after any run.

## 5. Failure and race cases

| case | behaviour |
|---|---|
| pack-objects fails (disk full, killed, ctx done) | publish fails, `landUpdateFailed` rules; loose objects untouched; quarantine discarded; ref unchanged |
| crash after the pack is written, before Migrate | quarantine is temporary and removed; nothing reachable; same as today |
| crash after Migrate, before update-ref | unreferenced pack in the repository; housekeeping removes it like any unreferenced pack (as after a failed push) |
| crash after the database commit (authoritative), before git commits | unchanged: the next EnsureFresh repairs (PLAN6); the pack is in the repository |
| housekeeping runs during Migrate | Migrate touches only `objects/pack` (always present): no fan-out directory is created, so the run3 race cannot happen for batches |
| lost CAS / rebuild | new attempt, new quarantine, new pack; the old quarantine (with its pack) is discarded |
| resolver writes objects | they are loose objects of the quarantine, packed with the rest |
| request cancelled mid-batch | unchanged: still lands, caller gets Canceled |
| range of a commit with a very long history | `rev-list` lists it once (today it is walked five times) |
| graph index unusable (shallow, grafts, replace refs) | graph checker falls back to git, logged, as today |

## 6. Config and API changes (exact)

No new configuration keys, no change of defaults. `graph_ancestry` also covers the
range query of a batch member (doc change in CONFIGURATION.md and LAND-BATCHING.md).

```go
// package ancestry (internal/graft/ancestry)
// RangeCommit is a commit of a range with all of its parents, as git rev-list --parents prints it.
type RangeCommit struct {
	ID      git.ObjectID
	Parents []git.ObjectID
}
// Added to Checker, implemented by the git and the graph checker:
// Range returns the commits reachable from head and not from base (git rev-list base..head), each
// with all of its parents, in no particular order. It is empty when head is reachable from base.
Range(ctx context.Context, base, head git.ObjectID) ([]RangeCommit, error)

// package rebase (internal/graft/rebase)
type LandKind int
const (
	LandAlreadyLanded LandKind = iota + 1 // commit is reachable from tip
	LandFastForward                       // tip is reachable from commit
	LandDiverged                          // neither: the change must be rebased or merged
)
type LandPlan struct{ /* unexported: kind, tip, commit, range */ }
var (
	ErrMergesInRange = errors.New("commit range contains merge commits")
	ErrUnrelated     = errors.New("commit shares no history with the tip")
)
// PlanLanding decides how commit lands onto tip from the range tip..commit (ancestry.Checker.Range).
func PlanLanding(tip, commit git.ObjectID, rng []ancestry.RangeCommit) (LandPlan, error)
func (p LandPlan) Kind() LandKind
// Replay returns the commits to replay onto the tip for the rebase strategy, oldest first, or
// ErrMergesInRange / ErrUnrelated. Only for LandDiverged.
func (p LandPlan) Replay() ([]git.ObjectID, error)

type Option func(*engineConfig)
// WithDeferredDurability starts the helpers without fsync of the loose objects they write. The
// caller makes those objects durable before any reference points to them.
func WithDeferredDurability() Option
func New(repo *localrepo.Repo, opts ...Option) *Engine
// Replay replays todo (oldest first) onto upstream with the empty-commit rules, the committer
// option and the conflict errors of localrepo.Rebase.
func (e *Engine) Replay(ctx context.Context, upstream git.ObjectID, todo []git.ObjectID, opts ...localrepo.RebaseOption) (git.ObjectID, error)
// Engine.Rebase is removed.

// package localrepo (merge_tree_stream.go, commit_writer.go)
type HelperOption func(*helperConfig)
// WithoutObjectFsync runs the helper with core.fsync=none: it writes only loose objects, and the
// caller makes them durable.
func WithoutObjectFsync() HelperOption
func (repo *Repo) NewMergeTreeStream(ctx context.Context, opts ...HelperOption) (*MergeTreeStream, error)
func (repo *Repo) NewCommitWriter(ctx context.Context, opts ...HelperOption) (*CommitWriter, error)

// package operations (new file land_batch_pack.go)
// packAttemptObjects packs every loose object of the attempt's quarantine object directory into one
// pack in that directory and removes the loose files and the emptied fan-out directories. It
// returns the number of packed objects (0: nothing to pack).
func packAttemptObjects(ctx context.Context, repo *localrepo.Repo, objectDir string) (int, error)
```

Removed as dead code: `serverLandBatchStepper.computeTip`'s mirror of the rebase
branch (replaced by the plan), `Engine.Rebase`, the per-member `IsAncestor` calls of
the stepper. `rejectMergeCommitsInRange` and `hasMergeBase` stay for the single-land
path (unless Q2).

## 7. Metrics

`graft_land_batch_phase_seconds{phase}` gains `plan` (peel + range + decision, per
member), `replay` (per member) and `pack` (per publish). `land_member` keeps its
meaning (the whole member). New counter `graft_land_batch_packed_objects_total`.
`bench-batch.sh` adds the new phases to its "time per phase" table.

## 8. Work packages

* WP81 (Opus): `ancestry.Range` (git and graph), `rebase.PlanLanding`, equivalence
  tests against the phase 4a sequence.
* WP82 (Opus): `localrepo` helper option, `rebase.Engine` options, `Replay`,
  removal of `Rebase`, equivalence with `localrepo.Rebase`.
* WP83 (Opus): stepper integration (peel, plan, replay), `packAttemptObjects`, the
  shared `publish`, metrics, dead-code removal.
* WP84 (Haiku, from an exact spec written by Opus after WP81-83 compile): spec
  tests `TestSpec*` for `PlanLanding` cases and `packAttemptObjects`; then the
  `bench-batch.sh` phase rows. Checked by Opus.
* WP85 (Opus + Haiku drafts): the measurement in the PM's window and the docs
  (LAND-BATCHING, BENCH, LIMITATIONS, CONFIGURATION section of `graph_ancestry`),
  numbers only from the real run.

## 9. Stage 2: pipelined publish (gated, not part of the approval of stage 1)

After stage 1, a batch is about 8-10 ms per member (the range query plus the
replay without fsync) and a fixed publish of 85-200 ms (pack, migrate, update-ref,
changeset log; fsync-bound, rising with the machine's load). That publish is
inherent per-batch work. It can overlap with the member work of the next batch:
batch N+1 lands its members onto the unpublished tip of N, in its own quarantine
whose alternates are N's quarantine and then the repository (so an object moved by
N's migrate is found in one of them at every instant: the link precedes the
removal), and publishes only after N's CAS succeeded; if N fails, N+1 is rebuilt
on the real tip. Tip-dependent early answers of N+1 wait for N's publish.

Gate G1: after the stage 1 acceptance run, stage 2 is proposed (as a binding
amendment of this plan, with its own invariants and tests) only if A3 or S fails,
or if they pass with less than 20% margin in either of the two runs, and the
phase table shows the publish as the largest share of the lane time. Otherwise it
is not built: it would add two batches in flight per lane for a gain the
acceptance does not need.

## 10. Test plan

Unit and package tests:

* `ancestry`: `Range` git vs graph on the seeded random DAGs of `TestGraphMatchesGitOnRandomDAG`
  (>= 200 commits, merges, criss-cross, several roots), every sampled pair; empty
  range; replace refs make the graph checker fall back.
* `rebase`: `PlanLanding` table (already landed, equal, fast-forward with merges in
  the range, diverged chain, merges in range, unrelated chain, root in range);
  oracle test: for seeded random DAGs, `Range`+`PlanLanding` equals the phase 4a
  sequence (`IsAncestor` x2, `rev-list --merges`, `merge-base --all`, the engine's
  todo `rev-list`) for every pair; `Replay` equals `localrepo.Rebase` (OIDs,
  conflict errors and contents) on the existing equivalence corpus; the helper
  option: the helpers' command carries `core.fsync=none` (test hook on the command
  options), output identical with and without it.
* `operations`: every existing `land_batch*` and `land*` test passes unchanged;
  new: peel through the info reader (tag peeled, blob and tree rejected with the
  same error, missing object), one range query per member (count `rev-list` and
  `merge-base` processes through the existing rebaser/test hooks: 1 and 0),
  `packAttemptObjects` (all loose objects in one pack, none left loose, no empty
  fan-out directory, objects readable, nothing to pack for fast-forwards, failure
  injected through the command hook -> `landUpdateFailed`, ref unchanged, loose
  objects untouched), invariant D after a batch (`fsck --connectivity-only`, the new
  pack is in `objects/pack`), batch of one also packs.
* `-race` on `internal/graft/landlane/...`, `internal/graft/rebase/...`,
  `internal/graft/ancestry/...` and the `TestLandBatch*`/`TestUserLand*` tests of
  `internal/gitaly/service/operations`.

Integration (`internal/graft/e2e`): `TestPhase4*`, `TestChaosLanding` (not
`-short`), `TestStorage*` (shadow and authoritative, SQLite and Postgres; S3 skips,
no server here), and a new e2e test: batched lands with the storage core in
authoritative mode, `VerifyRepository --connectivity` clean afterwards.

Benchmark (needs a PM window): `acceptance-phase4.sh` twice on the stage 1 tree;
plus one informational `bench-batch.sh` run with `[graft.storage] mode = "shadow"`
(SQLite) to report the voter's share, not part of the acceptance.

## 11. Acceptance of this plan

* Both acceptance runs: A1, A2, A3 PASS at 20, 50 and 100 agents; S reported (target
  PASS); T: lands per minute at or above the 4a record at every size.
* Per member (phase table): `plan` <= 10 ms and `replay` <= 5 ms mean at 50 and
  100 agents; `pack` + migrate scale with batches, not with members.
* All tests of section 10 green, `go vet`, `gofmt -l` clean, no probe left.
* Docs updated with the measured numbers and the load of each run.

Predicted (from 2.3-2.6, not a promise): member ~10 ms, publish 85-200 ms, lane
capacity of tens of lands per second, p99 at 100 agents in the low seconds; the
next bottleneck becomes `UserCommitFiles` and the per-repository changeset log
(2.5), which are outside this workstream.

## 12. Rejected alternatives

* Pipelining first (the original plan): it hides at most the publish share (40% of
  a batch in run2) and leaves R1 and R2. After stage 1 it is gated (section 9).
* One reference transaction for all members: already the case (2.3).
* Rebasing disjoint members in parallel: the members of a batch depend on each
  other's tips; parallel tree merges would need a merge implementation outside git
  with byte-identical results. Not needed after stage 1.
* Tuning `window`, `max_size` or `target_latency`: adaptive sizing reacts to slow
  members (2.5); tuning would hide R1 and R2 (forbidden by the charter).
* Turning off object fsync globally or in `quarantine.Migrate`: durability of every
  other write path depends on it, and it is not p4b's code.
* Keeping loose objects without fsync and letting Migrate fsync them: removes the
  write-time fsync but keeps two fsyncs per object in the publish (run2 migrate: 170
  ms for 9 files).
* An in-process range walk without the graph index: a reimplementation of git's
  revision walk; `rev-list` costs 5 ms and the graph index already gives an
  in-process path when enabled.

## 13. Split of the work

Opus writes everything on the land decision, the engine, the stepper, the
publish (durability) and every concurrency-related test, and reviews all Haiku
output. Haiku (one at a time, never two in this worktree) drafts the `TestSpec*`
files from an exact spec, the `bench-batch.sh` table rows and the first drafts of
the doc sections, all checked by Opus.

## 14. Risks and open questions

Risks:

* One small pack per batch in the repository, like one pack per push: housekeeping
  repacks sooner (the runs show `repack` at 0.4-1.8 s per run under `UserLand`). To
  watch in the acceptance runs; not a correctness issue.
* The numbers are from a shared WSL2 machine with fsync at 6 ms; the gain on other
  disks is smaller or larger, the structure is the same.
* The equivalence of `Range`+`PlanLanding` rests on the proof in 3.1; the oracle
  test on random DAGs is the guard.

Questions for the PM:

* Q1: approve stage 1 as the p4b deliverable, with stage 2 behind gate G1?
* Q2: the single-land path (`land.go`) has the same duplicated decision (its
  `landAttempt` walks history four times and rebases with `localrepo.Rebase`,
  LIMITATIONS 4a item 2). Moving it onto `Range`/`PlanLanding`/`Replay` removes the
  duplication (and `rejectMergeCommitsInRange`, `hasMergeBase`). It also makes the
  "off" column faster, so it changes the baseline of A3. Proposal: do it as WP86
  after the stage 1 acceptance runs, and report the acceptance before and after.
  In p4b or later?
* Q3: a benchmark window for two acceptance runs (about 12 minutes each) once WP81-85
  are green.
* Q4 (not p4b's code, for information): `quarantine.Migrate` creates a target fan-out
  directory and fsyncs it after migrating into it; housekeeping can remove an empty
  directory in between (seen in run3 with empty source directories). Batches no
  longer hit it after stage 1; other writers might, rarely. And the per-repository
  changeset-log order (ticket wait 68-106 ms per `UserCommitFiles`) is now the
  largest cost of an agent's commit.
