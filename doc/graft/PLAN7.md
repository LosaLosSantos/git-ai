# Graft phase 7: pack garbage collection and checkpoint compaction (binding plan)

Status: binding contract for WP71–WP79 (storage core milestone 3). It extends
the phase-6 foundation (`doc/graft/PLAN6.md`, which stays binding: the CAS
semantics, the write path, the read path and catch-up, the REFTABLE, IMPORT,
STALE CACHE and LOCK AND HISTORY rules, and RULES 1–15 all still apply). This
plan adds tables, interface methods and packages; it never edits an applied
migration and never changes a phase-6 exported signature except where a rule
below says so.

Owner workstream: **m3** (`internal/graft/corestore/**`,
`internal/gitaly/service/storagecore/**`, the storage sub-commands of
`cmd/graft`, `proto/storage_core.proto`, the `GraftStorage` part of the config,
`doc/graft/STORAGE.md`, and the storage core block of `internal/cli/gitaly/serve.go`).

## Problem

`doc/graft/STORAGE.md`, "Limits", states two gaps left after the first
release:

1. **Packs are never deleted.** Every reference transaction uploads a
   content-addressed pack before it commits (`doc/graft/PLAN6.md`, write path).
   Nothing ever removes one. Three kinds accumulate forever in
   `repos/<repo_id>/packs/`:
   * **orphans** of writes that uploaded a pack and then did not commit the
     changeset (a transaction aborted after the Preparing phase, a node that
     died before the database commit — `graft_storage_local_commit_failures_total`
     and every aborted Preparing pack);
   * **superseded** changeset packs, whose objects a later consolidated state
     already contains;
   * the packs of **removed repositories** (`RemoveRepository` deletes the
     database rows through `ON DELETE CASCADE`, but the bucket keeps every pack
     under the removed repository's prefix).
2. **No checkpoint compaction.** Hydration (`syncer.hydrate`) and the dry
   hydration of `graft storage verify --connectivity` replay **every** changeset
   from sequence 1: they download and index every pack the repository ever had.
   Catch-up of a node that fell behind does the same from its watermark. The
   cost of a fresh node and of a connectivity check grows without bound with the
   history of the repository.

The database is authoritative and `repo_id` is never reused, so both can be
solved without ever risking the correctness of a read or a write: a consolidated
**checkpoint** gives hydration a bounded starting point, and a crash-safe,
idempotent **mark and sweep** reclaims the packs that no reader needs, with the
database as the single source of truth over what is live.

## Goal and non-goals

Deliver, for the storage core in authoritative mode (and, for the parts that
make sense without a source of truth, in shadow mode — see the SHADOW SCOPE
rule):

* **Checkpoints.** Per repository, a checkpoint at sequence `N` = one
  consolidated, content-addressed pack holding every object reachable from the
  references as of `N`, plus the reference snapshot at `N`, recorded in the
  database **atomically after the pack is uploaded**. Hydration and catch-up
  start from the newest checkpoint `<= head` and replay only changesets `> N`.
  Created by a background task on configurable thresholds and on demand
  (`graft storage checkpoint` + RPC). Writes are never blocked while a
  checkpoint is built.
* **Pack GC.** Mark and sweep, idempotent, crash-safe after every step, the
  database authoritative. Reclaims (a) orphan packs, (b) changeset packs covered
  by a kept checkpoint, (c) packs of removed repositories, and the superseded
  checkpoint packs themselves. `graft storage gc` (+ `--dry-run`) and the
  background task.

Out of scope (stated as limits or future work in STORAGE.md): per-directory
permissions; S3/GCS/Azure testing (no server here — file/mem buckets only);
renaming anything; **local-repository housekeeping** (no `git gc`/repack of a
node's cache — the cache is rebuildable from a checkpoint, so an operator
reclaims local disk by removing and re-hydrating a cache; we do not add local
maintenance here); changelog-row / changeset-metadata compaction (we delete
packs, never changeset or changelog rows, so `graft log` and the changelog
cursors are untouched).

## Design

### Overview

The expensive, immutable state is the **packs** in the bucket. The cheap state
is the **changeset metadata** (`graft_changesets`, `graft_changeset_refs`) and
the **changelog** (`graft_changelog`). Phase 7 reclaims packs and never touches
the metadata or the changelog: every changeset row stays, so the WAL, the
changelog and their consumers' cursors are unaffected. A checkpoint supersedes
the *packs* of the changesets up to its sequence, not their rows.

```
seq:   1    2    3   ...        N (checkpoint)   N+1  ...  head
packs: p1   p2   p3  ...        [reclaimable once checkpoint N exists]
                                 C_N (consolidated)  p_{N+1} ... p_head
rows:  all changeset rows kept (WAL + changelog), forever
```

A **checkpoint at N** is: a consolidated pack `C_N` (objects reachable from the
references at N) uploaded to the bucket, plus the reference snapshot at N, plus
a row in `graft_checkpoints`, committed in one transaction after `C_N` is
uploaded. Because `C_N` holds the full reachable closure at N, the individual
packs `p1..pN` are then redundant for reconstructing any state `>= N`.

* **Hydration** uses the newest checkpoint `<= head`: it indexes `C_N`, sets the
  references to the snapshot at N, then replays changesets `(N, head]` exactly
  as the phase-6 hydration replays `(0, head]`.
* **Catch-up** of a node whose watermark is `< N` cannot download the reclaimed
  `p` packs, so it **re-hydrates** from the checkpoint. A node at `>= N` catches
  up from its watermark as before (those packs are never reclaimed).
* **GC** keeps every pack that a hydration or catch-up still needs: the kept
  checkpoint packs and every changeset pack with sequence `> C` (the newest kept
  checkpoint's sequence). It deletes the rest, orphans only after a grace
  period.

### What happens to unreachable objects (binding decision)

The checkpoint pack is built with `git pack-objects` over the object-ID targets
of the references at N, non-thin: it holds exactly the objects **reachable from
the references at N**. Objects that were reachable at some earlier sequence but
not at N — commits orphaned by a force-update, a deleted branch or tag, a
rebased-away lineage — are **not** carried into the checkpoint pack, and once the
changeset packs that introduced them are reclaimed they are gone from object
storage.

Justification. Hydration and catch-up only ever reconstruct the state at or
after a checkpoint; no node ever needs a pre-checkpoint, now-unreachable object
to reach `head`. `graft storage verify --connectivity` checks reachability from
the **current** references, which the checkpoint pack plus the packs after it
satisfy. The changeset log keeps its metadata (operation, actor, old/new object
IDs), so the history questions the log answers keep working; only the git
*objects* of superseded history below a checkpoint are pruned — exactly as
`git gc` prunes unreachable objects in an ordinary repository. Stated as a limit
in STORAGE.md. (Open question for the PM: whether the checkpoint pack should also
retain objects reachable from the tips of **open reviews**; see "Open questions".)

### Data model (new migration, both dialects)

A new migration `20261018000000_graft_storage_gc`, appended to
`sqliteMigrations` and `postgresMigrations` in
`internal/graft/corestore/sqlstore/migrations.go` (the phase-6 migration is
never edited). A v0.1.0 database (migration 1 only) gets these tables created at
`Open`; with zero checkpoints and zero tombstones every phase-7 path degrades to
the phase-6 behaviour (hydration replays from 1; GC still reclaims orphans).

```sql
-- A checkpoint: a consolidated pack holding every object reachable from the
-- references at `seq`, plus (in graft_checkpoint_refs) the reference snapshot.
CREATE TABLE graft_checkpoints (
    repo_id    <int>   NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
    seq        <int>   NOT NULL,            -- the changeset sequence the checkpoint is at (1..head_seq)
    pack_key   TEXT    NOT NULL,            -- repos/<repo_id>/packs/<sha256>.pack of the consolidated pack
    pack_bytes <int>   NOT NULL DEFAULT 0,
    created_at <ts>    NOT NULL,
    PRIMARY KEY (repo_id, seq)
);

-- The reference snapshot at a checkpoint: name -> target. Never holds an absent
-- value (only the references that exist at `seq`). `target` is a hex object ID
-- or "ref:<name>" for a symbolic reference, exactly like graft_refs.
CREATE TABLE graft_checkpoint_refs (
    repo_id <int>  NOT NULL,
    seq     <int>  NOT NULL,
    ordinal <int>  NOT NULL,
    name    BLOB   NOT NULL,                -- BYTEA on Postgres; may not be valid UTF-8
    target  TEXT   NOT NULL,
    PRIMARY KEY (repo_id, seq, ordinal),
    FOREIGN KEY (repo_id, seq) REFERENCES graft_checkpoints (repo_id, seq) ON DELETE CASCADE
);

-- One maintenance lease per repository: the node building a checkpoint or
-- running GC for it. Acquired by compare-and-swap (expires_at in the past is
-- free). Observability + exclusivity only; never a correctness gate for reads
-- or writes.
CREATE TABLE graft_gc_leases (
    repo_id    <int>  NOT NULL PRIMARY KEY REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
    holder     TEXT   NOT NULL,             -- node_id
    acquired_at <ts>  NOT NULL,
    expires_at  <ts>  NOT NULL
);

-- A tombstone for a removed repository, written in the same transaction as the
-- repository row's deletion. No foreign key: the repository row is gone. GC
-- deletes the bucket prefix repos/<repo_id>/ and then the tombstone.
CREATE TABLE graft_gc_removed (
    repo_id       <int> NOT NULL PRIMARY KEY,
    storage       TEXT  NOT NULL,           -- for logs/observability only
    relative_path TEXT  NOT NULL,           -- "
    removed_at    <ts>  NOT NULL
);
```

`<int>` = `INTEGER` on SQLite / `BIGINT` on Postgres; `<ts>` = RFC 3339 text in
UTC on SQLite / `TIMESTAMPTZ` on Postgres; names are `BLOB`/`BYTEA`, as the
phase-6 tables. `repo_id` of a checkpoint and of a tombstone is never reused, so
a removed-and-recreated repository never shares a checkpoint or a tombstone with
its predecessor.

Go types (new, in `internal/graft/corestore/types.go`):

```go
// Checkpoint is a consolidated state of a repository at a changeset sequence:
// one pack with every object reachable from the references at Seq.
type Checkpoint struct {
    RepoID    int64
    Seq       uint64
    PackKey   string
    PackBytes int64
    CreatedAt time.Time
}

// RemovedRepository is a tombstone of a removed repository whose bucket packs
// are still to be reclaimed.
type RemovedRepository struct {
    RepoID       int64
    Key          RepoKey
    RemovedAt    time.Time
}
```

### BlobStore additions (RULE: GC needs listing and deletion)

`corestore.BlobStore` (in `core.go`) gains two methods; `blobstore` implements
them on the gocloud bucket, `coretest`/test doubles follow:

```go
// BlobInfo describes a stored object for garbage collection.
type BlobInfo struct {
    Key     string
    Size    int64
    ModTime time.Time
}

type BlobStore interface {
    // ... existing Put, Get, Exists, Close ...

    // List calls fn for every object whose key starts with prefix, in no
    // defined order. It lists objects only, never directory markers. fn may
    // return an error to stop the iteration (returned as-is).
    List(ctx context.Context, prefix string, fn func(BlobInfo) error) error
    // Delete removes the object. A key that does not exist is success
    // (idempotent), so a re-run of GC never fails on an already-deleted pack.
    Delete(ctx context.Context, key string) error
}
```

Implementation (`blobstore`): `List` uses `bucket.List(&blob.ListOptions{Prefix:
prefix})` and the iterator's `*blob.ListObject` (`Key`, `Size`, `ModTime`,
`IsDir`), skipping `IsDir` entries (we list without a delimiter, so there are
none, but we guard). `Delete` uses `bucket.Delete`, mapping `gcerrors.NotFound`
to success. `ModTime` is the object's last-modified time; for the fileblob
driver it is the file mtime, for mem it is the write time (both used in tests);
for S3/GCS/Azure it is the provider's object mtime (untested here, see limits).
File buckets may leave empty directories behind after the last pack under them
is deleted; this is harmless and documented (the keys, which are what matters,
are gone).

### MetaStore additions

`corestore.MetaStore` (in `metastore.go`) gains the methods below; `sqlstore`
implements them on both dialects, with the phase-6 patterns (`bind` for
placeholders, `withTx`/`withReadTx` with the retry on busy/serialization,
`timeArg`/`scanTime`, `bytesArg`/`targetArg`, name chunks of
`maxNamesPerQuery`). `DeleteRepository` changes behaviour (writes a tombstone).

```go
// CheckpointInput is the state recorded by CreateCheckpoint.
type CheckpointInput struct {
    Seq       uint64             // 1..head_seq, strictly greater than the newest checkpoint's Seq
    Refs      map[string]Target  // the reference snapshot at Seq; no absent values; no empty map
    PackKey   string             // the already-uploaded consolidated pack (never empty)
    PackBytes int64
}

type MetaStore interface {
    // ... existing methods ...

    // CreateCheckpoint records a checkpoint and its reference snapshot in one
    // transaction. It locks the repository row, verifies Seq <= head_seq and
    // Seq > the newest existing checkpoint's Seq (else an error, nothing
    // written), inserts graft_checkpoints and graft_checkpoint_refs, and
    // returns the stored Checkpoint. A checkpoint at Seq that already exists is
    // returned unchanged (idempotent; the lease makes a concurrent build at the
    // same Seq a non-case, but a resumed build after a crash is safe).
    // ErrRepositoryNotFound for an unknown repository.
    CreateCheckpoint(ctx context.Context, repoID int64, in CheckpointInput) (Checkpoint, error)
    // NewestCheckpoint returns the checkpoint of the repository with the
    // greatest Seq; ok is false when there is none.
    NewestCheckpoint(ctx context.Context, repoID int64) (cp Checkpoint, ok bool, err error)
    // ListCheckpoints returns every checkpoint of the repository, ascending by
    // Seq.
    ListCheckpoints(ctx context.Context, repoID int64) ([]Checkpoint, error)
    // CheckpointRefs returns the reference snapshot at the checkpoint (name ->
    // target). ErrCheckpointNotFound when there is no checkpoint at seq.
    CheckpointRefs(ctx context.Context, repoID int64, seq uint64) (map[string]Target, error)
    // DeleteCheckpoint removes the checkpoint row and its snapshot (cascade). It
    // returns false when there was none. The caller deletes the pack afterwards
    // (a crash in between leaves an orphan pack, reclaimed by the orphan sweep).
    DeleteCheckpoint(ctx context.Context, repoID int64, seq uint64) (bool, error)

    // ChangesetPacks calls fn for every changeset of the repository with a
    // non-empty pack key, ascending by sequence, reading only (seq, pack_key).
    // It is the lean scan GC marks live packs with (ListChangesets would carry
    // every reference change).
    ChangesetPacks(ctx context.Context, repoID int64, fn func(seq uint64, packKey string) error) error

    // AcquireMaintenanceLease takes the repository's maintenance lease for
    // holder until now+ttl, by compare-and-swap: it succeeds when there is no
    // lease or the current one has expired. RenewMaintenanceLease extends it if
    // holder still holds it; ReleaseMaintenanceLease frees it if holder holds
    // it. All three are no bar to reads or writes; they only serialize GC and
    // checkpoint builds across nodes.
    AcquireMaintenanceLease(ctx context.Context, repoID int64, holder string, ttl time.Duration) (acquired bool, err error)
    RenewMaintenanceLease(ctx context.Context, repoID int64, holder string, ttl time.Duration) (held bool, err error)
    ReleaseMaintenanceLease(ctx context.Context, repoID int64, holder string) error

    // ListRepositories returns every repository registered in the storage
    // (its head sequence > 0), for the background maintenance task. keyPrefix
    // empty lists all storages' repositories; otherwise only the given storage.
    ListRepositories(ctx context.Context, storage string) ([]Repository, error)

    // ListRemovedRepositories returns the tombstones of removed repositories
    // whose bucket packs are still to be reclaimed.
    ListRemovedRepositories(ctx context.Context) ([]RemovedRepository, error)
    // DeleteRemovedRepository removes a tombstone after its bucket prefix was
    // cleared. Idempotent.
    DeleteRemovedRepository(ctx context.Context, repoID int64) error
}
```

`ErrCheckpointNotFound` is a new sentinel in `types.go`.

**DeleteRepository (changed, binding).** It now writes a `graft_gc_removed`
tombstone `(repo_id, storage, relative_path, removed_at)` in the **same
transaction** that deletes the repository row, reading the `repo_id` before the
delete. The cascade removes the repository's changesets, references, changelog,
reviews, checkpoints and lease; the tombstone survives (no FK) so that GC can
reclaim the bucket prefix afterwards. A key the database does not know writes no
tombstone and returns false, as before. The method keeps its signature
`(bool, error)`; the gate's `removeRepository` is unchanged.

### gitops addition

One new function in `internal/graft/corestore/gitops/gitops.go`, with a test;
nothing else in gitops changes:

```go
// BuildPackFrom writes a self-contained (non-thin) pack of every object
// reachable from tips (the consolidated pack of a checkpoint). The tips are the
// object-ID targets of the references at the checkpoint sequence; symbolic and
// zero targets are ignored. It is BuildFullPack restricted to the given tips:
// `git rev-list --objects --stdin <tips>` (no --not) piped into
// `git pack-objects --stdout`, hashed into a file in tmpDir. An empty tip set
// returns an empty Pack.
func BuildPackFrom(ctx context.Context, repo *localrepo.Repo, tips []git.ObjectID, tmpDir string) (Pack, error)
```

### Checkpoint creation (new package `internal/graft/corestore/checkpoint`)

`checkpoint.Checkpointer` builds and records a checkpoint for one repository. It
is used by the background maintenance task and by the `CreateCheckpoint` RPC.

```go
func New(core *corestore.Core, logger log.Logger, locator storage.Locator,
    repoFactory localrepo.Factory, fresh corestore.Freshener) *Checkpointer

// Create builds a checkpoint of repo at the current database head and records
// it. It requires authoritative mode and a local copy that EnsureFresh can
// bring to the head (the objects of the checkpoint come from the local cache).
// Returns the created Checkpoint and whether one was created (false = a
// checkpoint already exists at the head, nothing to do).
func (c *Checkpointer) Create(ctx context.Context, repo storage.Repository) (corestore.Checkpoint, bool, error)
```

Binding order (crash-safe; `FaultHook`-style test points at each `//!` below):

1. Require `core.Authoritative()` (else `FailedPrecondition`). `EnsureFresh` the
   local copy. Read `(refs, N) = core.Meta.Refs(repoID)` — the reference
   snapshot and the head sequence in **one consistent database snapshot**. If a
   checkpoint at `N` already exists (`NewestCheckpoint`), return it, `created =
   false`.
2. The tips are the object-ID targets of `refs` (skip symbolic and absent).
   `pack = gitops.BuildPackFrom(ctx, local, tips, tmpDir)` from the **local**
   repository (fresh to `>= N`, so it holds every object reachable from `refs`;
   nothing prunes the local cache). An empty repository (no object tips) records
   a checkpoint with no pack is **not** allowed — a checkpoint must have a pack;
   a repository whose references are only symbolic/empty is a degenerate case
   handled by recording the snapshot with an empty pack key is disallowed, so we
   skip creating a checkpoint (return `created = false`) when there are no object
   tips. (Hydration of such a repository from 1 is already trivial.)
3. `//! before_upload` — upload `pack` under `corestore.PackKey(repoID,
   pack.SHA256)` (`BlobStore.Put`, create-only).
4. `//! after_upload` — `core.Meta.CreateCheckpoint(repoID, CheckpointInput{Seq:
   N, Refs: refs, PackKey, PackBytes})`. The snapshot `refs` is exactly the
   references read in step 1, so it is consistent with `N`.
5. `//! after_commit` — return the checkpoint, `created = true`.

A crash after step 3 and before step 4 leaves the consolidated pack as an orphan
(no checkpoint row references it), reclaimed by the orphan sweep. A re-run
rebuilds from the head at that time (possibly a larger `N`) and commits. The
build never holds a reference lock and never blocks a write: it reads a database
snapshot and the local objects, and records the checkpoint in its own
transaction, while writes keep appending changesets `> N` (CKPT-2).

### Pack GC (new package `internal/graft/corestore/gc`)

`gc.Collector` runs the mark and sweep for one repository and the removed-
repository cleanup. Used by the background task and by `CollectGarbage`.

```go
func New(core *corestore.Core, logger log.Logger) *Collector

// Report is what a collection did (or would do, for a dry run).
type Report struct {
    CheckpointSeq      uint64 // the newest kept checkpoint, 0 when none
    PacksDeleted       int    // covered + orphan + dropped-checkpoint packs
    BytesReclaimed     int64
    CoveredDeleted     int
    OrphansDeleted     int
    OrphansKeptInGrace int
    CheckpointsDropped int
    RemovedRepos       int    // removed-repository prefixes reclaimed
    DryRun             bool
}

// CollectRepository reclaims the dead packs of one repository: changeset packs
// covered by the newest kept checkpoint, dropped-checkpoint packs, and orphans
// older than the grace period. The caller holds the repository's maintenance
// lease. grace is config gc_grace; retain is config checkpoint_retain.
func (c *Collector) CollectRepository(ctx context.Context, repoID int64, grace time.Duration, retain int, dryRun bool) (Report, error)

// CollectRemoved reclaims the bucket prefix of every removed-repository
// tombstone and deletes the tombstone. No lease (the repository is gone,
// repo_id never reused, no writer); idempotent.
func (c *Collector) CollectRemoved(ctx context.Context, dryRun bool) (Report, error)
```

`CollectRepository` (binding; the database is authoritative at every step, and
no live pack is ever deleted):

1. `checkpoints = ListCheckpoints(repoID)` ascending. Choose the **kept** set:
   always the newest; also the most recent `retain` checkpoints; also any
   checkpoint younger than `grace` (so an in-flight hydration started before a
   newer checkpoint appeared is safe). `C` = the newest checkpoint's sequence (0
   when there is none). The checkpoints **to drop** are the rest (older than the
   newest, beyond `retain`, and older than `grace`).
