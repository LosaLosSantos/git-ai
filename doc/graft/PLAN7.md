# Graft phase 7: pack garbage collection and checkpoint compaction (binding plan)

Status: binding contract for WP71–WP79 (storage core milestone 3). First
version 85189d4; **amended 2026-10-18 after the PM review** (changes A–H, the
answers to the open questions, and the "No patches: refactor the cause" section
of PM.md). It extends the phase-6 foundation (`doc/graft/PLAN6.md`, which stays
binding: the CAS semantics, the write path, the read path and catch-up, the
REFTABLE, IMPORT, STALE CACHE and LOCK AND HISTORY rules, and RULES 1–15). This
plan adds tables, interface methods and packages, and **refactors two phase-6
contracts** (the pack key format and `BlobStore.Put`, see the PACK OWNERSHIP
RULE), authorized by the PM provided every phase-6 storage test is green
afterwards. It never edits a released migration.

Second amendment (PM approval of be50089): change A (per-row pack ownership)
approved; the B/C refinement (GC BOUNDARY rule, no missing-pack restart)
confirmed; the REVIEW ROOTS RULE added (open reviews are a second kind of root
into the object graph, and the review service is now m3's for that check);
the enforcement points made explicit (every upload goes through one deadline
helper, `sync_timeout` and `W` are context deadlines, not assumptions).

Owner workstream: **m3** (`internal/graft/corestore/**`,
`internal/gitaly/service/storagecore/**`, the storage sub-commands of
`cmd/graft`, `proto/storage_core.proto`, the `GraftStorage` part of the config,
`doc/graft/STORAGE.md`, and the storage core block of `internal/cli/gitaly/serve.go`).

## Problem

`doc/graft/STORAGE.md`, "Limits", states two gaps left after the first release:

1. **Packs are never deleted.** Every reference transaction uploads a pack
   before it commits (PLAN6, write path), and nothing ever removes one:
   * **orphans** of writes that uploaded a pack and then did not commit (an
     aborted transaction after the Preparing phase, a node that died before the
     database commit);
   * **superseded** changeset packs, whose objects a later consolidated state
     already contains;
   * the packs of **removed repositories** (`RemoveRepository` cascades the
     database rows, the bucket keeps `repos/<repo_id>/`).
2. **No checkpoint compaction.** Hydration (`syncer.hydrate`) and the dry
   hydration of `graft storage verify --connectivity` replay every changeset
   from sequence 1. Their cost grows without bound with the history.

A third, latent problem was found in review and is the subject of the PACK
OWNERSHIP RULE: **bucket objects are silently shared between writes.** Pack keys
are content-addressed and `BlobStore.Put` returns success without writing when
the key exists, so a write can commit a changeset whose pack is an object it did
not write and whose liveness is decided by other rows. Any collector, however
careful, can then delete an object that a write in flight relies on. Phase 6 never
deletes, so the defect was harmless; phase 7 must remove its cause before it
deletes anything.

## Goal and non-goals

* **Pack ownership.** Every pack object in the bucket is written by, and belongs
  to, exactly one database row (a changeset or a checkpoint). No write relies on
  an object it did not upload.
* **Checkpoints.** Per repository, a checkpoint at sequence `N` = one consolidated
  pack holding every object reachable from the references at `N` and from the
  head and base commits of the reviews open at the time of the build, plus the
  reference snapshot at `N`, recorded atomically after the pack is uploaded.
  Hydration starts from the newest checkpoint and replays only changesets `> N`.
  Created on demand (`graft storage checkpoint` + RPC) and by the background task
  when it is enabled. Writes are never blocked while a checkpoint is built.
* **Pack GC.** Mark and sweep, database authoritative, idempotent, crash-safe
  after every step. Reclaims orphans, changeset packs covered by a checkpoint old
  enough that no reader can still need them, superseded checkpoint packs, and the
  packs of removed repositories. `graft storage gc` (+ `--dry-run`), `graft
  storage gc --removed`, and the background task.

Out of scope (stated as limits or future work in STORAGE.md): per-directory
permissions; S3/GCS/Azure testing (no server here, file and mem buckets only);
renaming anything; local-repository housekeeping (no `git gc` of a node's cache;
the cache can be rebuilt from a checkpoint); compaction of changeset, changelog or
review rows (only packs are deleted).

## Design

### Overview

The expensive, immutable state is the packs in the bucket. The cheap state is the
changeset metadata (`graft_changesets`, `graft_changeset_refs`) and the changelog
(`graft_changelog`). Phase 7 deletes packs and never touches those rows, so the
WAL, the changelog and their consumers' cursors are unaffected. A checkpoint
supersedes the *packs* of the changesets up to its sequence, not their rows.

```
seq:   1    2    3   ...   N (checkpoint)    N+1  ...  head
packs: p1   p2   p3  ...   pN                p_{N+1} ... p_head
                           C_N (consolidated pack of the checkpoint)
rows:  every changeset row is kept (WAL + changelog)
```

`p1..pN` are reclaimed only once the checkpoint `N` is old enough that no
hydration, catch-up or verification that could still read them is running (the
GC BOUNDARY rule). Every pack has exactly one owner row (the PACK OWNERSHIP
RULE), so "is this pack still needed" is a question about its owner row only.

### PACK OWNERSHIP RULE (change A, binding)

**Root cause.** The dedup race reported in review (a write's `Put` finds the key
and skips; GC deletes the key; the write commits a changeset referencing a missing
pack) is not a timing window to be narrowed. Its cause is structural: pack keys
are content-addressed and shared, `Put`-exists makes a write depend on an object
it did not write, and that object's liveness belongs to other rows (an older
covered changeset, or a failed write's orphan). The phase-6 orphan grace was a
time assumption, not an enforced invariant. Both are removed at their source.

**Decision: per-row ownership** (direction 1 of the PM review). Rejected:
database fencing of deletions with a doomed set (direction 2), see "Rejected
alternatives".

1. **Owned pack keys.** A pack key is
   `repos/<repo_id>/packs/<sha256>-<nonce>.pack`, where `<sha256>` is the SHA-256
   of the bytes (64 lowercase hex, still verified on every `Put` and `Get`, RULE
   22) and `<nonce>` is 16 random bytes (`crypto/rand`, 32 lowercase hex) drawn
   for that one upload. Every writer (the voter, `ImportRepository`, the
   checkpoint builder) uploads under a key it just generated
   (`corestore.NewPackKey(repoID, sum)`). There is no cross-row dedup: two writes
   with byte-identical packs store two objects. Legacy keys
   `repos/<repo_id>/packs/<sha256>.pack`, written by v0.1.0 and still referenced
   by its rows, stay readable (`ParsePackKey` accepts both forms) and are never
   written again.
2. **`BlobStore.Put` is strictly create-only.** `Put` writes with
   `blob.WriterOptions{IfNotExist: true}`; an existing key is the new error
   `corestore.ErrBlobExists`, never success. The `Exists` pre-check is removed (one
   bucket round trip less per write). A fresh nonce never collides in practice; if
   it does, the writer fails rather than share an object. This is a phase-6
   contract change: the blobstore tests "Put idempotent" and "concurrent Puts of
   the same key" assert the old requirement and are rewritten to the new one
   (exactly one concurrent `Put` of a key succeeds, the others return
   `ErrBlobExists`, and the stored content is that of the winner).
3. **Database fence for writers that are not upgraded.** A v0.1.0 binary cannot
   start against a migrated database (sql-migrate v1.8.1 refuses an unknown
   applied migration, and the store does not set `IgnoreUnknown`), but a node
   already running v0.1.0 keeps writing with shared keys. Migration 2 therefore
   installs a trigger on `graft_changesets` that rejects an insert whose
   `pack_key` is not NULL and not an owned key
   (`pack_key NOT LIKE 'repos/%/packs/%-%.pack'`). A write from an old binary that
   carries a pack fails at its database commit (authoritative: git aborts it;
   shadow: counted as a shadow error). Writes without a pack are unaffected.
   Operational consequence, documented: upgrade every node together; until a node
   runs the new binary, its pack-carrying writes fail.
4. **In-flight writes have an enforced deadline.** Without sharing, the only
   object a write relies on is its own upload, which is an unreferenced object
   until the write commits. The writer and the collector are bound by one rule:
   * **Writer.** Let `W = gc_grace − gc_margin`. The writer takes the time `t_r`
     (monotonic clock) of the repository-record read (`GetRepository`) that
     precedes its upload. It uploads the pack and commits the row that references
     it under a context whose deadline is `t_r + W`. When the deadline passes, the
     `Put` is cancelled (gocloud discards a cancelled writer, nothing is stored) or
     the commit transaction is not started, and the write fails like any storage
     failure (authoritative: `Unavailable`, git aborts; shadow: counted, the git
     write proceeds). The voter's Prepared phase uses the pack of the Preparing
     phase only when that pack's deadline has not passed; otherwise it builds and
     uploads a new owned pack itself (the existing "Preparing did not prepare"
     path), with a new `t_r` from its own record read. `ImportRepository` and the
     checkpoint builder re-read the record immediately before their upload, so a
     long pack build does not consume the deadline.
   * **Collector.** An unreferenced pack is deleted only when the collector itself
     **observed** it unreferenced at least `gc_grace` earlier, measured on the
     database clock (`graft_gc_orphans.first_seen`, see the CLOCK RULE), and it is
     still unreferenced in the same database snapshot whose time decides the age.
     The bucket's modification time is never used.
5. **Proof (no live pack is deleted).** Let `t_s` be the database time of the
   snapshot in which the collector finds key `K` unreferenced with
   `first_seen(K) <= t_s − gc_grace`. `first_seen(K)` was recorded after a listing
   that returned `K`, so the upload of `K` completed before `first_seen(K)`, and
   the writer's record read `t_r` was earlier still. The writer can commit only
   before `t_r + W < first_seen(K) + gc_grace − gc_margin <= t_s − gc_margin`. A
   commit before `t_s` is visible in the snapshot, so a committed row would
   reference `K`. Since none does, no writer will ever commit `K`, and deleting it
   is safe. The only clock assumption is that the drift of the clock **rates**
   (the writer's monotonic clock against the database clock) over `gc_grace` is
   smaller than `gc_margin`; absolute skew between machines does not enter.
   `gc_margin` also absorbs the latency of a commit that is in flight when the
   writer's deadline passes (a commit message sent just before the deadline may
   still complete after it). **Stated assumption** (repeated in STORAGE.md):
   `gc_margin` > about 2 × (relative clock-rate drift) × `gc_grace` + the longest
   in-flight commit. With ordinary clocks (drift well below 10⁻⁴) 5 min against
   1 h is far above it.
6. **Covered and superseded packs need no time.** A covered changeset pack (its
   owner row has `seq <= C_gc`) or the pack of a dropped checkpoint is owned by a
   committed row, so no writer relies on it (new writers never reuse a key, rule
   1), and its readers are excluded by the GC BOUNDARY rule. It is deleted in the
   run that finds it.
7. **Cost on the write path.** No database access is added. The `Put` loses its
   `Exists` round trip; the nonce and the deadline are negligible; the trigger is
   one `LIKE` per inserted changeset; byte-identical packs are stored twice (rare:
   retried identical pushes). The preparing and prepared histograms and the bucket
   bytes are measured before and after in the WP79 benchmark window.

### CLOCK RULE (binding)

Every time that decides a GC action is a **database time**: `graft_checkpoints.created_at`,
`graft_gc_orphans.first_seen`, `graft_gc_removed.removed_at` and the lease times
are written with the database clock (Postgres `now()` in the statement; SQLite
`time.Now()` of the host, which every node shares because SQLite is single-host),
and every "older than" comparison is made against the database time read in the
same transaction (`now()` on Postgres; on SQLite `time.Now()` taken before the
read transaction begins, so that every commit before it is visible). The writer's
deadline is a duration on its own monotonic clock. The only assumption is the
bounded rate drift of RULE 5 above, covered by `gc_margin`.

### GC BOUNDARY rule (changes B and C, binding)

Two boundaries are computed from the checkpoints, on the database clock:

* **`C_gc`** = the sequence of the newest checkpoint created at least `gc_grace`
  ago (0 when there is none). The collector reclaims changeset packs with
  `seq <= C_gc` and the packs of checkpoints with `seq < C_gc`. It keeps every
  changeset pack with `seq > C_gc` and every checkpoint with `seq >= C_gc`
  (`checkpoint_retain` of the first version is removed: the lag makes it
  unnecessary).
* **`C_sync`** = the sequence of the newest checkpoint created at least
  `gc_grace − sync_timeout − gc_margin` ago (0 when there is none). A sync round
  (catch-up or hydration, bounded by `sync_timeout` through its context) decides
  with `C_sync`: **re-hydrate when the watermark is below `C_sync`; otherwise
  catch up** (change C: a node that is a few changesets behind when a checkpoint
  is created catches up normally; the checkpoint does not count for it until it
  is about `gc_grace − sync_timeout` old).

Why no reader ever misses a pack. A checkpoint becomes `C_gc` only `gc_grace`
after its creation. For a collector run during a sync round that starts at `t0`
and ends before `t0 + sync_timeout` to reclaim a pack the round reads, the
checkpoint that defines its `C_gc` must have been created before `t0 + sync_timeout
− gc_grace`, so it was already `gc_grace − sync_timeout` old at `t0` (up to the
rate drift covered by `gc_margin`), so its sequence is `<= C_sync`. A catch-up
with watermark `>= C_sync` reads only packs `> C_sync`, a hydration reads the
newest checkpoint (whose sequence is `>= C_sync >= C_gc`) and the packs after it.
Configuration validation requires `gc_grace > sync_timeout + gc_margin`.

Consequence: under the invariant a sync never meets a reclaimed pack, so a
missing pack is corruption and stays an Internal error naming the pack (as in
phase 6). The restart on a missing covered pack of the first review is not added:
with these boundaries it would be unreachable code. (This refines B and C of the
PM review; confirmed by the PM.)

**Enforcement (binding).** The proof needs both bounds to be enforced, not
assumed. `sync_timeout` must bound **every** sync round and every hydration as a
context deadline that cancels the git processes and the bucket reads of the round;
WP76 audits the syncer's paths (the single-flight retry rounds, `fetch`,
`indexPack`, `applyBuild`, `install`), fixes any that can outlive the deadline, and
tests that a round slower than the bound is cancelled. Verify's dry hydration runs
under a deadline of `W` from its start, tested the same way.