2. `packKeys = {}`; `ChangesetPacks(repoID, func(seq, key){ packKeys[key] = seq })`.
   Build the live set:
   * every kept checkpoint's `PackKey`;
   * every `key` in `packKeys` with `seq > C`.
   The covered set = every `key` in `packKeys` with `seq <= C`.
3. Drop the superseded checkpoints: for each, `DeleteCheckpoint` (row) first,
   then its pack becomes an orphan for step 4 (we do **not** delete the pack
   before the row: a crash in between would leave a row pointing at a missing
   pack, which a hydration might pick — the row goes first, the pack is an orphan
   afterwards). In a dry run, nothing is deleted; the pack is counted as a
   dropped-checkpoint pack.
4. Sweep: `Blobs.List("repos/<repoID>/packs/", func(obj){ ... })`. For each
   `obj.Key`:
   * in the live set → keep;
   * in the covered set (`packKeys[key] <= C`) → delete (covered by a kept
     checkpoint);
   * otherwise it is an **orphan** (no changeset row and no kept checkpoint
     references it — a dead write's pack, or a just-dropped checkpoint's pack):
     delete only when `now - obj.ModTime >= grace`, else count it in
     `OrphansKeptInGrace` and keep it.
   `dryRun` deletes nothing and only counts.
5. Return the `Report`. Idempotent: the live/covered sets are recomputed from
   the database each run, and `Blobs.Delete` of an already-deleted key is a
   no-op, so a crashed sweep converges on the next run and a re-run is cheap.

`CollectRemoved`: `ListRemovedRepositories`; for each tombstone,
`Blobs.List("repos/<repo_id>/", delete each key)`, then
`DeleteRemovedRepository(repo_id)`. A crash after a partial prefix delete leaves
the tombstone; the next run finishes (idempotent). No lease.

### Hydration from a checkpoint (syncer, binding change)

`internal/graft/corestore/syncer/syncer.go`, `hydrate`, now seeds from the
newest usable checkpoint instead of always from sequence 1. The phase-6 WP55
RULE (catch-up never moves a reference backwards; the watermark only claims
sequences whose effects are local) and the install/stale-cache handling are
unchanged.

`hydrate(ctx, repo, r, hash, path)`:

1. Choose the seed: iterate `ListCheckpoints(r.ID)` from the greatest `Seq`
   downward and pick the first whose `Seq <= r.HeadSeq` and whose pack
   `Blobs.Exists`/can be fetched. Call it `cp` (seed sequence `S = cp.Seq`,
   snapshot `snap = CheckpointRefs(r.ID, S)`). If there is none, `S = 0`, no
   seed pack, no snapshot — the phase-6 replay from 1.
2. `InitBare` the build repository. When seeded, index `cp.PackKey` into it
   (downloaded and SHA-256-verified exactly like a changeset pack; the pack key
   is validated to belong to `r.ID`), and validate every name/target of `snap`.