### SHADOW BUDGET requirement (WP73b, binding; charter case B)

Found in WP73b: `TestStorageShadowModeBenchmarkHasNoDivergence` failed under
machine load with `select repository: context deadline exceeded`. Traced (pool
waits 0, SQLite lock waits under 50 ms; the failure reproduced byte for byte
with a 150 ms budget): Preparing's git work used up the shadow budget, and
Prepared failed at its first context-aware call, which was the `SELECT` of
`GetRepository`. The design behaved as specified; the test asserted more than
the design guarantees.

* **Old (implicit) requirement, asserted by the test:** in shadow mode every
  reference transaction is recorded in the database, under any load.
* **Requirement (RULE 4, stated):** shadow mode never fails or noticeably delays
  a git write: the storage-core work of one reference transaction, the Preparing
  and Prepared phases together, runs under `shadow_timeout`. A transaction whose
  work exceeds it is **not recorded**: the failure is counted in
  `graft_storage_shadow_errors_total` and logged at error level, and the git write
  proceeds. The database misses that write until the next write of the same
  reference, which is recorded as a forced changeset (counted in
  `graft_storage_shadow_divergences_total` and logged); meanwhile `graft storage
  verify` reports the reference as diverging. Recording every write is
  guaranteed only while the shadow work of every transaction fits the budget.
* **Operator rule:** switch to authoritative mode only with
  `graft_storage_shadow_errors_total`, `graft_storage_shadow_divergences_total`
  and `graft_storage_untracked_writes_total` at zero and `graft storage verify
  --connectivity` consistent on every repository.
* **Tests:** the mirroring test runs with a non-binding budget, because it tests
  the correctness of the mirroring and whether a load fits 5 s depends on the
  machine; `TestStorageShadowModeBudgetOverrun` tests the budget path
  deterministically, for an overrun in Preparing and one in Prepared (git write
  succeeds, error logged, nothing recorded, verify reports the divergence, the next
  write is forced and logged), and the voter's unit tests check the counter.

### SQLite write admission (WP73b, binding)

Found in WP73b: `TestCommitsOnOneReferenceAreDense/sqlite` (32 writers on one
reference) failed under load with `database is locked`. Traced on two cores: the
write lock is busy all the time (summed holds at least the wall time; mean hold
9 ms), but SQLite's busy handler keeps no queue, so the lock goes to whichever
waiter retries first: mean wait 0.16 s, longest 3.05 s where a fair queue bounds
it near 0.3 s, and under heavier load the tail crossed the 10 s busy timeout.
Postgres queues its row-lock waiters and is not affected.

* Every SQLite `Store` admits its write transactions one at a time, first come
  first served (`sqlstore.writeQueue`), before `BEGIN IMMEDIATE`; the admission is
  handed directly to the next waiter, so no newcomer overtakes it, and a writer
  whose context ends leaves the queue at once.
* **One admission covers one attempt** of `withTx`: it is taken right before
  `BEGIN IMMEDIATE` and released when that attempt committed or rolled back. A
  writer never holds it while `retryTx` sleeps before its next attempt, so the
  writers behind it never wait for a lock nobody uses. No transaction function
  starts another write transaction, and the admission is always taken before
  SQLite's lock, so the two cannot wait for each other (LOCK ORDER).
* Read transactions (the query-only pool) and plain reads need no admission.
* **Limit:** the fairness is per process. Several processes on one SQLite file
  (several nodes on one host) still meet in SQLite's busy handler; a multi-node
  deployment uses Postgres, SQLite is for one node or for tests (STORAGE.md).
* **Tests:** writers are admitted in arrival order; a writer whose context ends
  leaves the queue and writes nothing; the hand-over never loses the admission when
  it races a cancellation; the dense test and the LOCK ORDER `-race` test; and the
  dense test's wait distribution before and after, measured with the WP73b tracing
  under the same pinning.

### Liveness is per key (change G)

Liveness is decided per key from the rows that reference it: a key is live when
**any** changeset with `seq > C_gc` or any checkpoint with `seq >= C_gc`
references it. The snapshot therefore keeps, per key, the **maximum** referencing
sequence. With owned keys every key has one row, but legacy v0.1.0 keys can be
shared by several changesets, and the maximum makes them correct.

### What happens to unreachable objects (binding decision)

The checkpoint pack holds exactly the objects reachable from:
* the object-ID targets of the references at `N`;
* the `head_commit_id` and `base_commit_id` of every review that is **open** when
  the checkpoint is built (answer 2 of the review).

Objects reachable only from earlier states (commits orphaned by a force-update,
deleted branches or tags, rebased-away lineages) and not from an open review are
not carried into the checkpoint pack; once the changeset packs that introduced
them are reclaimed, they are gone from object storage, as `git gc` prunes
unreachable objects. Hydration and catch-up only reconstruct states at or after a
checkpoint, verification checks reachability from the current references, and
the changeset log keeps its metadata. **Documented limit:** the diff of a closed or
merged review whose commits are reachable from no reference at a checkpoint may
no longer be available.

### REVIEW ROOTS RULE (binding, added after the PM approval)

**Root cause.** Open reviews are a second kind of root into the object graph,
next to the references, and phase 6 did not count them: nothing made a review's
commits live, and the review service accepted any commit present in the serving
node's local cache, including one the storage core had already pruned. The
checkpoint closure (above) makes open-review commits live; this rule makes the
review service create only roots the storage core holds, and closes the race
between a review write and a checkpoint commit.