3. `f = newFetched(S)`; `fetch(ctx, buildRepo, tmpDir, r.ID, hash, f,
   r.HeadSeq)` — downloads and indexes the changeset packs `(S, head]` and folds
   their reference changes, as today.
4. Compute the final reference set as of head: start from `snap` (the references
   at `S`), then apply `f`'s changes — a changed reference's final value
   overwrites, an absent final value deletes it. `applyBuild(combined)`.
5. `WriteWatermark(buildPath, {r.ID, f.head})`, then `install` as today.

The number of changesets replayed is `head - S`, bounded by the checkpoint
cadence, instead of `head`. If no checkpoint's pack can be fetched **and** the
replay from 1 needs a pack that GC reclaimed, hydration fails with an Internal
error naming the missing pack (a genuine inconsistency); the newest checkpoint's
pack is always present by GC-3, so this is the corruption case only.

### Catch-up below a checkpoint (syncer, binding change)

A node whose watermark `wm.Seq < C` (the newest checkpoint) cannot download the
reclaimed packs `(wm.Seq, C]`, so it must re-hydrate from the checkpoint rather
than catch up. In `sync()`, the condition that chooses hydration gains this
case:

* read `cp, hasCp = NewestCheckpoint(r.ID)` once per round (cheap);
* hydrate when `!ok || wm.RepoID != r.ID || !localCurrent || (hasCp && wm.Seq <
  cp.Seq)`; otherwise catch up (then `wm.Seq >= cp.Seq`, and every pack `(wm.Seq,
  head]` is live, so it exists).

Re-hydration rebuilds the local copy from the checkpoint and replaces it through
the existing `install` path (which moves the stale copy aside under the
repository lock). This is correct because the commit point is the database: every
committed write is in the database at a sequence `<= head`, so it is either in
the checkpoint (`<= S`) or replayed from `(S, head]`. It discards the local
objects of a far-behind cache (wasteful but rare — an actively-served node stays
near the head and never falls below a checkpoint). `checkReplaceable` (the
shadow-history guard, IMPORT RULE) still runs first and is unaffected
(authoritative mode writes no file changelog or file reviews).

Belt-and-suspenders in `fetch`: if a changeset pack with `seq <= C` is
`ErrBlobNotFound` during a catch-up (a lagging round races a concurrent GC that
reclaimed a covered pack), return `errRestart` so `sync` re-evaluates and
hydrates from the checkpoint. A missing pack with `seq > C` stays a hard
Internal error (real corruption). `fetch` is given the newest checkpoint
sequence for this test.

### Verify after compaction (service/storagecore/verify_objects.go, binding change)

`checkStoredObjects` (the dry hydration of `graft storage verify
--connectivity`) replays from the newest checkpoint instead of from 1, so it
keeps passing after GC reclaimed the covered packs:

* `cp, ok = NewestCheckpoint(record.ID)`. When `ok`, index `cp.PackKey` into the
  empty verification repository, then index the changeset packs for `seq in (C,
  head]`. When not, index all changeset packs `1..head` (as today).
* Then `git rev-list --objects --quiet` from the object-ID targets of the
  database references, as today.

A checkpoint pack that is missing or corrupt, or a missing changeset pack `>
C`, is reported as a connectivity failure beginning `storage core objects:`
exactly as a missing changeset pack is today.

### Background maintenance task (new, `internal/graft/corestore/maintenance`)

`maintenance.Runner` runs the checkpoints and GC on a timer. Wired in
`internal/cli/gitaly/serve.go` (my section only), started with the server and
stopped on shutdown (as `bundleGenerationStrategy.Start(ctx)` / `stop()`), only
when the storage core is authoritative and `maintenance = "auto"`.

```go
func New(core *corestore.Core, cp *checkpoint.Checkpointer, collector *gc.Collector,
    fresh corestore.Freshener, locator storage.Locator, logger log.Logger, cfg Config) *Runner
func (r *Runner) Start(ctx context.Context) (stop func())
```

Each tick (every `maintenance_interval`):

1. `CollectRemoved` (tombstones; any node, no lease).
2. For each repository `ListRepositories` reports in this node's storages that
   this node has a **local copy with a current watermark** (so it can build a
   checkpoint from local objects):
   * `AcquireMaintenanceLease(repoID, node_id, lease_ttl)`; skip the repository
     when another node holds it.
   * `EnsureFresh`.
   * If `head_seq - newestCheckpointSeq >= checkpoint_changesets` **or** the pack
     bytes of the changesets since the newest checkpoint `>= checkpoint_pack_bytes`
     (summed from `ChangesetPacks` / `ListChangesets` pack bytes), `Checkpointer.Create`.
   * `Collector.CollectRepository(repoID, gc_grace, checkpoint_retain, false)`.
   * `RenewMaintenanceLease` around a long build; `ReleaseMaintenanceLease` at
     the end (a crash lets the lease expire after `lease_ttl`).

A node without a local copy of a repository never builds its checkpoint (it has
no objects), but any node may run GC of it (GC needs only the database and the
bucket). In practice the node that serves a repository maintains it.

### Configuration (`internal/gitaly/config/config_graft_storage.go`, my section)

New keys on `[graft.storage]` (`GraftStorage` struct), all optional:

```toml
[graft.storage]
# ... existing mode, database, bucket, node_id, sync_timeout, shadow_timeout ...
maintenance            = "auto"    # off | auto — background checkpoints + GC (authoritative only). Default auto.
maintenance_interval   = "1h"      # how often the background task scans repositories (Go duration)
checkpoint_changesets  = 1000      # create a checkpoint after this many changesets since the last one (0 = disabled)
checkpoint_pack_bytes  = 268435456 # ...or after this many bytes of changeset packs since the last one (0 = disabled)
checkpoint_retain      = 2         # number of most-recent checkpoints to keep (>= 1)
gc_grace               = "1h"      # minimum age before an orphan or a dropped-checkpoint pack is deleted (Go duration)
lease_ttl              = "10m"     # maintenance lease duration, renewed during a build
```

Defaults (new `Default*` constants): `maintenance` = `"auto"`,
`maintenance_interval` = 1h, `checkpoint_changesets` = 1000,
`checkpoint_pack_bytes` = 256 MiB, `checkpoint_retain` = 2, `gc_grace` = 1h,
`lease_ttl` = 10m. Validation (extend `GraftStorage.validate`): `maintenance in
{"", "off", "auto"}`; durations positive; `checkpoint_retain >= 1`;
thresholds `>= 0`; it is not an error to set them in shadow/off mode (they are
ignored, documented). `gc_grace` must comfortably exceed the Preparing→commit
window (bounded by `shadow_timeout` / the 10 s reftable lock wait +
`sync_timeout`); the default 1h does. Accessor helpers mirror
`SyncTimeoutOrDefault`.

### RPCs (`proto/storage_core.proto`)

`StorageCoreService` is in the gate's `Passthrough`, so these RPCs are **not**
intercepted (no `AuthoritativeMutators` entry needed); they handle freshness
themselves.

```proto
// CreateCheckpoint builds a consolidated checkpoint of the repository at the
// current head and records it, so that hydration and catch-up start from it.
// Requires authoritative mode.
rpc CreateCheckpoint(CreateCheckpointRequest) returns (CreateCheckpointResponse) {
  option (op_type) = { op: MUTATOR };
}
// CollectGarbage reclaims the storage core packs the repository no longer needs
// (orphans past the grace period, and packs covered by a kept checkpoint) and,
// as a cheap global step, the packs of removed repositories. dry_run reports
// what it would delete without deleting anything.
rpc CollectGarbage(CollectGarbageRequest) returns (CollectGarbageResponse) {
  option (op_type) = { op: MUTATOR };
}

message CreateCheckpointRequest  { Repository repository = 1 [(target_repository)=true]; }
message CreateCheckpointResponse {
  uint64 checkpoint_sequence = 1; // the sequence the checkpoint is at
  bool   created             = 2; // false when a checkpoint already existed at the head
  int64  pack_bytes          = 3;
  uint32 references          = 4; // references in the snapshot
}

message CollectGarbageRequest {
  Repository repository = 1 [(target_repository)=true];
  bool       dry_run    = 2;
}
message CollectGarbageResponse {
  uint64 checkpoint_sequence   = 1; // newest kept checkpoint, 0 when none
  uint32 packs_deleted         = 2;
  int64  bytes_reclaimed       = 3;
  uint32 covered_deleted       = 4;
  uint32 orphans_deleted       = 5;
  uint32 orphans_kept_in_grace = 6;
  uint32 checkpoints_dropped   = 7;
  uint32 removed_repositories  = 8;
  bool   dry_run               = 9;
}
```