* **R1 — review writes (review service, authoritative mode).** `CreateReview`
  (head and base) and `UpdateReview` when it moves the head (the base never moves
  after creation) read `(refs, S) = Meta.Refs(repoID)` in one snapshot and check,
  in the local repository (which the gate caught up first), that every new root
  commit is reachable from the object-ID targets of `refs` (one `rev-list` run
  with the refs as negative tips). Otherwise `FailedPrecondition` ("commit <oid>
  of the review is not reachable from the references of the repository in the
  storage core"). The database write of the review then carries `S`: in its
  transaction, under the repository row lock, it requires that the newest
  checkpoint's sequence is `<= S`, else it returns `ErrReviewRootsStale`. This is
  an **optimistic compare-and-swap protocol**, not a retry that hides a race: the
  condition is decided under the lock, and a conflict means only that a checkpoint
  was committed between the check and the write. The service then takes a new
  snapshot and checks again, **at most 3 attempts in all** (checkpoints are minutes
  apart at the least, so a second conflict is already exceptional); when the
  attempts are exhausted it returns `Aborted` ("the commits of the review could not
  be checked against a stable snapshot: checkpoints were recorded concurrently";
  Gitaly's Aborted interceptor shows clients its generic "try again" message and
  keeps this one in the `error_details` metadata). Tested: success after exactly
  one conflict, and exhaustion. Comments, verdicts and closes do not move roots and
  are not checked.

  *As implemented (WP74).* `dbreview.Store` gains `Refs()` (the snapshot) and
  `WithRootsAt(S)` (a store whose `Create` and `Update` pass `rootsAt = S`); the
  review service detects this capability with an interface assertion, so the
  files store (storage core off or in shadow mode) is unchanged. The check runs
  inside the write, after the snapshot: `CreateReview` checks head and base;
  `UpdateReview` checks the new head inside the review store's update function,
  only when the head moves, so an update that keeps the head is not refused. If
  a reference tip of the snapshot is missing locally (a changeset committed on
  another node after the gate caught the repository up), the service calls
  `EnsureFresh` once before checking; afterwards a missing tip is `Internal`.
  The reachability test is `gitops.IsReachable`.
* **R2 — checkpoint commit.** The build records the root set it included (the
  open reviews' head and base commits, read after `Refs`). `CreateCheckpoint`, in
  its transaction under the repository row lock, requires every open review's head
  and base to be in that set, else `ErrCheckpointRootsChanged` and nothing is
  recorded (the CLI returns `FailedPrecondition` "the reviews changed during the
  checkpoint build, run it again"; the background task tries again at its next
  tick).
* **Proof.** Both conditions are checked under the same row lock, so a review
  write R and a checkpoint commit K are serialized. If R commits before K, R2
  makes K include R's roots. If R commits after K at checkpoint `N`, R1 gives `N
  <= S`, and R's roots were reachable from the references at `S`: either they are
  reachable at `N` (in K's closure), or the changesets `(N, S]` brought them into a
  pack `> N` (an object present locally but unreachable is packed again by the
  voter, whose `rev-list --not --all` excludes only reachable objects), which stays
  live until a later checkpoint whose R2 includes R.
* **LOCK ORDER (PM check 3).** Every transaction that must serialize with a
  changeset, a root-moving review write or a checkpoint commit takes the
  repository row (`SELECT ... FOR UPDATE` on Postgres; SQLite's immediate
  transaction holds the database write lock from `BEGIN`) **as its first
  statement**, before it reads or writes any child table: `CommitChangeset`,
  `RegisterRepository`, `ImportHistory`, `InsertReview`, `UpdateReview` with
  `rootsAt != 0`, `CreateCheckpoint` and the lease methods. Transactions that touch
  only child rows without changing their `repo_id` (`UpdateReview` with `rootsAt ==
  0`, `ReportApplied`, the orphan observations, `DeleteCheckpoint`) never lock the
  repository row and never upgrade a lock, so they cannot form a cycle with the
  first group. Insert-only child transactions take Postgres's implicit `KEY SHARE`
  on the parent and nothing after it. Tested with `-race` on Postgres and SQLite:
  concurrent lands, review creations and head moves, comments, and checkpoint
  commits on one repository, with no deadlock or serialization error surfacing
  (the retry of `withTx` is counted and must stay at zero for deadlocks), and every
  invariant checked at the end.
* **Reviews created before phase 7** may already reference a pruned commit (not
  possible after R1); so may reviews that `ImportHistory` brings in from the
  shadow-mode files. A checkpoint build whose open-review root is missing from the
  building node's repository fails with `FailedPrecondition` naming the review IDs
  (metadata `review_id`), counts
  `graft_storage_checkpoint_review_objects_missing_total`, and records nothing; GC
  of the existing checkpoints is unaffected. Closing the review resolves it.
* **Files.** `internal/gitaly/service/review/**` (the R1 check only, smallest
  change, with tests), `internal/graft/corestore/dbreview/**` (carries `S` to the
  store), `sqlstore` (the conditioned review writes and the R2 check).

### REVIEW ROOTS amendment: review roots are references (proposed after WP74, PM decision pending)

**Finding (WP74).** Gitaly housekeeping on a node keeps only what local
references reach. After its expiry it can drop the commit of an open review
whose branch was deleted. Checkpoint builds on that node then refuse with
`FailedPrecondition` until the review closes. Re-hydrating would loop:
housekeeping would prune the object again. The cause is the same as for R1/R2:
an open review is a root that neither the local repository nor the reference
graph of the storage core knows about. Every mechanism that decides liveness
(local housekeeping, the voter's `--not --all`, the checkpoint closure, GC)
respects references only.

**Facts in the code (evidence).**
1. Every reference write by a service goes through the voter:
   `updateref.Updater` always runs with `gitcmd.WithRefTxHook`
   (`internal/git/updateref/updateref.go:320`). The hook manager calls
   Preparing/Prepared for every reference transaction (`voter.go` header). So a
   ref the review service writes becomes a changeset with a database
   compare-and-swap. Its pack holds every object not reachable from the local
   references (`gitops.BuildPack`, `--not --all`), so a commit that only the
   cache has is uploaded with it.
2. The database records every reference name, while the Graft changelog records
   only branches and tags (`sqlstore/changeset.go:202`, `IsChangelogRef`;
   `changelog.ParseUpdates` names `refs/keep-around/*` as not recorded). A
   keep-around write therefore adds a changeset but no changelog entry and no
   `graft watch` event.
3. `refs/keep-around/` is an internal hidden prefix (`git/reference.go:62`,
   `InternalReferenceTypeHidden`). Gitaly adds it to `uploadpack.hideRefs` and
   `receive.hideRefs` (`gitcmd/command_description.go:453-484`), so clients
   never see it advertised and can neither push nor delete it.
   `ListRefs` shows it only to a caller whose patterns include it, as in
   upstream Gitaly. The graft CLI and the SDK list no references.
4. Hydration, catch-up and verify treat every database reference alike, so
   keep-around references reach every node through the normal path.

**Option (a), recommended: review roots are references.** A review's head and
base are protected by `refs/keep-around/<oid>` (target `<oid>`), written
through the ordinary write path (voter, database CAS, changeset).
* **Writes.** `CreateReview` writes the keep-around references of head and base
  (one reference transaction, which creates the absent ones), then inserts the
  review. `UpdateReview` writes the new head's reference, then updates the
  review. Comments, verdicts and closes write no reference. The request path
  never deletes one.
* **Invariant, in the database (authoritative mode).** "Every open review's head
  and base has its keep-around reference in `graft_refs`." Two checks, both
  under the repository row lock (LOCK ORDER), keep it:
  * `InsertReview` and `UpdateReview` with `requireRoots` (replacing `rootsAt`)
    require the references to exist with that value, else
    `ErrReviewRootsMissing`. The service then writes them again and retries, at
    most 3 attempts, then Aborted. This happens only if a sweep deleted the
    reference between the two steps.
  * `CommitChangeset` refuses to delete a `refs/keep-around/<oid>` that an open
    review uses (a reference conflict). This also protects them from
    `DeleteRefs`/`WriteRef` callers.
* **Deletion.** One path: a maintenance sweep (WP77) deletes keep-around
  references that no open review uses. The changeset constraint makes it safe
  against a concurrent creation. A leaked reference (crash between the steps,
  closed review) only keeps objects alive until the next sweep.
* **What this removes.**
  * R2: `CheckpointInput.Roots`, `checkReviewRoots`,
    `ErrCheckpointRootsChanged`.
  * The checkpoint build's review step: `ListReviews`, the head `H`, and the
    missing-review-commit path with its metric. The closure is "refs at N" and
    EnsureFresh needs only `>= N`.
  * R1's reachability check, its checkpoint-sequence condition (`rootsAt`,
    `ErrReviewRootsStale`), and `gitops.IsReachable`'s only caller.
  The roots rule becomes the general reference rule. A checkpoint can no
  longer be blocked by a review: a review whose commits are already lost
  simply has no reference.
* **What it adds.**
  * One reference transaction per review creation and per head move. Each
    produces one changeset, plus a pack only when the commit was not yet in
    the storage core.
  * The two database checks.
  * The sweep.
  * A backfill in maintenance for reviews written before this amendment or
    imported from files, which writes their references when the objects are
    present (a review whose objects are missing is logged and counted, and
    blocks nothing).
* **UX.** `CreateReview` on a commit that only the serving node has works again:
  the reference write uploads it. The STORAGE.md sentence added in WP74 ("push
  it first") is replaced.
* **Modes.** Off and shadow mode use the file review store. The service writes
  the same references there too: the same local protection against
  housekeeping, and in shadow mode the voter records them. The database
  invariant exists only in authoritative mode, where the database decides.
  This is one code path for all modes; in off mode it fixes the existing
  "verdict commit pruned after a force push" limitation for review heads.
* **Costs to measure (WP79).** The latency of a review creation (one extra
  reference transaction, comparable to a one-reference land). The number of
  changesets per review (1 per creation or head move, plus 1 per sweep batch).

**Option (b), rejected: local-only hidden keep-alive references.** References
present on each serving node but excluded from the database, from verify and
from the voter.
* It does not address liveness in the storage core. R1 and R2 stay, and so does
  everything this amendment removes.
* Every database-side reader of local references would need an exclusion: the
  voter's hook input, `ImportRepository`'s `ReadRefs` (it records every local
  reference), verify, the syncer's checks of local values, and the stale-cache
  checks.
* `--not --all` in the voter would count them as reachable. A later changeset's
  pack could then omit objects that only such a local reference protects:
  a new class of "present locally, not live in the storage core" objects.
* A node that hydrates later, or that has not served the repository since a
  review was created, has no such reference and does not learn about the
  review until a request arrives. Its housekeeping can prune in between,
  which is the same stall. A per-node rebuild of the references from the
  database on every hydration and catch-up would be needed, and it stays
  racy.

**Recommendation: (a).** It removes R2 and the checkpoint's review step
entirely, and replaces R1 by a reference-existence check. It uses only existing
mechanisms (voter, CAS, hidden refs, sweep) and fixes the housekeeping finding
on every node. Implementation would be a WP74b before WP76, replacing the
WP74 R1/R2 code (doc and tests included).

### Data model (new migration, both dialects)

Migration `20261018000000_graft_storage_gc`, appended to `sqliteMigrations` and
`postgresMigrations` in `internal/graft/corestore/sqlstore/migrations.go`. A
v0.1.0 database gets it at `Open`; with no checkpoints, observations or tombstones
every phase-7 path degrades to phase-6 behaviour.

```sql
-- A checkpoint: the consolidated pack and (graft_checkpoint_refs) the reference
-- snapshot at seq.
CREATE TABLE graft_checkpoints (
    repo_id    <int> NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
    seq        <int> NOT NULL,           -- 1..head_seq
    pack_key   TEXT  NOT NULL,           -- an owned key
    pack_bytes <int> NOT NULL DEFAULT 0,
    created_at <ts>  NOT NULL,           -- database clock
    PRIMARY KEY (repo_id, seq)
);

-- The reference snapshot at a checkpoint (no absent values). target is a hex
-- object ID or "ref:<name>", as in graft_refs.
CREATE TABLE graft_checkpoint_refs (
    repo_id <int> NOT NULL,
    seq     <int> NOT NULL,
    ordinal <int> NOT NULL,
    name    BLOB  NOT NULL,              -- BYTEA on Postgres
    target  TEXT  NOT NULL,
    PRIMARY KEY (repo_id, seq, ordinal),
    FOREIGN KEY (repo_id, seq) REFERENCES graft_checkpoints (repo_id, seq) ON DELETE CASCADE
);

-- The collector's observations of unreferenced packs (PACK OWNERSHIP RULE 4).
-- Written only by the collector, never on the write path.
CREATE TABLE graft_gc_orphans (
    repo_id    <int> NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
    pack_key   TEXT  NOT NULL,
    first_seen <ts>  NOT NULL,           -- database clock
    PRIMARY KEY (repo_id, pack_key)
);

-- One maintenance lease per repository (exclusivity and efficiency; GC and
-- checkpoint correctness do not depend on it, see change E).
CREATE TABLE graft_gc_leases (
    repo_id     <int> NOT NULL PRIMARY KEY REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
    holder      TEXT  NOT NULL,          -- node_id
    acquired_at <ts>  NOT NULL,          -- database clock
    expires_at  <ts>  NOT NULL           -- database clock
);

-- A tombstone of a removed repository, written in the transaction that deletes
-- the repository row. No foreign key: the row is gone.
CREATE TABLE graft_gc_removed (
    repo_id       <int> NOT NULL PRIMARY KEY,
    storage       TEXT  NOT NULL,        -- for logs
    relative_path TEXT  NOT NULL,        -- for logs
    removed_at    <ts>  NOT NULL         -- database clock
);
```

Plus the owned-key trigger (PACK OWNERSHIP RULE 3), both dialects:

```sql
-- SQLite
CREATE TRIGGER graft_changesets_owned_pack BEFORE INSERT ON graft_changesets
WHEN NEW.pack_key IS NOT NULL AND NEW.pack_key NOT LIKE 'repos/%/packs/%-%.pack'
BEGIN
    SELECT RAISE(ABORT, 'storage core: the pack key is not an owned key; every node must run the same version');
END;

-- Postgres
CREATE FUNCTION graft_changesets_owned_pack() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.pack_key IS NOT NULL AND NEW.pack_key NOT LIKE 'repos/%/packs/%-%.pack' THEN
        RAISE EXCEPTION 'storage core: the pack key is not an owned key; every node must run the same version'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER graft_changesets_owned_pack BEFORE INSERT ON graft_changesets
    FOR EACH ROW EXECUTE FUNCTION graft_changesets_owned_pack();
```

`<int>` = `INTEGER` on SQLite / `BIGINT` on Postgres; `<ts>` = RFC 3339 text in UTC
on SQLite / `TIMESTAMPTZ` on Postgres. The `Down` steps drop the trigger (and the
Postgres function) and the five tables. WP71 checks that sql-migrate executes the
trigger bodies (they contain `;` inside `BEGIN ... END` and `$$ ... $$`) as single
statements on both drivers, with a test.

### Types and pack keys (`internal/graft/corestore/types.go`)

```go
// Checkpoint is a consolidated state of a repository at a changeset sequence.
type Checkpoint struct {
    RepoID    int64
    Seq       uint64
    PackKey   string
    PackBytes int64
    CreatedAt time.Time // database clock
}

// RemovedRepository is a tombstone whose bucket prefix is still to be reclaimed.
type RemovedRepository struct {
    RepoID    int64
    Key       RepoKey
    RemovedAt time.Time // database clock
}

// GCState is what the collector decides on, read in one database snapshot.
type GCState struct {
    Now         time.Time            // database time of the snapshot
    Packs       map[string]uint64    // changeset pack key -> greatest referencing seq
    Checkpoints []Checkpoint         // ascending by Seq
    Orphans     map[string]time.Time // observed unreferenced key -> first_seen
}

var (
    ErrBlobExists             = errors.New("storage core: blob already exists")
    ErrCheckpointNotFound     = errors.New("storage core: checkpoint not found")
    ErrPackDeadline           = errors.New("storage core: the pack upload is older than the write deadline")
    ErrReviewRootsStale       = errors.New("storage core: a checkpoint was recorded after the review's commits were checked")
    ErrCheckpointRootsChanged = errors.New("storage core: the open reviews changed during the checkpoint build")
)

// NewPackKey returns a new owned pack key for a pack of the repository with the
// given SHA-256: "repos/<repo_id>/packs/<hex sha256>-<hex nonce>.pack", with a
// random 16-byte nonce.
func NewPackKey(repoID int64, sum [32]byte) string

// ParsePackKey returns the repository ID and the SHA-256 of an owned key or of a
// legacy key "repos/<repo_id>/packs/<hex sha256>.pack", and false for anything
// else. Only canonical spellings are accepted (lowercase hex, no leading zeros).
func ParsePackKey(key string) (repoID int64, sum [32]byte, ok bool)

// IsOwnedPackKey reports whether key is an owned key (not a legacy one).
func IsOwnedPackKey(key string) bool
```

`PackKey(repoID, sum)` (the legacy constructor) is removed: no caller may produce a
legacy key any more. Every call site (voter, `shadowpack.go`, import, tests) moves
to `NewPackKey`.

### BlobStore changes (`core.go`, `blobstore`)

```go
// BlobInfo describes a stored object.
type BlobInfo struct {
    Key  string
    Size int64
}

type BlobStore interface {
    // Put stores the content under key, which must not exist: an existing key is
    // ErrBlobExists and nothing is written. A pack key's content must hash to its
    // SHA-256 (ErrBlobCorrupt otherwise, nothing stored). A cancelled context
    // stores nothing.
    Put(ctx context.Context, key string, r io.Reader) error
    // Get, Exists, Close: unchanged.

    // List calls fn for every object whose key starts with prefix, in no defined
    // order, objects only (no directory entries). An error from fn stops the
    // iteration and is returned.
    List(ctx context.Context, prefix string, fn func(BlobInfo) error) error
    // Delete removes the object; a missing key is success.
    Delete(ctx context.Context, key string) error
}
```

**Atomic create-only on every driver (verified in the gocloud v0.46.0 sources).**
memblob checks and inserts under its bucket-wide mutex, so its `IfNotExist` is
atomic. fileblob is not: its writer's `Close` runs `os.Stat` then `os.Rename`
under a mutex created per writer (`fileblob.go` 764 and 789), which excludes
nothing, so two concurrent writers of one key both succeed and the second rename
overwrites the first; this holds within one process and between processes
sharing a file bucket. The phase-6 package doc's "fileblob supports IfNotExist" is
true only for sequential writes and is corrected. For file buckets `Put`
publishes the object itself with **link(2)**, the filesystem's own atomic
create-only primitive (PM review: an advisory lock binds only the writers that
honour it, and lock semantics vary on network filesystems, while a file bucket
shared by several hosts over NFS is a supported setup): fileblob's writer
creates the temporary file `<path>.<hex nanos>.tmp` in the key's directory (so
the key-to-path mapping stays fileblob's) and receives the content, `Put` fsyncs
it, `os.Link`s it to the key's path (EEXIST is `ErrBlobExists`), discards the
writer (whose cancelled `Close` only removes the temporary name) and fsyncs the
directories. A network filesystem can report EEXIST for a link that succeeded
but whose reply was lost and retransmitted; when the name is the same file as the
temporary one (`os.SameFile`), the publish was this writer's. No `.attrs`
sidecar is bypassed (file buckets are opened with `MetadataDontWrite`). File
buckets therefore need a filesystem with hard links (every local Linux/macOS
filesystem and NFS); one without them fails every `Put` with a clear error. The
flock version of 9c8bb58 is replaced. The upstream defect is a stated limit in
STORAGE.md. S3, GCS and Azure use conditional requests, atomic on the server
(untested here).

`List` uses `bucket.List(&blob.ListOptions{Prefix: prefix})` and skips `IsDir`
entries; `Delete` maps `gcerrors.NotFound` to success. fileblob lists every file
except its `.attrs` sidecars, **including the temporary file of a write in flight
and one that a crashed writer left behind** (`<path>.<nanos>.tmp`, in the key's
directory because the store opens file buckets with `NoTempDir`). The collector
therefore applies the orphan protocol to every unreferenced object under a
repository prefix, whether its key parses as a pack key or not: a temporary file
older than `gc_grace` belongs to a writer whose deadline has passed. `BlobInfo` carries no
modification time on purpose (the CLOCK RULE: GC never reads bucket times). A
file bucket may keep empty directories after the last key under them is deleted;
the keys, which are what matters, are gone (documented).

### MetaStore additions (`metastore.go`, `sqlstore`)

Implemented on both dialects with the phase-6 patterns (`bind`, `withTx` /
`withReadTx` with the busy/serialization retry, `timeArg`/`scanTime`,
`bytesArg`/`targetArg`, name chunks of `maxNamesPerQuery`).

```go
// CheckpointInput is the state CreateCheckpoint records.
type CheckpointInput struct {
    Seq       uint64            // 1..head_seq, greater than the newest checkpoint's Seq
    Refs      map[string]Target // the reference snapshot at Seq: no absent value, not empty
    PackKey   string            // an owned key, already uploaded
    PackBytes int64
    Roots     []string          // the open-review head and base commits the pack includes (REVIEW ROOTS R2)
}

type MetaStore interface {
    // ... phase-6 methods ...

    // CreateCheckpoint records a checkpoint and its snapshot in one transaction
    // with created_at = the database time. It locks the repository row and
    // requires Seq <= head_seq, Seq > the newest checkpoint's Seq and an owned
    // PackKey of the repository (else an error, nothing written). A checkpoint at
    // Seq that exists already is returned unchanged with created=false (its pack
    // is kept; the caller's pack becomes an orphan). It requires every open
    // review's head and base commit to be in in.Roots (REVIEW ROOTS R2), else
    // ErrCheckpointRootsChanged and nothing is written. ErrRepositoryNotFound for
    // an unknown repository.
    CreateCheckpoint(ctx context.Context, repoID int64, in CheckpointInput) (cp Checkpoint, created bool, err error)
    // NewestCheckpoint returns the checkpoint with the greatest Seq; ok is false
    // when there is none.
    NewestCheckpoint(ctx context.Context, repoID int64) (cp Checkpoint, ok bool, err error)
    // SyncBoundary returns the Seq of the newest checkpoint created at least age
    // ago on the database clock, 0 when there is none (the C_sync of the GC
    // BOUNDARY rule).
    SyncBoundary(ctx context.Context, repoID int64, age time.Duration) (uint64, error)
    // CheckpointRefs returns the snapshot of the checkpoint at seq;
    // ErrCheckpointNotFound when there is none.
    CheckpointRefs(ctx context.Context, repoID int64, seq uint64) (map[string]Target, error)
    // DeleteCheckpoint removes the checkpoint row and its snapshot; false when
    // there was none.
    DeleteCheckpoint(ctx context.Context, repoID int64, seq uint64) (bool, error)

    // GCSnapshot reads, in one read transaction, the database time, the changeset
    // pack keys with their greatest referencing sequence, the checkpoints and the
    // orphan observations of the repository. ErrRepositoryNotFound if unknown.
    GCSnapshot(ctx context.Context, repoID int64) (GCState, error)
    // RecordOrphans stores first_seen = the database time for every key that has
    // no observation yet (existing observations are kept).
    RecordOrphans(ctx context.Context, repoID int64, keys []string) error
    // ForgetOrphans removes the observations of the keys (deleted, or referenced
    // since they were observed).
    ForgetOrphans(ctx context.Context, repoID int64, keys []string) error

    // AcquireMaintenanceLease takes the lease for holder until the database time
    // plus ttl, by compare-and-swap: it succeeds when there is no lease, the lease
    // has expired, or holder holds it already. RenewMaintenanceLease extends it
    // when holder still holds it (held=false otherwise). ReleaseMaintenanceLease
    // frees it when holder holds it. They never bar a read or a write.
    AcquireMaintenanceLease(ctx context.Context, repoID int64, holder string, ttl time.Duration) (acquired bool, err error)
    RenewMaintenanceLease(ctx context.Context, repoID int64, holder string, ttl time.Duration) (held bool, err error)
    ReleaseMaintenanceLease(ctx context.Context, repoID int64, holder string) error

    // ListRepositories returns every repository of the given storage whose import
    // is complete (head_seq > 0), ascending by ID. An empty storage lists the
    // repositories of every storage.
    ListRepositories(ctx context.Context, storage string) ([]Repository, error)

    // ListRemovedRepositories returns the tombstones of the given storage (every
    // storage when empty) whose removed_at is at least minAge ago on the database
    // clock (change F).
    ListRemovedRepositories(ctx context.Context, storage string, minAge time.Duration) ([]RemovedRepository, error)
    // DeleteRemovedRepository removes a tombstone; idempotent.
    DeleteRemovedRepository(ctx context.Context, repoID int64) error
}
```

**InsertReview and UpdateReview (changed, REVIEW ROOTS R1).** Both take a new
last parameter `rootsAt uint64`. When it is not 0, the transaction locks the
repository row and requires the newest checkpoint's sequence to be `<= rootsAt`,
else `ErrReviewRootsStale` and nothing is written. Callers that do not move a
review's roots pass 0 (comments, verdicts, closes, imports). `dbreview` carries the
value from the review service to the store.

**DeleteRepository (changed).** In the transaction that deletes the repository row
it reads the row's `repo_id` and inserts the `graft_gc_removed` tombstone with
`removed_at` = the database time. The cascade removes changesets, references,
changelog, reviews, checkpoints, observations and the lease; the tombstone stays.
An unknown key writes nothing and returns false, as before. Signature unchanged.

### Core additions (`core.go`, `open/open.go`)

`corestore.Core` gains `GCGrace` and `GCMargin time.Duration` (from the config,
also in shadow mode: GC runs there too, so the writers' deadline applies) and the
method `func (c *Core) WriteDeadline() time.Duration` = `GCGrace − GCMargin` (the
`W` of the PACK OWNERSHIP RULE).

### gitops addition

```go
// BuildPackFrom writes a self-contained (non-thin) pack of every object
// reachable from tips: `git rev-list --objects --stdin` (the tips on stdin, no
// --not) piped into `git pack-objects --stdout`, hashed into a file in tmpDir.
// Zero tips are ignored; no tips is an empty Pack. A tip that is not in the
// repository is an error: callers check the tips with MissingObjects first, so
// that a missing object is reported precisely and never yields a partial pack.
func BuildPackFrom(ctx context.Context, repo *localrepo.Repo, tips []git.ObjectID, tmpDir string) (Pack, error)

// MissingObjects returns the tips that are not in the repository, in the order
// given (one `cat-file --batch-check` run).
func MissingObjects(ctx context.Context, repo *localrepo.Repo, tips []git.ObjectID) ([]git.ObjectID, error)

// IsReachable reports whether commit is reachable from one of tips (one
// `rev-list --stdin --max-count=1` run with the tips negated; tips that are
// blobs or trees reach nothing, annotated tags are peeled). For REVIEW ROOTS R1.
func IsReachable(ctx context.Context, repo *localrepo.Repo, commit git.ObjectID, tips []git.ObjectID) (bool, error)
```

(WP74: an earlier draft had BuildPackFrom report a missing tip through an
`IsMissingObject` predicate; the build order checks with MissingObjects before
any pack is built, so no caller needs to tell that error apart.)

### Writers (WP73, change A on the write path)

**One upload helper (binding).** Every path that uploads a pack and then records
a row (the voter's Preparing and Prepared phases, `ImportRepository`, the
checkpoint builder; no other production code calls `Put`) goes through

```go
// PackUpload is a pack uploaded under a new owned key, with the deadline before
// which a row may reference it (PACK OWNERSHIP RULE 4).
type PackUpload struct {
    Key      string
    Bytes    int64
    Deadline time.Time // readAt + WriteDeadline(), monotonic
}

// UploadPack uploads the pack read from r (size bytes, SHA-256 sum) under
// NewPackKey(repoID, sum), with the Put under a context whose deadline is
// readAt + WriteDeadline(). readAt is the time of a repository-record read that
// found the repository, taken before the upload.
func (c *Core) UploadPack(ctx context.Context, repoID int64, readAt time.Time, sum [32]byte, size int64, r io.Reader) (PackUpload, error)

// CommitContext returns ctx bounded by the upload's deadline, or an error
// (ErrPackDeadline) when the deadline has passed: the caller must not start the
// commit and must not reference the key.
func (u PackUpload) CommitContext(ctx context.Context) (context.Context, context.CancelFunc, error)
```

Each path is tested past `W` (with a shortened `gc_grace` or a hook): the `Put` is
cancelled or the commit is not started, the write fails as a storage failure, and
no row references the key.

* **Voter.** `preparePack` records `t_r` after its `GetRepository` and uploads with
  `NewPackKey` under a context with deadline `t_r + W`; `preparedPack` keeps the
  deadline. `prepare` (Prepared) uses the prepared pack only when the repository ID
  matches **and** its deadline has not passed; otherwise it calls `buildAndUpload`
  with a new `t_r` from the record it has just read. The `CommitChangeset` call runs
  under the pack's deadline (the earlier of the request context and `t_r + W`). A
  transaction without a pack has no deadline. Shadow mode: the same, inside the
  shadow timeout (which is far shorter than `W`).
* **ImportRepository.** After building the full pack it re-reads the record
  (`GetRepository`, which also confirms the registration still exists), uploads
  with `NewPackKey` and commits changeset 1 under `t_r + W`. A resumed import uploads
  a new owned pack; the pack of the interrupted attempt is an orphan.
* **Checkpoint builder.** See below; same rule.

### Checkpoint creation (`internal/graft/corestore/checkpoint`)

```go
func New(core *corestore.Core, logger log.Logger, locator storage.Locator,
    repoFactory localrepo.Factory, fresh corestore.Freshener) *Checkpointer

// Create builds a checkpoint of repo at the current database head and records
// it. created is false when a checkpoint at the head exists already.
func (c *Checkpointer) Create(ctx context.Context, repo *gitalypb.Repository) (cp corestore.Checkpoint, created bool, err error)
```

The fault hook is an unexported field of the `Checkpointer` that tests set
(`export_test.go`), like the syncer's test hook, not a package variable, so
that parallel tests do not share it. Its points are `after_snapshot` (after
step 1: a test writes a review at `S > N` there), `after_upload` and
`after_commit`. `Create` takes `*gitalypb.Repository` because the repository
factory builds from it; both callers (the RPC handler and the maintenance
runner) have one.

Binding order:

1. Require authoritative mode (else `FailedPrecondition`, SHADOW SCOPE). Read
   `(refs, N) = Meta.Refs(repoID)` (one snapshot). If `NewestCheckpoint` is at
   `N`: return it, `created = false`. (WP74: the reftable check of the draft is
   dropped. The local repository may not exist before step 3, and EnsureFresh
   replaces a copy that is not reftable, so the check could only refuse a node
   that has no copy yet. The build only reads objects, which does not depend on
   the reference backend. The object hash comes from the repository record.)
2. **Roots before freshness (PM check 1).** Read the open reviews
   (`ListReviews(repoID, "", false, 0)`): their `head_commit_id` and
   `base_commit_id` (each validated as an object ID of the repository's hash;
   empty values skipped) are the build's root set (`Roots`). Then read the record
   again: its head `H` (`>= N`) is at least the snapshot sequence `S` of every review
   read, because each of them was written after its own check at `S` and before
   this read. A review's roots can come from changesets in `(N, S]`, so the
   reference snapshot `N` alone does not tell which objects the builder needs.
3. **Change D:** `EnsureFresh(repo)`. EnsureFresh catches up to a head read after
   the call arrived, which is `>= H`; then read the local watermark and require
   `wm.RepoID == repoID && wm.Seq >= H` (else `Internal`: the Freshener contract is
   broken, never retried around). The local repository now holds every object of
   the changesets `<= H`, hence every object reachable from `refs` and every root
   of a review checked at `S <= H`.
4. **All objects present, or a precise error, never a partial pack.**
   `MissingObjects` over the reference tips and the roots, before any pack is built:
   a missing reference tip is impossible after step 3 and is `Internal` naming the
   object; a missing review root can only belong to a review written without R1
   (created before phase 7, or brought in by `ImportHistory` from the shadow-mode
   files) and is `FailedPrecondition` naming the review IDs, with the metric;
   nothing is uploaded. A review commit that is not an object ID of the
   repository's hash cannot be packed and is refused the same way.
5. `pack = BuildPackFrom(local, tips ∪ roots, tmpDir)`. No object tips → return
   `created = false` (nothing to consolidate; hydration of such a repository is
   already trivial).
6. Re-read the record (`t_r`), `Core.UploadPack`. `//! after_upload`.
7. `CreateCheckpoint(repoID, {Seq: N, Refs: refs, PackKey, PackBytes, Roots})`
   under the upload's `CommitContext`. `ErrCheckpointRootsChanged` (a review moved
   its head after step 2) → the build fails with `FailedPrecondition` (its pack is
   an orphan). `//! after_commit`.

The roots of a review written after step 2 are not in `Roots`: either R2 refuses the
checkpoint (the review committed before step 7) or R1 makes the review's snapshot
`>= N` (it committed after step 7), so its roots are live without this checkpoint.

A crash after 5 leaves the pack as an orphan (reclaimed by the orphan protocol); a
re-run starts over from the head at that time. The build holds no reference lock
and never blocks or fails a write (CKPT-2).

**Change E (fencing).** `CreateCheckpoint` does **not** check the maintenance lease
in its transaction, and checkpoint correctness does not rely on the orphan grace
being a guess either: it relies on (a) per-sequence idempotency (a second build
at the same `N` returns the existing checkpoint, its own owned pack becomes an
orphan), (b) monotonic sequences (a checkpoint at a smaller `N` than the newest
is refused), and (c) the enforced orphan protocol for the loser's pack. Two
checkpoints at different sequences built by two nodes whose leases overlapped are
both valid states. The lease only prevents wasted builds.

### Pack GC (`internal/graft/corestore/gc`)

```go
func New(core *corestore.Core, logger log.Logger) *Collector

type Report struct {
    Boundary           uint64 // C_gc
    CoveredDeleted     int
    CheckpointsDropped int
    OrphansDeleted     int
    OrphansObserved    int    // newly observed, kept for now
    OrphansInGrace     int    // observed earlier, not old enough yet
    PacksDeleted       int
    BytesReclaimed     int64  // sizes from List
    RemovedRepos       int
    DryRun             bool
}

// CollectRepository reclaims the dead packs of one repository. The caller holds
// the repository's maintenance lease (efficiency only).
func (c *Collector) CollectRepository(ctx context.Context, repoID int64, dryRun bool) (Report, error)

// CollectRemoved reclaims the bucket prefixes of the tombstones of the storage
// (every storage when empty) that are older than gc_grace, then deletes the
// tombstones.
func (c *Collector) CollectRemoved(ctx context.Context, storage string, dryRun bool) (Report, error)

// FaultHook is called at "after_drop_row", "after_delete", "mid_sweep" and
// "after_prefix" (tests only).
var FaultHook func(point string) error
```

`CollectRepository` (binding):

1. `st = GCSnapshot(repoID)`. `C_gc` = the greatest `Seq` among `st.Checkpoints`
   with `CreatedAt <= st.Now − gc_grace` (0 when none).
2. **Drop superseded checkpoints:** for each checkpoint with `Seq < C_gc`:
   `DeleteCheckpoint` (row first), `//! after_drop_row`, then `Blobs.Delete` of its
   pack (owned by that row only, and no reader uses a checkpoint below `C_gc`).
   A crash in between leaves an unreferenced pack, reclaimed by the orphan protocol.
3. **Live set** (per key, change G): kept checkpoint packs (`Seq >= C_gc`) and
   every key of `st.Packs` whose greatest sequence is `> C_gc`. **Covered set:**
   keys of `st.Packs` whose greatest sequence is `<= C_gc`, minus the live set.
4. **Sweep** `Blobs.List("repos/<repoID>/packs/")`, for each key:
   * live → keep;
   * covered → `Blobs.Delete`;
   * referenced by no row (a pack key or any other object under the prefix, such
     as a file bucket's leftover temporary file): if `st.Orphans[key] <= st.Now − gc_grace` →
     `Blobs.Delete` and then forget the observation; if it has a younger
     observation → keep (in grace); if it has none → collect it for
     `RecordOrphans`.
   `//! mid_sweep` after the first deletion. Finally `RecordOrphans(new)` and
   `ForgetOrphans` for the observations whose key is now referenced or was not
   listed.
5. Dry run: steps 1 and 3, and the sweep only counts (no delete, no observation
   written).

Every decision comes from one snapshot and the PACK OWNERSHIP proof; `Delete` of a
missing key is success; a crashed run converges on the next one. Two collectors
racing on one repository (lease expired) both make only safe decisions: deletes
are idempotent, observations are inserted with "do nothing on conflict".

### Removed repositories (change F)

`CollectRemoved`: `ListRemovedRepositories(storage, gc_grace)`; for each tombstone,
`Blobs.List("repos/<repo_id>/")` and delete every key, `//! after_prefix`, then
`DeleteRemovedRepository`. A tombstone is swept only once `removed_at` is at least
`gc_grace` old: a writer whose record read found the repository did so before
`removed_at`, and its upload is bounded by `t_r + W`, so no upload can arrive in the
prefix after the sweep (the PACK OWNERSHIP RULE makes this an enforced bound, not
an assumption). A crash after a partial prefix deletion leaves the tombstone; the
next run finishes it. No lease (the repository is gone and its ID is never reused).

### Hydration from a checkpoint (syncer, WP76)

`hydrate` seeds from `NewestCheckpoint`:

1. Seed `S` = the newest checkpoint's `Seq` and its snapshot (`CheckpointRefs`),
   or `S = 0` without one.
2. `InitBare`; when seeded, download, verify (SHA-256 against the key, `repo_id`
   of the key) and index the checkpoint pack, and validate every name and target of
   the snapshot (RULE 22).
3. `f = newFetched(S)`; `fetch(..., f, r.HeadSeq)` downloads and indexes the packs
   `(S, head]` and folds their reference changes.
4. Final references = the snapshot, overwritten by `f`'s final values (an absent
   final value deletes); `applyBuild` once; watermark `{r.ID, f.head}`; `install`.

A missing checkpoint pack or a missing pack `> S` is an Internal error naming the
key (corruption, GC BOUNDARY rule).

### Catch-up and the sync boundary (syncer, WP76, change C)

In `sync()`, each round reads `C_sync = SyncBoundary(r.ID, gc_grace − sync_timeout −
gc_margin)` and hydrates when `!ok || wm.RepoID != r.ID || !localCurrent ||
wm.Seq < C_sync`; otherwise it catches up as in phase 6 (the WP55 RULE unchanged).
`checkReplaceable` still runs before any hydration.

### Verify after compaction (`verify_objects.go`, WP76)

`checkStoredObjects` indexes the newest checkpoint's pack and the changeset packs
`(S, head]` (all packs `1..head` without a checkpoint), then runs the reachability
check as today, and also checks the open reviews' head and base commits. It runs
under a deadline of `W` from its start (a dry hydration that read checkpoint `S`
must end before a checkpoint newer than `S` can become `C_gc`); a missing pack is
reported as a connectivity failure starting with `storage core objects:`.

### Background maintenance (`internal/graft/corestore/maintenance`, WP77)

```go
func New(core *corestore.Core, cp *checkpoint.Checkpointer, collector *gc.Collector,
    fresh corestore.Freshener, locator storage.Locator, logger log.Logger, cfg Config) *Runner
func (r *Runner) Start(ctx context.Context) (stop func())
```

**Off by default** (answer 3): it runs only with `maintenance = "auto"` and in
authoritative mode; the CLI is always available. Each tick (`maintenance_interval`):
`CollectRemoved` for this node's storages; then, for each repository of
`ListRepositories` that this node has a current local copy of:
`AcquireMaintenanceLease` (skip when held elsewhere), checkpoint when
`head_seq − newest checkpoint seq >= checkpoint_changesets` or the pack bytes of
the changesets after the newest checkpoint `>= checkpoint_pack_bytes`, then
`CollectRepository`, renewing during a long build and releasing at the end. Wired in
`serve.go` (the storage core block only), started with the server and stopped on
shutdown.

### Configuration (`config_graft_storage.go`, my section)

```toml
[graft.storage]
# ... mode, database, bucket, node_id, sync_timeout, shadow_timeout ...
maintenance           = "off"      # off | auto: background checkpoints and GC (authoritative only). Default off.
maintenance_interval  = "1h"       # how often the background task runs
checkpoint_changesets = 1000       # checkpoint after this many changesets since the newest one (0 = never)
checkpoint_pack_bytes = 268435456  # ...or after this many pack bytes since the newest one (0 = never)
gc_grace              = "1h"       # age before an unreferenced pack is deleted and a checkpoint becomes C_gc
gc_margin             = "5m"       # safety margin of the writers' deadline (gc_grace - gc_margin) and the boundaries
lease_ttl             = "10m"      # maintenance lease duration, renewed during a build
```

Defaults are `Default*` constants with the values above. Validation (extends
`GraftStorage.validate`): `maintenance in {"", "off", "auto"}`; durations positive;
thresholds `>= 0`; `gc_margin < gc_grace`; **`gc_grace > sync_timeout + gc_margin`**
(GC BOUNDARY rule). `checkpoint_retain` of the first version does not exist.

### RPCs (`proto/storage_core.proto`)

`StorageCoreService` is in the gate's `Passthrough`, so these RPCs are not
intercepted and need no `AuthoritativeMutators` entry.

```proto
// CreateCheckpoint builds a consolidated checkpoint of the repository at the
// current head and records it. Authoritative mode only.
rpc CreateCheckpoint(CreateCheckpointRequest) returns (CreateCheckpointResponse) {
  option (op_type) = { op: MUTATOR };
}
// CollectGarbage reclaims the packs this repository no longer needs: orphans
// observed at least gc_grace ago, packs covered by a checkpoint at least gc_grace
// old, and superseded checkpoints. It does not touch other repositories.
rpc CollectGarbage(CollectGarbageRequest) returns (CollectGarbageResponse) {
  option (op_type) = { op: MUTATOR };
}
// CollectRemovedRepositories reclaims the packs of the repositories of a storage
// that were removed at least gc_grace ago.
rpc CollectRemovedRepositories(CollectRemovedRepositoriesRequest) returns (CollectRemovedRepositoriesResponse) {
  option (op_type) = { op: MUTATOR scope_level: STORAGE };
}

message CreateCheckpointRequest  { Repository repository = 1 [(target_repository)=true]; }
message CreateCheckpointResponse {
  uint64 checkpoint_sequence = 1;
  bool   created             = 2; // false: a checkpoint at the head existed already
  int64  pack_bytes          = 3;
  uint32 references          = 4;
  uint32 review_commits      = 5; // open-review head/base commits included
}

message CollectGarbageRequest {
  Repository repository = 1 [(target_repository)=true];
  bool       dry_run    = 2;
}
message CollectGarbageResponse {
  uint64 boundary            = 1; // C_gc
  uint32 covered_deleted     = 2;
  uint32 checkpoints_dropped = 3;
  uint32 orphans_deleted     = 4;
  uint32 orphans_observed    = 5;
  uint32 orphans_in_grace    = 6;
  uint32 packs_deleted       = 7;
  int64  bytes_reclaimed     = 8;
  bool   dry_run             = 9;
}

message CollectRemovedRepositoriesRequest {
  string storage_name = 1 [(storage)=true];
  bool   dry_run      = 2;
}
message CollectRemovedRepositoriesResponse {
  uint32 repositories    = 1;
  uint32 packs_deleted   = 2;
  int64  bytes_reclaimed = 3;
  bool   dry_run         = 4;
}
```

Handlers (`service/storagecore/{checkpoint,gc}.go`): off → `FailedPrecondition`;
`CreateCheckpoint` in shadow → `FailedPrecondition`; `CollectGarbage` acquires the
lease (held elsewhere → `FailedPrecondition` "another maintenance operation of
this repository is in progress"), collects, releases. WP78 checks the exact
`op_type` options the repository's proto lint accepts for a storage-scoped mutator
and follows the existing storage-scoped RPCs.

### SDK and CLI

SDK (`sdk/storage_gc.go`): `func (r *Repo) StorageCheckpoint(ctx)
(*StorageCheckpoint, error)`, `func (r *Repo) StorageGC(ctx, dryRun bool)
(*StorageGC, error)`, and `func (c *Client) StorageGCRemoved(ctx, storage string,
dryRun bool) (*StorageGCRemoved, error)`.

CLI (`cmd/graft/storage.go`):

```
graft storage checkpoint [-repo R] [--json]
graft storage gc [-repo R] [--dry-run] [--json]
graft storage gc --removed [-storage S] [--dry-run] [--json]
```

`gc -repo R` touches only that repository (answer 1); `--removed` is the storage-wide
sweep of removed repositories and takes no `-repo`.

## Rejected alternatives

* **Database fencing of deletions** (a `graft_gc_doomed` set serialized with
  `CommitChangeset`, direction 2 of the PM review). It keeps objects shared between
  rows and adds a lookup to every commit under the reftable lock. It is not complete
  by itself: after the collector deletes the blob and removes the intent, a writer
  whose `Put` skipped the key before the deletion can commit with neither the intent
  nor the blob present, so the writer needs an additional protocol (re-upload after
  the intent clears, and keep re-checking) whose correctness depends on the order of
  bucket operations outside the transaction. The in-flight orphans of each write still
  need a time rule on top. Ownership removes the sharing instead, needs no write-path
  database access, and makes the in-flight bound an enforced deadline.
* **Bucket modification times for orphans** (the first version). An object's mtime
  is set by the bucket provider's clock, with no bound on skew against the
  collector, untested for S3/GCS/Azure here, and under the old `Put` an existing
  object's mtime is not refreshed. Replaced by the collector's own observations on
  the database clock.
* **A pending record per in-flight upload.** Rigorous, but one more database write
  per transaction on the write path (RULE 4), and it still needs an expiry for
  records of dead writers, i.e. the same deadline. The deadline alone suffices.
* **Restart a sync when a covered pack is missing** (B of the first review).
  Under the GC BOUNDARY rule a sync never meets a reclaimed pack, so this path would
  be unreachable; a missing pack is corruption and is reported as such.
* **`checkpoint_retain`.** The lagging boundary already keeps every checkpoint a
  reader can use; a count adds nothing.
* **Fold the reference snapshot from changeset metadata at hydration time**: `O(N)`
  rows per hydration; the snapshot table is `O(refs)` rows written once.
* **Delete changeset and changelog rows below a checkpoint**: the changelog cursors
  must stay valid and the rows are cheap.
* **Additive catch-up from a checkpoint** (index the checkpoint pack into a
  far-behind cache): entangles the seed with the WP55 compare-and-swap logic;
  re-hydration is correct and the case is rare (the node lagged by about
  `gc_grace − sync_timeout`).
* **Carry unreachable objects into the checkpoint pack**: defeats compaction.
* **Full-bucket scan to find removed repositories**: `O(objects)` and racy; the
  tombstone is crash-safe and bounded.

## Invariants (binding; each has a test)

* **OWN-1.** Every pack object is written by exactly one writer and referenced by at
  most one row (legacy keys excepted); `Put` never succeeds on an existing key.
* **OWN-2.** A writer commits a row referencing its pack only before `t_r + W`; no
  writer of a not-upgraded binary can commit a pack-carrying changeset (trigger).
* **I1 (preserved).** Every object reachable from the current references, and the
  head and base commits of the open reviews at the last checkpoint, is in a kept
  pack.
* **GC-1.** The collector never deletes a pack referenced by a changeset with
  `seq > C_gc` or a checkpoint with `seq >= C_gc`.
* **GC-2.** The collector never deletes a pack a write in progress may still commit
  (PACK OWNERSHIP proof).
* **GC-3.** No sync round and no verification ever needs a pack the collector
  deletes during it (GC BOUNDARY rule).
* **GC-4.** At most one checkpoint build or collection per repository at a time
  under normal operation (the lease); correctness does not depend on it.
* **GC-5.** A node whose watermark is below `C_sync` re-hydrates from the newest
  checkpoint; a node at or above it catches up without re-hydrating.
* **CKPT-1.** Hydrating from a checkpoint at `N` and replaying `(N, head]` yields
  exactly the state at head.
* **CKPT-2.** Building a checkpoint never blocks or fails a write.
* **REM-1.** A removed repository's prefix is swept only when its tombstone is at
  least `gc_grace` old, and no upload can arrive in it afterwards.
* **COMPAT-1.** A v0.1.0 database works after migration 2: no checkpoints means
  replay from 1; legacy keys stay readable and collectable.
* **CRASH/IDEMPOTENT.** Every GC and checkpoint step is crash-safe and a re-run
  converges; the database decides liveness.
* **HISTORY.** No changeset, changelog or review row is deleted by compaction.

## Crash and race cases (each tested)

1. Two writes with byte-identical packs (the dedup race of the review): each uploads
   its own key; the collector deletes the first's covered key; the second write's
   changeset and pack are intact (test with a fault hook between the second write's
   upload and its commit, the collector deleting in between, SQLite and Postgres).
2. A write whose upload is older than its deadline at Prepared time: it rebuilds and
   re-uploads a new owned pack; one whose commit cannot start before the deadline
   fails like a storage failure (test with a shortened `gc_grace`).
3. An unreferenced pack observed, then committed by its writer before its deadline:
   the next run forgets the observation and keeps the pack.
4. A not-upgraded writer inserts a legacy key: the trigger rejects it (test inserts a
   changeset with a legacy key through `CommitChangeset` bypassing `NewPackKey`).
5. Checkpoint crash after the upload, before the row: the pack is an orphan; a re-run
   commits a new checkpoint.
6. Collector crash after `DeleteCheckpoint`, before the pack deletion; mid-sweep;
   after a partial prefix deletion of a removed repository: the next run converges.
7. A node one changeset behind when a checkpoint is created catches up and does not
   re-hydrate (change C).
8. A node whose watermark is below `C_sync` re-hydrates and reaches head.
9. Hydration while the collector runs; GC concurrent with continuous lands; a
   checkpoint build concurrent with lands (all `-race`).
10. A checkpoint build on a copy that lags behind the head (change D): EnsureFresh
    brings it to `>= N` and the checkpoint is complete.
11. A write in flight on a repository being removed: its pack lands before the sweep
    (bound `t_r + W`), the sweep at `removed_at + gc_grace` deletes it.
12. Two nodes race for the lease: one wins; a holder that crashes lets the lease
    expire after `lease_ttl`.
13. An open review whose commits are missing locally: the checkpoint fails with
    `FailedPrecondition` naming the review; GC still runs.

## Test plan (multilevel; SQLite and Postgres wherever a store is involved)

* **sqlstore unit** (both dialects): every new method; `CreateCheckpoint`
  monotonicity, idempotency (`created=false`), owned-key and `seq <= head`
  checks, snapshot round trip with non-UTF-8 names and symbolic targets;
  `GCSnapshot` maxima per key with legacy shared keys; `RecordOrphans` keeps an
  existing `first_seen`; lease CAS between two `Store`s and expiry on the database
  clock; `DeleteRepository` tombstone and cascade; `ListRemovedRepositories` age
  filter; `SyncBoundary`; the trigger rejects a legacy key and accepts an owned one
  and NULL; migration 2 on a migration-1 database (fixture) and the trigger bodies
  executed as single statements.
* **blobstore unit** (file and mem): `Put` on an existing key is `ErrBlobExists` and
  leaves the content unchanged; concurrent `Put`s of one key: exactly one wins;
  `List` by prefix, sizes, no directory entries; `Delete` idempotent.
* **types unit**: `NewPackKey`/`ParsePackKey`/`IsOwnedPackKey` round trips, legacy
  parsing, non-canonical spellings refused, nonce uniqueness.
* **gitops unit** (SHA-1 and SHA-256): `BuildPackFrom` is self-contained (`fsck` after
  indexing into an empty repository); `MissingObjects`.
* **voter/import unit**: owned keys on every upload; the deadline (prepared pack
  expired → rebuilt; commit past the deadline → refused); the phase-6 voter, import,
  syncer, gate and storagecore suites stay green.
* **checkpoint unit**: hydration from a checkpoint equals the source at head
  (references, `fsck`); open-review commits survive the deletion of their branch;
  closed-review commits do not (documented limit); lagging copy (change D); missing
  review objects → `FailedPrecondition`; `created=false`.
* **gc unit**: every branch of the sweep; covered vs live per key; dropped
  checkpoints; observations (new, in grace, expired, referenced since); removed
  repositories younger and older than `gc_grace`; dry run deletes and writes
  nothing; re-run is a no-op.
* **-race**: crash/race cases 1, 9 and 12.
* **crash injection**: every `FaultHook` point of the checkpoint and gc packages,
  and the voter's `after_upload`, each followed by a re-run that converges and a
  `VerifyRepository --connectivity` that passes.
* **e2e** (`internal/graft/e2e/storage_gc_test.go`, two and three real gitaly
  processes, one database and one bucket, SQLite and Postgres): writes on A,
  `graft storage checkpoint`, `graft storage gc` with a shortened `gc_grace`, a fresh
  node B hydrates from the checkpoint (asserted from the downloaded keys and the
  `graft_storage_sync_*` metrics), serves, verify is consistent; a node below
  `C_sync` re-hydrates; `graft storage gc --removed` reclaims a removed repository;
  changelog and reviews intact.
* **binding tests of the second review** (each on SQLite and Postgres):
  * the deadline on every upload path (voter Preparing, voter Prepared, import,
    checkpoint): past `W` the `Put` is cancelled or the commit is not started, the
    write fails as a storage failure, and no row references the key;
  * `graft_gc_orphans` lifecycle: an observed key that a slow write then committed is
    forgotten by the next run and kept; an observed key gone from the bucket is
    forgotten; a crash between the blob deletion and the row deletion converges;
  * the trigger: a legacy-format insert is rejected with the documented error, an
    owned key and NULL are accepted, legacy rows stay readable, and GC uses the
    greatest sequence of a shared legacy key;
  * REVIEW ROOTS: R1 rejects an unreachable head or base; R1's stale snapshot (a
    checkpoint committed between the check and the write) is detected under the lock
    and re-checked; R2 refuses a checkpoint whose build missed a review moved during
    the build;
  * a sync round slower than `sync_timeout` and a dry hydration slower than `W` are
    cancelled;
  * (third review) a checkpoint build on a node whose watermark is below the
    snapshot `S` of an open review created at `S > N` includes that review's roots
    (EnsureFresh to `H`); a root missing locally is a precise error and no pack is
    uploaded; R1 succeeds after exactly one `ErrReviewRootsStale` and returns
    `Aborted` after 3; the LOCK ORDER `-race` test on Postgres and SQLite.
* **measurement** (WP79, in the benchmark window the PM schedules): bucket size and
  fresh-node hydration time before and after a checkpoint and GC on a repository
  with many changesets; the preparing/prepared histograms before and after WP73.

## Acceptance criteria

* After a checkpoint and GC the bucket holds the kept checkpoint packs and the packs
  after `C_gc`, not the whole history; orphans and removed-repository packs are gone
  (measured).
* Fresh-node hydration time depends on `head − newest checkpoint`, not on `head`
  (measured).
* `graft storage verify --connectivity` is consistent after GC and compaction;
  `graft log`, `WatchChangesets` and reviews are unchanged.
* A v0.1.0 database migrates and every storage path works.
* Every phase-6 storage test is green (the blobstore `Put` tests rewritten to the
  new contract, stated in the report); the new tests pass on SQLite and Postgres.
* No live pack is ever deleted (OWN-1, OWN-2, GC-1..GC-3), proven by the race and
  crash tests.

## SHADOW SCOPE rule (binding)

Checkpoints and the background task are authoritative-mode only: a checkpoint seeds
hydration, which does not happen in shadow mode, and its snapshot is the database's
view, which is not authoritative there. `CollectGarbage` and
`CollectRemovedRepositories` also run in shadow mode (orphans and removed
repositories only; no checkpoints means no covered set). The pack ownership rule,
the trigger and the writers' deadline apply in both modes. Off mode opens no bucket:
every entry point returns `FailedPrecondition`.

## Work packages and split

| WP | content | depends on |
|---|---|---|
| WP71 | migration 2 (tables, trigger), every new `sqlstore` method, `DeleteRepository` tombstone | — |
| WP72 | `BlobStore` `List`/`Delete`, strict `Put`, owned pack keys in `types.go` | — |
| WP73 | writers: owned keys and the deadline in the voter, `shadowpack.go`, import; `Core` fields | WP71, WP72 |
| WP74 | `gitops.BuildPackFrom`/`MissingObjects`, `checkpoint` package, REVIEW ROOTS (R1 in the review service and `dbreview`, R2 in `CreateCheckpoint`) | WP71–WP73 |
| WP75 | `gc` package | WP71–WP73 |
| WP76 | syncer (seed, `C_sync`), `verify_objects.go` | WP71, WP74 |
| WP77 | config, `maintenance` package, `serve.go` wiring | WP74–WP76 |
| WP78 | proto, handlers, SDK, CLI | WP74, WP75 |
| WP79 | e2e, STORAGE.md and CONFIGURATION.md, benchmark | all |

**Opus writes** everything that deletes data, is concurrent, is a compare-and-swap
or is crash recovery: WP71–WP77 entirely, the handlers of WP78, and the race, crash
and e2e tests. **Haiku drafts** from an exact spec, monitored and checked by Opus:
the `zz_*_spec_test.go` spec tests of each WP (from this file and the signatures
only, in `/home/feder/m3-work/spec-tests7/<WP>/...`), the CLI and SDK plumbing of
WP78, table-driven scaffolding for the pure `sqlstore` methods, and first drafts of
the STORAGE.md and CONFIGURATION.md prose (Opus fixes and fills the measured
numbers). Work files live in `/home/feder/m3-work/`, never in `/tmp` (PM.md).

## Risks

* **Mixed-version clusters.** Old running nodes fail their pack-carrying writes as
  soon as migration 2 is applied (intended, the trigger); operators must upgrade every
  node together. Documented in STORAGE.md.
* **Lost dedup.** Byte-identical packs are stored once per write; measured.
* **Writer deadline.** A write slower than `gc_grace − gc_margin` (55 min by default)
  fails; an import of a very large repository must fit in it after its pack build
  (the deadline starts after the build).
* **Unreachable-object pruning** loses the diff of closed reviews whose commits are
  unreachable at a checkpoint (documented limit).
* **Missing open-review objects** block checkpoints of the repository until the
  review is closed; the root fix is in the review service (open question).
* **Re-hydration** discards a far-behind cache's objects (rare).
* **Lease held by a crashed node** delays maintenance by at most `lease_ttl`.
* **S3/GCS/Azure untested**; the design reads no bucket times, so the clock risk of
  the first version is gone, but `List` and conditional `Put` are exercised on file
  and mem buckets only.

## Resolved questions (PM review of 85189d4)

1. `graft storage gc -repo R` touches only that repository; removed repositories are
   swept by `graft storage gc --removed` and by the background task.
2. The checkpoint closure includes the head and base commits of open reviews.
3. `maintenance` defaults to `off` in this release.
4. The benchmark window is requested at WP79.

5. (Second review) Change A approved; the B/C refinement confirmed; the review
   service check is m3's (REVIEW ROOTS RULE); old running nodes' pack-carrying
   writes are counted as shadow errors in shadow mode, write failures in
   authoritative mode.

## Open questions for the PM

None open.

## Foundation touchpoints and ownership

New (m3): `corestore/checkpoint/**`, `corestore/gc/**`, `corestore/maintenance/**`,
`service/storagecore/{checkpoint,gc}.go`, `sdk/storage_gc.go`,
`internal/graft/e2e/storage_gc_test.go`. Changed (m3): `sqlstore/**`,
`corestore/{core,types,metastore}.go`, `blobstore/**`, `gitops/gitops.go`,
`voter/**`, `syncer/syncer.go`, `open/open.go`, `service/storagecore/{import,verify_objects}.go`,
`cmd/graft/storage.go`, `proto/storage_core.proto` (+ generated code),
`config_graft_storage.go`, `doc/graft/STORAGE.md`, `corestore/dbreview/**`, and
`internal/gitaly/service/review/**` (the REVIEW ROOTS R1 check only, assigned to
m3 by the PM). Shared, my section only:
`internal/cli/gitaly/serve.go`, `doc/graft/CONFIGURATION.md`. No change to the land
path: the voter is m3's; the land paths call it unchanged.

## RULES (binding, in addition to PLAN6 RULES 1–15)

16. **Packs only.** Compaction deletes packs, never changeset, changelog or review rows.
17. **Ownership.** Every upload uses a fresh owned key; `Put` never succeeds on an
    existing key; no code produces a legacy key.
18. **Enforced deadline.** A writer uploads and commits only before `t_r + W`.
19. **Observed age on the database clock.** An unreferenced pack is deleted only after
    the collector observed it unreferenced at least `gc_grace` earlier, decided in one
    snapshot; bucket times are never read.
20. **Lagging boundaries.** The collector uses `C_gc`; a sync decides with `C_sync`.
21. **Checkpoints are authoritative-mode only**; GC of orphans and removed repositories
    also runs in shadow mode.
22. **Untrusted data** (RULE 10 extended): checkpoint packs, snapshots and review
    commit IDs read from the database or the bucket are validated before they touch a
    local repository.
23. **Crash-safe order**: upload before record; row before pack deletion; prefix before
    tombstone deletion; `Delete` of a missing key is success.
24. **No fallback paths**: a pack the invariants say exists and does not is reported as
    corruption, never routed around.
25. **Review roots**: in authoritative mode a review's head and base must be reachable
    from the database references of the snapshot its write carries (R1), and a
    checkpoint includes every open review's roots (R2), both under the repository
    row lock.
26. **One upload path**: every production upload goes through `Core.UploadPack`, and
    every row that references an upload is committed under its `CommitContext`.