Handlers (`internal/gitaly/service/storagecore/checkpoint.go`, `gc.go`):
`CreateCheckpoint` → off/shadow → `FailedPrecondition`; MVCC/non-reftable →
`FailedPrecondition`; otherwise `Checkpointer.Create`. `CollectGarbage` → off →
`FailedPrecondition`; resolves the repository, `AcquireMaintenanceLease`
(contending → `Aborted`/`FailedPrecondition` "another maintenance operation is
in progress"), `CollectRepository` + `CollectRemoved`, release the lease. In
shadow mode `CollectGarbage` runs the orphan + removed-repo sweeps only (no
checkpoints exist, so there is no covered set) — see SHADOW SCOPE.

### SDK and CLI

SDK (`sdk/storage_gc.go`, mirroring `sdk/storage.go`): `func (r *Repo)
StorageCheckpoint(ctx) (*StorageCheckpoint, error)` and `func (r *Repo)
StorageGC(ctx, dryRun bool) (*StorageGC, error)` with plain result structs.

CLI (`cmd/graft/storage.go`, extending the dispatcher and the usage strings):

```
graft storage checkpoint [-repo R] [--json]
graft storage gc         [-repo R] [--dry-run] [--json]
```

Human output: `checkpoint` → `checkpoint at changeset N: K references, pack B
bytes` (or `already checkpointed at changeset N` when `created=false`); `gc` →
one line per category and a total, with `(dry run)` when `--dry-run`.

## Rejected alternatives

* **Store the reference snapshot by folding changeset metadata at hydration
  time** instead of a `graft_checkpoint_refs` table. Rejected: folding is
  `O(N)` metadata rows per hydration, and a self-contained snapshot makes the
  checkpoint directly verifiable. The snapshot is `O(refs)` rows written once.
* **Delete changeset and changelog rows below a checkpoint** (full WAL
  compaction). Rejected: the changelog is the ordered log clients follow and its
  cursors must stay valid (`ListChangesets`/`WatchChangesets`); the rows are
  cheap; and keeping them lets catch-up fold reference history above a checkpoint
  without a special case. We compact packs, not rows.
* **A pending-record table for in-flight uploads** instead of a grace period on
  orphans. Rejected for v1: it adds a database write to every write path (RULE
  4: shadow must not slow a write), while the Preparing→commit window is tightly
  bounded and a generous `gc_grace` covers it at zero write-path cost. Kept as a
  fallback if a bucket's mtime proves unreliable (noted as a risk).
* **Additive catch-up from a checkpoint** (index the checkpoint pack into the
  existing cache of a node below the checkpoint, then replay) instead of a full
  re-hydration. Rejected for v1: it entangles the checkpoint seed with the WP55
  "never revert a local write" compare-and-swap logic; re-hydration is provably
  correct and the case is rare. Noted as a possible later optimization.
* **Carry unreachable objects into the checkpoint pack** (pack `--all` of the
  building node's local cache, not just the refs-at-N closure). Rejected: it
  would defeat compaction (the cache accumulates every object ever seen) and no
  reader needs those objects to reach head. See "unreachable objects" above.
* **No lease; rely on GC idempotency for concurrency.** Rejected: idempotent GC
  is safe to race, but a GC on one node must not run while a checkpoint build on
  another uploads a not-yet-committed checkpoint pack (it would look like an
  orphan). One maintenance lease per repository makes checkpoint build and GC
  mutually exclusive and is the invariant the assignment requires. (Orphan grace
  still protects the checkpoint pack as defence in depth.)
* **Full-bucket scan to find removed repositories** (list `repos/<id>/` prefixes
  and match against `graft_repositories`). Rejected: `O(all objects)` and racy
  against concurrent creation. A tombstone written in the delete transaction is
  crash-safe and bounded.

## Invariants (binding; each has a test)

* **I1 (preserved).** Every object reachable from the current references of a
  repository is in some pack the storage core keeps: the newest checkpoint's
  pack holds the closure at its sequence, and every changeset pack after it is
  kept, so the closure at head is covered.
* **GC-1.** GC never deletes a pack referenced by a changeset with sequence `>`
  the newest kept checkpoint (they are in the live set).
* **GC-2.** GC never deletes a pack a write in progress may still commit: an
  orphan is deleted only when older than `gc_grace`, which exceeds the
  Preparing→commit window.
* **GC-3.** GC never deletes a pack a node hydrating from a kept checkpoint
  reads: the newest checkpoint is always kept, and an older one is dropped only
  when a newer exists and it is older than `gc_grace` (> `sync_timeout`, the
  hydration budget).
* **GC-4.** At most one checkpoint build or GC per repository across nodes at a
  time (the `graft_gc_leases` compare-and-swap).
* **GC-5.** A node whose watermark is below the newest checkpoint still reaches
  head: it re-hydrates from the checkpoint; a covered pack missing mid-catch-up
  restarts the sync into hydration.
* **CKPT-1.** A checkpoint at `N` holds every object reachable from the
  references at `N`; hydrating from it and replaying `(N, head]` yields exactly
  the state at head (references and connectivity).
* **CKPT-2.** Building a checkpoint never blocks a write and never fails one: it
  reads a database snapshot and local objects outside any reference lock and
  records the checkpoint in its own transaction.
* **COMPAT-1.** A v0.1.0 database (migration 1 only) works unchanged after
  migration 2: with no checkpoints hydration replays from 1, and GC still
  reclaims orphans and removed-repository packs.
* **CRASH/IDEMPOTENT.** Every GC and checkpoint step is crash-safe and a re-run
  converges; the database is authoritative over what is live; a `Delete` of a
  missing key is a no-op.
* **HISTORY (preserved).** No changeset, changelog or review row is deleted by
  compaction; `graft log`, `WatchChangesets`, reviews and the connectivity
  check keep working after GC and compaction.

## Crash and race cases (each tested with an injected fault)

1. Checkpoint crash after the pack upload, before the row commit → orphan
   checkpoint pack; reclaimed by the orphan sweep; a re-run rebuilds and commits.
2. Checkpoint crash after the row commit → the checkpoint exists; nothing to do.
3. GC crash mid-sweep → some dead packs remain; the next run reclaims them; no
   live pack was deleted.
4. GC crash after `DeleteCheckpoint` (row), before the pack delete → the pack is
   an orphan; reclaimed by the orphan sweep.
5. Removed-repository crash after a partial prefix delete, before the tombstone
   delete → the tombstone remains; the next run finishes (idempotent).
6. A write uploads its pack (Preparing) while GC sweeps → the pack is younger
   than `gc_grace`, so it is kept; after the commit it is a live changeset pack.
7. A checkpoint build on node A while GC runs on node B → prevented by the single
   maintenance lease; the orphan grace also protects the in-flight checkpoint
   pack.
8. A node catching up from `wm < C` while GC reclaims `(wm, C]` → the sync detects
   `wm < C` and hydrates from the checkpoint; a covered pack missing mid-fetch
   restarts the sync.
9. A node hydrating from the newest checkpoint while GC runs → GC keeps the
   newest checkpoint pack and every pack `> C`, so the hydration is unaffected.
10. `RemoveRepository` with a concurrent hydration leaving a stale local copy →
    the STALE CACHE RULE moves the local copy aside; the tombstone drives the
    bucket cleanup; `repo_id` is never reused, so a recreated repository is
    independent.
11. Two nodes race `AcquireMaintenanceLease` → exactly one wins; the other skips.
12. A checkpoint build node crashes holding the lease → the lease expires after
    `lease_ttl`; GC and future builds resume.

## Test plan (multilevel; SQLite and Postgres everywhere a store is involved)

* **sqlstore unit** (`sqlstore_test`, both dialects via `coretest.Dialects()`):
  `CreateCheckpoint` (monotonic sequence, `Seq <= head_seq`, idempotent at the
  same sequence, snapshot round-trip including non-UTF-8 names and symbolic
  targets); `NewestCheckpoint`/`ListCheckpoints`/`CheckpointRefs`/`DeleteCheckpoint`
  (cascade of the snapshot); `ChangesetPacks` (only non-empty keys, ascending);
  lease acquire/renew/release with a CAS race between two `Store`s and expiry;
  `DeleteRepository` writes a tombstone and cascades, re-registration gets a new
  `repo_id` and a fresh tombstone space; `ListRemovedRepositories`/
  `DeleteRemovedRepository`; `ListRepositories`.
* **blobstore unit** (`blobstore_test`, file + mem): `List` under a prefix with
  correct `ModTime`/`Size`, directory markers skipped; `Delete` idempotent on a
  missing key; `List`/`Delete` leave other keys untouched; S3 variant behind
  `GRAFT_TEST_S3_URL` only.
* **gitops unit** (`gitops_test`, SHA-1 and SHA-256): `BuildPackFrom` yields a
  self-contained pack whose index into an empty repository makes the tips'
  history connectivity-complete (`git fsck`); an empty tip set is an empty pack.
* **checkpoint unit** (`checkpoint_test`, `coretest` + `gittest`): build a
  checkpoint from a source repository, hydrate a fresh repository from the
  checkpoint and replay, assert references and `git fsck` connectivity equal the
  source at head; an object unreachable before the checkpoint is absent after
  compaction (the documented behaviour); `created=false` when a checkpoint
  already exists at head.
* **gc unit** (`gc_test`, `coretest` + `gittest`, both dialects): an orphan is
  kept within grace and reclaimed after it; covered packs (`<= C`) are deleted,
  packs `> C` and the kept checkpoint pack survive; a dropped old checkpoint's
  pack is reclaimed; a removed-repository prefix is cleared and its tombstone
  removed; `--dry-run` deletes nothing and reports the same counts; a re-run is
  a no-op.
* **-race** (`syncer_test`/`gc_test`/e2e): GC concurrent with continuous lands
  (every land keeps committing, no live pack is deleted, no CAS or connectivity
  break); GC concurrent with a hydration (the hydration completes); a checkpoint
  build concurrent with lands (writes are not blocked and the checkpoint is
  correct); re-hydration of a node forced below the checkpoint.
* **crash injection** (`checkpoint.FaultHook`, `gc.FaultHook`, and the real-binary
  `CrashEnv` for e2e): each fault point of §"Crash and race cases" 1–5 and 12,
  each followed by a re-run that converges and a `VerifyRepository
  --connectivity` that passes.
* **e2e** (`internal/graft/e2e/storage_gc_test.go`, two and three real gitaly
  processes sharing one database and one bucket, SQLite and Postgres): node A
  writes many changesets; `graft storage checkpoint`; `graft storage gc`; a fresh
  node B hydrates from the checkpoint (asserted: it downloads the checkpoint pack
  and only the changeset packs after it, via `graft_storage_sync_*` and the
  bucket), serves, and `VerifyRepository --connectivity` is consistent; a node
  that fell below the checkpoint re-hydrates; a repository removed on one node
  has its packs reclaimed; the changelog and reviews are intact after GC.
* **migration**: open a database created with migration 1 only (fixture), assert
  migration 2 applies, and that hydration, catch-up, checkpoint, GC and verify
  all work with zero checkpoints and zero tombstones.
* **measurement** (ask the PM for a benchmark window — the machine is shared):
  bucket size and fresh-node hydration time before and after a checkpoint + GC,
  on a repository with many changesets. Numbers go into STORAGE.md only from a
  real run.

## Acceptance criteria

* After `checkpoint_changesets` writes, a checkpoint plus GC leaves the bucket
  holding the newest checkpoint pack and the packs after it, not the whole
  history (measured bucket size drops; orphans and removed-repository packs are
  gone).
* A fresh node's hydration time is a function of `head - newestCheckpointSeq`,
  not of `head` (measured; constant as history grows past a checkpoint).
* `graft storage verify --connectivity` reports `consistent` after GC and
  compaction; `graft log`, `WatchChangesets` and reviews are unchanged.
* A v0.1.0 database migrates and every storage path works.
* All phase-6 storage tests stay green; the new unit, `-race`, crash and e2e
  tests pass on SQLite and Postgres.
* No live pack is ever deleted (GC-1..GC-5), proven by the `-race` and crash
  tests.

## SHADOW SCOPE rule (binding)

Checkpoints and background maintenance are **authoritative-mode only**: a
checkpoint's purpose is to seed hydration, which does not happen in shadow or
off mode, and its reference snapshot is the database's view, which is not
authoritative in shadow mode. `CreateCheckpoint` and the background `Runner`
therefore refuse / do nothing outside authoritative mode. **GC** of orphans and
of removed-repository packs is useful in shadow mode too (shadow uploads a pack
per recorded write, and a dead shadow write leaves an orphan), so `CollectGarbage`
and `CollectRemoved` run in shadow mode as well, doing the orphan and
removed-repository sweeps only (there are no checkpoints, hence no covered set).
Off mode opens no bucket: every entry point returns `FailedPrecondition`.

## Work split: what Opus writes, what Haiku drafts

Per the engineering charter, Opus (the main chat) writes everything that deletes
data, is concurrent, is a compare-and-swap, or is crash recovery; Haiku drafts
scoped support from an exact spec, actively monitored, output checked by Opus.

**Opus writes:**

* the migration (`sqlstore/migrations.go`) and all new `sqlstore` methods
  (checkpoint transaction, lease CAS, tombstone, the changed `DeleteRepository`,
  `ChangesetPacks`, `ListRepositories`, `ListRemovedRepositories`);
* the `BlobStore` `List`/`Delete` interface and the `blobstore` implementation;
* `checkpoint.Checkpointer` (upload-then-commit, crash points);
* `gc.Collector` (the mark and sweep that deletes packs, and removed-repository
  cleanup);
* the syncer changes (hydration from a checkpoint, the below-checkpoint
  re-hydration decision, the covered-pack-missing restart);
* the `maintenance.Runner` (the lease-guarded concurrent background task);
* the `verify_objects.go` change;
* `gitops.BuildPackFrom`;
* the config validation, the proto, the service handlers, and the `serve.go`
  wiring;
* the integration, `-race` and crash tests, and the e2e test.

**Haiku drafts (exact spec, Opus verifies):**

* the spec tests `zz_*_spec_test.go` for every WP (read only this file and the
  foundation signatures), under `<workspace>/spec-tests7/<WP>/...`;
* the CLI subcommand plumbing in `cmd/graft/storage.go` (flag binding, dispatch,
  human/JSON output) mirroring the existing `import|state|verify`;
* the SDK result structs and methods mirroring `sdk/storage.go`;
* first drafts of the STORAGE.md and CONFIGURATION.md prose and tables (Opus
  edits for correctness and fills the measured numbers);
* table-driven unit-test scaffolding for the pure `sqlstore` methods from the
  exact specs above (Opus owns the concurrency and deletion tests).

## Risks

* **Bucket mtime reliability.** The orphan grace uses `BlobInfo.ModTime`. File
  and mem buckets are exact; S3/GCS/Azure are untested here. A provider with a
  coarse or wrong mtime could, in principle, reclaim a fresh orphan. Mitigation:
  a generous default grace (1h ≫ the write window); the pending-record
  alternative is kept in reserve. Documented as an S3-untested limit.
* **`pack-objects` non-determinism.** Two nodes could build byte-different
  checkpoint packs for the same `N`; the maintenance lease makes concurrent
  builds a non-case, and a resumed build after a crash simply uploads a fresh
  content-addressed pack.
* **Re-hydration discards a far-behind cache's local objects** (correct but
  wasteful); rare, because an actively-served node stays near head.
* **Unreachable-object pruning** can make a *closed/abandoned* review's diff
  unavailable if its commits are reachable from no reference at the checkpoint;
  usually reviews target live branches. Open question below.
* **Unbounded local-cache growth** (no local `git gc`) is out of scope; an
  operator reclaims local disk by removing and re-hydrating a cache. Noted in
  STORAGE.md.
* **Lease held by a crashed node** blocks maintenance of that repository until
  `lease_ttl` expires (bounded; default 10m).
* **Large-bucket listing** for removed-repository cleanup is `O(objects under
  the prefix)`; per-repository GC is bounded by one repository's prefix.

## Open questions for the PM

1. **`graft storage gc` scope.** This plan makes `CollectGarbage` repository-
   scoped (matching `import`/`verify`'s `-repo`) and folds the removed-repository
   tombstone sweep into every call as a cheap global step. Would you prefer a
   separate storage-scoped `graft storage gc --all` that iterates every
   repository in a storage plus the tombstones? (The background task already
   covers everything; this is only about the operator CLI shape.)
2. **Open-review objects.** Should the checkpoint pack also retain objects
   reachable from the tips of **open** reviews, so a review's diff survives
   compaction even when its branch was deleted? It adds a `graft_reviews` read
   and extra tips to the pack build. Default in this plan: no (documented limit).
3. **Default `maintenance = "auto"`.** The background task runs by default in
   authoritative mode. Do you want it `off` by default for the first milestone-3
   release (operators opt in), with only the CLI available?
4. **Benchmark window.** I need a quiet window on the shared machine to measure
   bucket size and hydration time before/after for the STORAGE.md numbers (the
   other workstream's tests skew it, as the LOCK AND HISTORY benchmark noted).

## Foundation touchpoints and ownership

New files (mine): `internal/graft/corestore/checkpoint/**`,
`internal/graft/corestore/gc/**`, `internal/graft/corestore/maintenance/**`,
`internal/gitaly/service/storagecore/{checkpoint,gc}.go`,
`sdk/storage_gc.go`, `internal/graft/e2e/storage_gc_test.go`.
Changed (mine): `sqlstore/**`, `corestore/{core,types,metastore}.go`,
`blobstore/**`, `gitops/gitops.go`, `syncer/syncer.go`,
`service/storagecore/verify_objects.go`, `cmd/graft/storage.go`,
`proto/storage_core.proto` (+ generated code), `config_graft_storage.go`,
`doc/graft/STORAGE.md`.
Shared (my section only, listed in the report): `internal/cli/gitaly/serve.go`
(the storage core block — wire the maintenance runner),
`doc/graft/CONFIGURATION.md` (the `[graft.storage]` section).
I do not touch the land path (`internal/graft/landlane/**`, `land*.go`): the
checkpoint reads a database snapshot and the local objects, and GC touches only
the bucket and the database, so phase 7 needs no land-path change. If
implementation proves otherwise I stop and tell the PM.

## RULES (binding for every WP, in addition to PLAN6 RULES 1–15)

16. **Packs only.** Compaction deletes packs, never changeset, changelog or
    review rows. The changelog and its cursors are untouched.
17. **Database authoritative over liveness.** GC computes the live set from the
    database every run and never deletes a pack a committed changeset `> C` or a
    kept checkpoint references.
18. **Grace before orphan deletion.** An unreferenced pack is deleted only when
    older than `gc_grace`.
19. **One maintenance operation per repository across nodes** (the lease).
20. **Checkpoints are authoritative-mode only**; GC of orphans and removed
    repositories also runs in shadow mode (SHADOW SCOPE).
21. **Crash-safe and idempotent**: upload before record; row before pack delete;
    tombstone before prefix delete; a `Delete` of a missing key is success.
22. **Untrusted data** (RULE 10 extended): a checkpoint pack and its snapshot
    read from the database or the bucket are validated (pack SHA-256 against its
    key and `repo_id`; names and targets with git's rules) before anything
    touches a local repository.
