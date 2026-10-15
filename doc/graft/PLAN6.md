# Graft phase 6: the storage core (binding plan)

Status: binding contract for WP51–WP61. The Opus foundation (this file, the
`internal/graft/corestore` types and interfaces, the config, the proto and the
wiring stubs) is fixed. A work package changes a foundation file only when a
rule below says so. Anything that seems to need a change elsewhere goes into
`needs_outside_change` in the WP report; it is not done silently.

## Goal

Make the repository state of Graft live in a **strongly consistent database
plus object storage**, the way Scion does, so that Gitaly servers become
**re-creatable caches** that scale horizontally:

* Every reference change is a **changeset** appended to a per-repository
  write-ahead log in the database (`graft_changesets`), with a **per-reference
  compare-and-swap** against `graft_refs`, in one database transaction.
* The objects of a changeset are uploaded **before** the changeset is
  committed, as one content-addressed, immutable pack in object storage
  (`repos/<repo_id>/packs/<sha256>.pack`).
* The Graft changelog (ChangesetService) and reviews are written in the same
  database, atomically with the reference change for the changelog.
* Any node can serve any repository: before serving, it catches up its local
  git repository (the cache) from the database and the object storage, or
  builds it from nothing (hydrate).

Honest limit, to be stated in the docs: git still runs on a local repository.
"Stateless" means that the local repository is a cache that any node can
rebuild from the database and the object storage, not that nodes keep no
local state. There is no virtual filesystem in this phase.

This phase delivers milestone 1 (shadow mode, import, verify) and milestone 2
(authoritative mode, any node serves and hydrates). Garbage collection of
packs, checkpoint compaction and per-directory permissions are milestone 3,
after the first release.

## Modes (`[graft.storage] mode`)

* `off` (default, also the empty string): nothing changes. No database or
  bucket is opened.
* `shadow`: every reference transaction is also committed to the database and
  its pack uploaded, but the database **never rejects** a write: on a CAS
  mismatch the changeset is recorded with `forced = true` and the
  `graft_storage_shadow_divergences_total` counter is incremented. Any shadow
  error (database down, upload failure, timeout, panic) is logged and counted
  (`graft_storage_shadow_errors_total`) and the git write proceeds. The file
  changelog and the file review store stay the source of truth. Reads are not
  affected. Shadow mode works with or without Praefect.
* `authoritative`: the database is the source of truth. A reference
  transaction commits **only** if its database CAS succeeds (commit point =
  the Prepared phase of the reference-transaction hook). Reads and writes
  first catch up the local repository (EnsureFresh). The changelog and reviews
  are served from the database. Requires `graft.standalone = true`; it is
  rejected by config validation otherwise (Praefect is not supported in
  authoritative mode, see RULE 13).

## Configuration (foundation, `internal/gitaly/config/config_graft.go`)

```toml
[graft.storage]
mode = "authoritative"                   # off | shadow | authoritative
database = "sqlite:///var/opt/graft/meta.db"   # or postgres://user@host/db?sslmode=disable
bucket = "file:///var/opt/graft/objects"        # gocloud URL: file://, mem://, s3://, gs://, azblob://
node_id = "node-a"                       # default: os.Hostname()
sync_timeout = "10s"                     # EnsureFresh budget per RPC
shadow_timeout = "5s"                    # shadow work budget per transaction
```

Credentials are never in the TOML: Postgres uses the standard `PG*`
environment variables and `.pgpass` (pgx honours them), buckets use the
standard cloud SDK environment (AWS_*, GOOGLE_APPLICATION_CREDENTIALS,
AZURE_*), as `internal/backup/sink.go` does.

## Data model (foundation, `internal/graft/corestore/sqlstore/migrations.go`)

The schema is fixed by the foundation; WP51 implements it. Both dialects
(SQLite and Postgres) have the same tables and columns. Names in Go are in
`internal/graft/corestore/types.go`.

* `graft_repositories(repo_id PK, storage, relative_path, object_format,
  head_seq, log_seq, next_review_id, created_at, UNIQUE(storage, relative_path))`.
  `repo_id` is never reused: a repository removed and created again gets a
  new one, which invalidates the local caches of the old one.
* `graft_refs(repo_id, name BLOB, target TEXT, seq, PK(repo_id, name))`:
  the current references. `target` is a hex object ID or `ref:<name>` for a
  symbolic reference. `HEAD` is a row like any other.
* `graft_changesets(repo_id, seq, operation, actor, node_id, pack_key NULL,
  pack_bytes, forced, created_at, PK(repo_id, seq))`: the WAL. `seq` is dense
  per repository and starts at 1.
* `graft_changeset_refs(repo_id, seq, ordinal, name BLOB, old_target NULL,
  new_target NULL, PK(repo_id, seq, ordinal))`: the effective reference
  changes of a changeset. `old_target` is the **actual** previous database
  value (NULL = absent), `new_target` NULL = deleted.
* `graft_changelog(repo_id, log_seq, entry TEXT, PK(repo_id, log_seq))`: the
  Graft changelog (heads and tags only), `entry` = `changelog.Entry` JSON.
  Written in the same transaction as the changeset.
* `graft_nodes(repo_id, node_id, applied_seq, updated_at, PK(repo_id, node_id))`:
  how far each node's cache is (observability only, never used for decisions).
* `graft_reviews(repo_id, review_id, version, target_branch BLOB, closed,
  body TEXT, updated_at, PK(repo_id, review_id))`: reviews in authoritative
  mode (`body` = protojson of `gitalypb.Review`).

All child tables reference `graft_repositories` with `ON DELETE CASCADE`.

## Reference CAS semantics (RULE 5, binding)

The reference-transaction hook receives `<old> <new> <ref>` lines. git
reports the all-zero old value both for "must not exist" and for "forced".
The voter tells them apart in the Prepared phase, where git holds the locks
and the local repository still shows the old values:

| hook old value | local ref now | `RefUpdate.Expect` | database check |
|---|---|---|---|
| non-zero `X` (or `ref:T`) | any | `ExpectValue`, `Old = X` | current must equal `X` |
| zero | absent | `ExpectValue`, `Old = ""` | ref must be absent |
| zero | exists | `ExpectAny` | none (forced) |

A changeset whose updates all leave the database unchanged (for example a
forced deletion of an absent reference) writes nothing and returns
`CommitResult{NoOp: true}`. On mismatch the store returns `*ErrRefConflict`
listing every mismatching reference (expected and actual) and writes nothing
— unless `ChangesetInput.Force` is set (shadow mode), in which case it writes
the changeset with `forced = true` and returns the conflicts in
`CommitResult.Conflicts`.

The voter converts `*ErrRefConflict` into a
`git/updateref.MismatchingStateError` (first conflict; `ActualObjectID` = the
database value, zero OID if absent) wrapped in the hook error, so that
`UserLand`'s lost-race retry (`land.go`, `updateLandedReference`) classifies
it as a conflict exactly like a git lock-time mismatch.

**Files backend (superseded rule, see "REFTABLE RULE"):** the earlier text said to
skip force-deletions-only transactions on the files backend. That was wrong: for a
deletion without an expected old value the loose half is force-only too, so both
halves were skipped and the database kept a reference git had deleted. The binding
rule is now the REFTABLE RULE below.

## Write path (WP54, binding order)

In `GitLabHookManager.ReferenceTransactionHook`, Prepared phase, after
`enforceGraftPolicy` and before the Praefect vote (foundation wires the call):

1. Parse all lines (all references, not only heads and tags; symbolic values
   `ref:<target>` are accepted). Skip per the files-backend rule.
2. Resolve the repository in the database by `(storage, relative_path)`.
   Shadow: not tracked → skip silently (counter
   `graft_storage_untracked_writes_total`). Authoritative: not tracked →
   `FailedPrecondition` (the gate normally stops this earlier).
3. Compute the expectations (table above) with one `for-each-ref` run.
4. Build the pack: objects reachable from the new non-zero object targets and
   not reachable from any current local reference
   (`git rev-list --objects --stdin` with `--not --all`, piped into
   `git pack-objects --stdout`), written to a temporary file while hashing
   (SHA-256). Empty → no pack.
5. Upload the pack (`BlobStore.Put`, create-only; an existing key is
   success).
6. `MetaStore.CommitChangeset` with the updates, operation/actor/members meta
   (from the same helper the file changelog uses), node ID, pack key and size,
   and `Changelog` meta in authoritative mode.
7. Authoritative: on `*ErrRefConflict` return the mismatching-state error →
   git aborts the transaction. On any other error return `Unavailable`
   (`structerr.NewUnavailable`) → git aborts. On success remember
   `(repo_id, seq)` for the Committed/Aborted phases (process-local map keyed
   by SHA-256 of relative path + hook input, TTL 15 minutes, at most 4096
   entries).
8. Committed phase: call `LocalApplier.NoteCommitted(repoPath, repoID, seq)`
   so the node's watermark can advance without downloading its own pack.
   Never blocks (TryLock semantics, see syncer).
9. Aborted phase after a successful database commit (git failed to commit
   locally): log at error level and count
   `graft_storage_local_commit_failures_total`; the database is the truth and
   the next EnsureFresh repairs the local cache.

In authoritative mode the file changelog is **not** written (the database
changelog replaces it); in shadow mode it is written as today.

Repositories that use the MVCC reference backend are skipped in shadow mode
(counter `graft_storage_mvcc_skipped_total`) and rejected with
`FailedPrecondition` in authoritative mode.

## Read path and catch-up (WP55, binding)

`Syncer.EnsureFresh(ctx, repo)` (authoritative only; no-op otherwise):

1. Read `Repository` by key. Not found → if a local repository exists it is
   untracked → `FailedPrecondition("repository is not imported into the
   storage core")`; otherwise `NotFound`.
2. Read the local watermark `<gitdir>/graft/storage/applied` (JSON
   `{"repo_id":N,"seq":M}`). Missing local repository, or watermark with a
   different `repo_id` → hydrate (below). `seq == head_seq` → return.
3. Catch up: list changesets `(seq, head_seq]`, download and verify their
   packs, `git index-pack --stdin` each into the repository, then compute the
   final value of every changed reference **as of head_seq** and apply them in
   one `update-ref --stdin` transaction **without hooks**
   (`updateref.WithDisabledTransactions`, never the UpdaterWithHooks), then
   write the watermark (temp file + fsync + rename) and report it
   (`ReportApplied`, best effort, at most once per second per repository).
4. Hydrate: create a bare repository with the recorded object format and the
   reftable backend in the storage's temporary directory, apply all
   changesets from 1, set `HEAD`, write the watermark, then rename it into
   place. A local repository with a different `repo_id` is moved away to the
   storage's temporary directory first (never deleted in place).
5. Single flight per repository per node; a per-repository mutex protects the
   apply + watermark section. Lock contention from git (a concurrent local
   write holds the reference lock) is retried with backoff until
   `sync_timeout`, then `Unavailable`.
6. The watermark only advances contiguously: `NoteCommitted(seq)` with
   `seq == applied+1` advances it (and any already-noted higher contiguous
   seqs), otherwise the seq is remembered in memory and the next catch-up
   covers the gap.

Untrusted input (RULE 10): reference names read from the database are checked
with git's reference-name rules (`git.ValidateReference` or equivalent) and
targets with the repository's object hash before anything touches the local
repository; a pack's SHA-256 must match its key.

## Gate: which RPCs run in authoritative mode (WP56, binding)

A unary and a stream interceptor (`internal/graft/corestore/gate`), installed
on every listener through `server.WithUnaryInterceptor` /
`WithStreamInterceptor` (foundation wiring), apply only in authoritative
mode:

* `/gitaly.HookService/*` and `/gitaly.StorageCoreService/*` are passed
  through untouched (no EnsureFresh: hooks run while git holds locks).
* Repository-scoped RPCs (from `protoregistry.GitalyProtoPreregistered`):
  * MUTATOR not in the allowlist → `FailedPrecondition("<method> is not
    supported in authoritative storage mode")`.
  * Everything else → `EnsureFresh` first, then the handler.
* `RepositoryService/CreateRepository`: after the handler succeeded, register
  the repository with an initial changeset containing its `HEAD` (operation
  `create_repository`). If the registration says the repository exists, the
  local repository is moved to the temporary directory and `AlreadyExists` is
  returned. CreateRepository does not call EnsureFresh first.
* `RepositoryService/RemoveRepository`: delete the database row first, then
  run the handler; a not-tracked repository is just removed locally.
* Storage- and server-scoped RPCs pass through.

The allowlist lives in `internal/graft/corestore/gate/policy.go` (foundation
seeds it). WP56 proves every entry with an authoritative-mode test (the RPC's
reference changes reach the database: `VerifyRepository` reports no
divergence afterwards) and removes an entry it cannot prove, reporting it.
Adding an entry needs that proof.

Land retries: the foundation adds a call to `deps.GraftStorageSyncer`
(`EnsureFresh`, no-op when nil) before every retry attempt of the single land
path and the batch executor, so a retry after a database conflict sees the
new tip.

## Import, state and verify (WP57)

`StorageCoreService` (`proto/storage_core.proto`, foundation):

* `ImportRepository` (mutator): registers a local repository that the
  database does not know, in one transaction: changeset 1 with operation
  `import`, all references (including `HEAD`), one pack with every object
  reachable from them, the file changelog entries with their sequences
  preserved (so consumers' cursors stay valid) and the file reviews. Fails
  with `AlreadyExists` when tracked. Works in shadow and authoritative mode.
* `GetStorageState` (accessor): mode, tracked, repository ID, head sequence,
  this node's applied sequence, node ID.
* `VerifyRepository` (accessor): authoritative → EnsureFresh first; then
  compares every local reference with the database (divergences listed) and,
  when asked, runs `git fsck --connectivity-only`.
* `graft storage import|state|verify` subcommands of the `graft` CLI, through
  the SDK.

## Changelog and reviews in the database (WP58, WP59)

* WP58: `changelog.Source` interface (Scan, Subscribe) with the file
  implementation unchanged and a database implementation (Scan from
  `graft_changelog`; Subscribe = in-process notification from the voter plus
  polling, every 250 ms while a watcher is active). ChangesetService uses the
  database source in authoritative mode.
* WP59: `review/store.ReviewStore` interface extracted from `*store.Store`
  (same method set) and a database implementation (`next_review_id`
  allocation and `version` optimistic concurrency with retry in `Update`). The
  review server uses it in authoritative mode.

## Prerequisites (WP60)

* `internal/git/mvcc/manifest.go`: `ParseManifest` must not panic on crafted
  TOC offsets (`uint64 >= 2^63`), plus fuzz corpus entries.
* Verify that a fetched manifest hashes to its requested name.
* `updateref.parseStderr`: classify the MVCC backend's lock errors like
  reftable's.
* `update_with_hooks.go`: send `ReferenceTransactionAborted` when `Prepare`
  or the Prepared hook fails (not only when `Commit` fails).
* Replace the stale `doc/mvcc_rpc_flow.md` with a short note pointing to the
  storage core docs.

## Proof and docs (WP61)

* Multi-node end-to-end tests: two (and three) real gitaly processes with
  separate storage directories, one database and one bucket:
  * Write on A, read on B.
  * Concurrent lands on the same branch from both nodes: no lost update, every
    landed request in the database changelog exactly once.
  * A fresh node with an empty disk hydrates and serves.
  * A repository created on A and removed on B.
* Crash injection (test hooks in the voter): after the upload and before the
  database commit (orphan pack only, no changeset); after the database commit
  and before the git commit (node behind, repaired by the next EnsureFresh).
* Shadow mode over the existing benchmark: zero divergences.
* Bench: voter overhead and lands per minute with SQLite and Postgres, 1, 10
  and 50 writers.
* `doc/graft/STORAGE.md`: architecture, modes, configuration, operations
  (import, verify, rollback), limits, and the clean-room note.

## Foundation map (fixed; read before coding)

| What | Where |
|---|---|
| Shared types, errors, `PackKey`, `IsChangelogRef`, `Watermark` | `internal/graft/corestore/types.go`, `core.go` |
| `MetaStore` (+ `ChangesetInput`, `CommitResult`, `InitialState`, review errors) | `internal/graft/corestore/metastore.go` |
| `BlobStore`, `Freshener`, `LocalApplier`, `Core`, `NopFreshener`, `KeyOf` | `internal/graft/corestore/core.go` |
| In-process changelog notifications (`SubscribeChangelog`, `NotifyChangelog`) | `internal/graft/corestore/notify.go` |
| Schema (both dialects) | `internal/graft/corestore/sqlstore/migrations.go` |
| Opening the core from config | `internal/graft/corestore/open/open.go` |
| Test helpers (SQLite / per-test Postgres DB, file bucket, `Open`) | `internal/graft/corestore/coretest/coretest.go` |
| Config `[graft.storage]` + validation | `internal/gitaly/config/config_graft_storage.go` |
| Proto | `proto/storage_core.proto` (generated) |
| Hook wiring (`StorageVoter`, `WithStorageVoter`, `changelogMetaOf`) | `internal/gitaly/hook/storage_voter.go`, `referencetransaction.go` |
| Dependencies (`GraftStorage`, `GraftStorageSyncer`, getters) | `internal/gitaly/service/dependencies.go` |
| Land retries call `ensureStorageFresh` | `internal/gitaly/service/operations/{server,land,land_batch_server}.go` |
| Production wiring | `internal/cli/gitaly/serve.go` |
| Test-server wiring (`testserver.WithGraftStorage(core)`) | `internal/testhelper/testserver/gitaly.go` |
| `changelog.Source` + `FileSource` | `internal/graft/changelog/source.go` |
| `store.ReviewStore` | `internal/graft/review/store/iface.go` |
| Praefect registration of `ImportRepository` | `internal/praefect/coordinator.go` |

The stubs of the implementation packages (`sqlstore`, `blobstore`, `gitops`,
`voter`, `syncer`, `gate`, `service/storagecore`) have the binding exported
signatures and doc comments. A WP replaces the stub bodies of its package; it
does not change an exported signature (add unexported helpers freely, and new
exported identifiers only when this plan names them).

### Test environment

* Postgres for tests runs in Docker (`graft-pg`, `127.0.0.1:54329`, trust
  auth). `GRAFT_TEST_POSTGRES_DSN=postgres://postgres@127.0.0.1:54329/postgres?sslmode=disable`
  is exported by `~/scion/env.sh`, so `gtest.sh` runs the Postgres variants.
  `coretest.DatabaseURL(t, sqlstore.Postgres)` creates a fresh database per
  test. Never use the `postgres` database itself for data.
* There is no S3 server in this environment: bucket tests use `file://` and
  `mem://`; S3 tests run only when `GRAFT_TEST_S3_URL` is set.
* **Import cycles:** `testserver` imports `voter`, `syncer` and `gate`, and
  `coretest` imports `sqlstore` and `blobstore`. Tests of `sqlstore`,
  `blobstore`, `gitops`, `voter`, `syncer` and `gate` that use `coretest` or
  `testserver` are in the external test package (`package voter_test`, ...).
* Test servers with the storage core:
  `testserver.RunGitalyServer(t, cfg, setup.RegisterAll, testserver.WithGraftStorage(coretest.Open(t, coretest.Options{...})))`.
  Authoritative mode needs `cfg.Graft.Standalone = true` and a standalone
  GitLab client (see `internal/graft/e2e/e2e_test.go`, `startGitaly`). Two
  "nodes" are two servers built from two `testcfg.Build(t)` configurations
  (separate storage directories, same storage name `default`) whose cores
  share `DatabaseURL` and `BucketURL`.

## Work packages

Waves: WP51, WP52, WP53 and WP60 start at once; WP54, WP55, WP58 and WP59
draft against the signatures while wave 1 builds and complete once it is
merged; WP56 and WP57 after WP54 and WP55; WP61 last. Spec-test writers for
every WP start at once.

### WP51 — MetaStore on SQLite and Postgres

Owns: `internal/graft/corestore/sqlstore/**` (the table definitions in
`migrations.go` are fixed).

* `Open`: `ParseDSN`; SQLite with the documented DSN parameters
  (`sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=10000&_foreign_keys=on&_txlock=immediate")`);
  Postgres with `sql.Open("pgx", dsn)`. Apply the migrations with
  `migrate.MigrationSet{TableName: migrationTable}` and the dialect names
  `"sqlite3"` / `"postgres"` (see `internal/gitaly/storage/relational/sqlite.go`
  for the pattern). `Open` pings the database.
* Write queries once with `?` placeholders and rebind them to `$1..$n` for
  Postgres in one helper. Times: SQLite stores RFC 3339 text with
  nanoseconds in UTC; Postgres `TIMESTAMPTZ`. Booleans: 0/1 on SQLite.
* Transactions that fail with SQLite busy/locked errors, Postgres
  serialization failures (40001) or deadlocks (40P01) are retried up to 5
  times with a short jittered backoff; every other error is returned.
* `CommitChangeset` (one transaction):
  1. Lock the repository row (Postgres `SELECT ... FOR UPDATE`; SQLite's
     immediate transaction already holds the write lock).
     `ErrRepositoryNotFound` if absent.
  2. Validate the input: at least one update, non-empty and unique names,
     `Expect` known, `ExpectAny` updates ignore `Old`. Invalid input is a
     plain error (no write).
  3. Read the current targets of the named references (chunks of at most
     500 names) and evaluate every expectation (CAS table). Conflicts are
     listed in update order.
  4. Conflicts without `Force` → roll back, return `&ErrRefConflict{...}`.
  5. Effective updates = updates whose current value differs from `New`. None
     → roll back, return `CommitResult{NoOp: true, Conflicts: ...}` (no
     sequence is used).
  6. `seq = head_seq + 1`; insert the changeset (`forced` = Force and at least
     one conflict), its `graft_changeset_refs` rows (ordinal from 0, actual
     old value, NULL for absent), upsert or delete the `graft_refs` rows
     (`seq` = the new seq).
  7. If `Changelog` is set: one entry per effective update with
     `IsChangelogRef(name)` and neither value symbolic, sequences
     `log_seq+1...`, OldOID/NewOID = actual old / new value or the zero OID of
     the repository's object format (40 or 64 zeros), Operation, Actor
     (`strings.ToValidUTF8(actor, "�")`), Rewritten, Members,
     MembersTruncated from the meta, RecordedAt from the meta or now (UTC).
     Store `json.Marshal(entry)` (the Entry's own MarshalJSON keeps non-UTF-8
     names).
  8. Update `head_seq` and `log_seq`; commit; return the result.
* `RegisterRepository`: one transaction: insert the repository (unique
  violation → `ErrRepositoryExists`, also when found by a SELECT first),
  then the imported changelog entries (sequences must be > 0 and strictly
  increasing, else error; `log_seq` = last), the imported reviews (IDs must
  be decimal without leading zeros; version 1; `closed` = state is not
  `Review_STATE_OPEN_UNSPECIFIED`; `body` = protojson;
  `next_review_id` = max + 1), then the initial changeset through the same
  code as `CommitChangeset` (every update must be `ExpectValue` with an empty
  `Old`, else error). A new repository always gets a new `repo_id` (never
  reused, also after `DeleteRepository`).
* `Refs` reads `head_seq` and all references in one read transaction
  (Postgres `REPEATABLE READ`, read only).
* `ListChangesets`, `ScanChangelog`, `ReportApplied` (upsert), reviews as
  documented on the interface. `GetReview` / `UpdateReview` return
  `ErrReviewNotFound` for a non-numeric ID. `UpdateReview` with a stale
  version returns `ErrReviewVersionConflict`; with an unknown ID
  `ErrReviewNotFound`.
* Tests (external package `sqlstore_test`, every test for each
  `coretest.Dialects()`): every row of the CAS table; forced commits;
  NoOp; ref names with non-UTF-8 bytes round trip; changelog entry contents
  (zero OIDs, symbolic skipped, only heads/tags); `RegisterRepository` with
  history; `DeleteRepository` cascades and re-registration gets a new ID;
  `ListChangesets` ranges; reviews CRUD, ordering and version conflicts;
  concurrency: 32 goroutines × 20 commits each with `ExpectValue` on the same
  reference (each reads the current value first and retries on conflict) end
  with dense sequences and exactly 640 changesets; 32 goroutines on 32
  different references never conflict; two `Store`s on the same database
  (two "nodes") behave like one.

### WP52 — BlobStore on gocloud

Owns: `internal/graft/corestore/blobstore/**`.

* `Open`: register the drivers as `internal/backup/sink.go` does (fileblob,
  memblob, s3blob, gcsblob, azureblob); for `file://` create the directory
  (`os.MkdirAll`, mode 0o700) before opening.
* `Put`: if the key exists, return nil without reading `r`. Otherwise write
  with `blob.WriterOptions{IfNotExist: true}` and treat
  `gcerrors.FailedPrecondition` as success. If the driver reports
  `gcerrors.Unimplemented` for IfNotExist, fall back to a plain write (keys
  are content-addressed, so the content is the same) and document it. For a
  pack key, hash the content while writing; on mismatch cancel the write
  (cancel the writer's context before `Close`, so nothing is committed) and
  return `ErrBlobCorrupt`.
* `Get`: `gcerrors.NotFound` → `ErrBlobNotFound`; for a pack key return a
  reader that hashes the content and returns `ErrBlobCorrupt` instead of
  `io.EOF` on mismatch (and on a short read).
* Tests (external package): file and mem buckets; Put idempotent; concurrent
  Puts of the same key; corrupt content rejected on Put and on Get (write a
  wrong file directly into the file bucket); missing key; keys with nested
  directories; S3 variant only with `GRAFT_TEST_S3_URL`.

### WP53 — git operations

Owns: `internal/graft/corestore/gitops/**`.

Implement every function as documented in `gitops.go`. Binding details:

* Run git only through `repo.Exec` / `repo.ExecAndWait` with `gitcmd.Command`
  (never a shell). Revisions are passed as arguments only after validating
  them as object IDs of the repository's hash.
* `BuildPack`: `git rev-list --objects --not --all --not <tips...>` (the two
  `--not` flags make `--all` excluded and the tips included), its output piped
  into `git pack-objects --stdout` (no `--thin`), written to
  `os.CreateTemp(tmpDir, "graft-pack-*")` through a SHA-256 hasher. No output
  lines → no file, empty Pack. `BuildFullPack`: `git rev-list --objects --all`
  piped the same way.
* `IndexPack`: `git index-pack --stdin` (no `--strict`), stdin = r.
* `ReadRefs`: `git for-each-ref --format=%(refname)%00%(objectname)%00%(symref)`
  plus `HEAD` (`git symbolic-ref --quiet HEAD`, or its object ID when
  detached). `ReadRefValues`: the same, filtered to the exact names.
* `ApplyRefs`: one `updateref.New(ctx, repo, updateref.WithDisabledTransactions())`
  transaction (`Update(name, oid, "")`, `Delete`, `UpdateSymbolicReference`
  for symbolic targets including HEAD). If git reports a file/directory
  conflict for a transaction that deletes and creates, retry as two
  transactions (deletions first). `IsLockError` recognizes
  `updateref.AlreadyLockedError`, `updateref.ErrPackedRefsLocked` and
  reftable lock failures.
* `InitBare`: `git init --bare --object-format=<f> --ref-format=reftable <path>`
  through `gitCmdFactory.NewWithoutRepo`.
* `ValidateName`: `HEAD`, or `git.ValidateReference`. `ValidateTarget`:
  `hash.ValidateHex` for object IDs; `ref:` + `ValidateName` for symbolic.
* Watermark file JSON `{"repo_id":N,"seq":M}`; `ReadWatermark` of a missing
  file is `ok=false, err=nil`; of a corrupt file an error.
* Tests (external package, `gittest` repositories, SHA-1 and SHA-256 via
  `gittest.ObjectHashIsSHA256`-aware helpers): pack of new objects only, pack
  indexes into an empty repository and makes the tips' history complete with
  the earlier packs (`git fsck --connectivity-only`); empty pack; full pack;
  ReadRefs with symbolic refs and detached HEAD; ApplyRefs sets, deletes,
  symbolic HEAD, rejects `refs/../x`, `HEAD:x`, names with spaces and
  invalid OIDs without touching the repository; lock contention detected;
  watermark round trip and atomicity.

### WP54 — voter (write path)

Owns: `internal/graft/corestore/voter/**`. Depends on WP51–WP53 (draft
against the signatures first).

Implement "Write path" exactly. Binding details:

* Parse hook input lines `<old> SP <new> SP <ref>`; values are hex object IDs
  of the payload's object format or `ref:<target>`. Malformed input:
  authoritative → error (abort); shadow → counted and skipped.
* The repository is `repoFactory.Build(payload.Repo)` (it carries the
  quarantine when there is one); the database key is
  `corestore.KeyOf(payload.Repo)`; the pack's temporary directory is
  `locator.TempDir(payload.Repo.GetStorageName())`.
* Reference backend (`repo.ReferenceBackend`, compare by Name): see the
  REFTABLE RULE (authoritative: only reftable; shadow: files processed, MVCC
  skipped). There is no force-deletions-only skip any more.
* Every reference name of the hook input is checked with `gitops.ValidateName`
  in the parser; an invalid name is malformed input (authoritative: abort;
  shadow: counted and skipped).
* The pending map (relative path + SHA-256 of the input → repo ID, seq) has a
  TTL of 15 minutes and at most 4096 entries (oldest evicted).
  `Committed` pops it and calls `applier.NoteCommitted` with the repository
  path from `locator.GetRepoPath(ctx, payload.Repo, storage.WithRepositoryVerificationSkipped())`
  (strip the quarantine: build the path from storage and relative path
  only). `Aborted` pops it and, if it existed, logs at error level and
  increments `graft_storage_local_commit_failures_total`.
* After a commit that appended changelog entries, call
  `corestore.NotifyChangelog(repoID)`.
* Conflicts: return `fmt.Errorf("storage core: %w", updateref.MismatchingStateError{...})`
  for the first conflict (absent values as the zero OID). Other storage
  errors: `structerr.NewUnavailable("storage core: %w", err)`.
* Shadow: the whole Prepared work runs under `context.WithTimeout(ctx,
  core.ShadowTimeout)` and a `recover`; it never returns an error.
* Faults for tests: an exported package variable
  `var FaultHook func(point string) error` (nil in production) called with
  `"after_upload"` and `"after_commit"`; a non-nil error is returned from
  Prepared as if the process had died at that point (authoritative: abort).
  The `CrashEnv` variable (read once in `New`) makes the process exit with
  code 75 at the same points, for real-binary tests.
* Metrics (one `prometheus.Collector`): `graft_storage_changesets_total{mode,result}`
  with results `committed`, `noop`, `conflict`, `error`, `skipped`;
  `graft_storage_shadow_divergences_total`, `graft_storage_shadow_errors_total`,
  `graft_storage_untracked_writes_total`, `graft_storage_mvcc_skipped_total`,
  `graft_storage_local_commit_failures_total`, `graft_storage_pack_bytes_total`,
  histogram `graft_storage_prepared_seconds`.
* Tests (external package `voter_test`, `coretest.Open`, `gittest`): call
  `Prepared`/`Committed`/`Aborted` directly with a `gitcmd.HooksPayload`
  built for the repository; every CAS-table case; conflict error matches
  `errors.As(err, &updateref.MismatchingStateError{})`; shadow with a closed
  database returns nil and counts an error; shadow divergence is recorded as
  forced; untracked repository; pack content (index it into an empty
  repository); NoOp; files-backend force-deletion skip; authoritative
  changelog entries with meta; `NoteCommitted` called with the right seq;
  `FaultHook` at both points. Plus one end-to-end test through
  `testserver.WithGraftStorage` in authoritative mode: `UserCreateBranch`
  then the database has the branch; a CAS conflict injected by committing
  directly to the database makes `UserLand` retry and succeed.

### WP55 — syncer (catch-up and hydration)

Owns: `internal/graft/corestore/syncer/**`. Depends on WP51–WP53.

Implement "Read path and catch-up" exactly. Binding details:

* Repository path: `locator.GetRepoPath(ctx, repo, storage.WithRepositoryVerificationSkipped())`;
  the local repository exists when that directory exists and
  `locator.ValidateRepository` accepts it.
* Single flight with `golang.org/x/sync/singleflight` keyed by storage +
  relative path; the per-repository `sync.Mutex` (in a map, entries removed
  when idle) covers apply + watermark. `EnsureFresh` runs under
  `context.WithTimeout(ctx, core.SyncTimeout)`.
* Catch-up applies packs in sequence order (`Blobs.Get` → `gitops.IndexPack`),
  then one `gitops.ApplyRefs` with the final values as of head, then
  `gitops.WriteWatermark`. `gitops.IsLockError` → retry with backoff
  (10 ms doubling to 500 ms) until the timeout, then `Unavailable`.
* Hydration builds in `locator.TempDir(storage)/<random>/repo.git`, then
  `os.MkdirAll` of the target's parent and `os.Rename`. A stale local
  repository (other `repo_id`) is renamed into the temporary directory first
  and removed in the background.
* `NoteCommitted` records the seq in an in-memory set and, if the
  per-repository mutex is free (`TryLock`), advances the watermark through
  contiguous seqs; `EnsureFresh` also advances through them before
  downloading anything.
* Errors: database unavailable → `Unavailable`; untracked with a local
  repository → `FailedPrecondition("repository is not imported into the
  storage core")`; untracked without → `NotFound("repository not found")`.
* `Applied` reads the watermark. `ReportApplied` at most once per second per
  repository, errors only logged.
* Metrics: `graft_storage_sync_total{result}` (`fresh`, `caught_up`,
  `hydrated`, `error`), `graft_storage_sync_seconds`,
  `graft_storage_sync_changesets_total`, `graft_storage_sync_bytes_total`.
* Tests (external package): changesets produced with `gitops.BuildPack` and
  `MetaStore.CommitChangeset` from a source repository; EnsureFresh on an
  empty storage hydrates an identical repository (refs, HEAD, fsck);
  catch-up after more changesets; repo_id mismatch re-hydrates;
  untracked/not-found errors; concurrent EnsureFresh calls do one sync;
  NoteCommitted contiguity (out of order, with gaps); lock contention (hold
  a reference lock with a started `update-ref` transaction) retries and then
  succeeds; corrupt pack in the bucket fails without touching references;
  an invalid reference name in the database is rejected.

### WP56 — gate (authoritative-mode interceptors)

Owns: `internal/graft/corestore/gate/**` (`policy.go` may only lose entries,
or gain a special case described below). Depends on WP54, WP55.

Implement "Gate" exactly. Binding details:

* Method lookup: `registry.LookupMethod(info.FullMethod)`; unknown methods and
  non-repository scopes pass through. Target repository:
  `methodInfo.TargetRepo(req)`. Streams: wrap the `grpc.ServerStream` so that
  the first `RecvMsg` reads the request, runs the checks and EnsureFresh, and
  then returns it to the handler.
* `CreateRepository`: after the handler succeeded, read the object format
  (`repo.ObjectHash(ctx)` → `Format`), `HEAD` (`gitops.ReadRefs`), register
  with `InitialState{Changeset: &{Updates: [HEAD], Operation:
  "create_repository", Actor: identity.Actor(ctx, "unknown"), NodeID}}` and
  write the watermark `{repoID, 1}`. `ErrRepositoryExists` → move the new
  local repository into the temporary directory, return `AlreadyExists`.
* `RemoveRepository`: `DeleteRepository` first; if it was tracked and the
  local handler returns NotFound, return success.
* Proof tests (external package `gate_test`, `testserver.WithGraftStorage`,
  authoritative): one table-driven test with a representative call for every
  allowlist entry, each followed by "local references == database
  references" (`gitops.ReadRefs` vs `MetaStore.Refs`). An entry that cannot
  be proven is removed from `policy.go` and reported. If
  `RepositoryService/WriteRef` of `HEAD` is not reported by the hook (no
  `ref:` line reaches the voter), keep the entry and add a post-handler step
  in the gate that commits the new `HEAD` target as a changeset (operation
  `write_ref`). Plus: a blocked mutator returns `FailedPrecondition`; an
  accessor on a second node sees the first node's write; HookService calls
  are not intercepted; an untracked repository returns `FailedPrecondition`.

### WP57 — StorageCoreService, SDK and CLI

Owns: `internal/gitaly/service/storagecore/**`, `sdk/storage*.go`,
`cmd/graft/storage*.go` plus one registration line in `cmd/graft`.
Depends on WP51–WP55.

* `ImportRepository`: storage core off → `FailedPrecondition`; MVCC backend →
  `FailedPrecondition`; tracked → `AlreadyExists`. Build
  `gitops.BuildFullPack` in `locator.TempDir`, upload, read refs
  (`gitops.ReadRefs`), the file changelog (`changelog.Scan(repoPath, 0, ...)`)
  and the file reviews (`store.Open(repoPath).List("", true, 0)`), and call
  `RegisterRepository` with changeset 1 (operation `import`, actor
  `identity.Actor(ctx, "import")`, every reference `ExpectValue` with empty
  `Old`, pack key and size, no changelog meta), the changelog history and the
  reviews. Write the local watermark `{repoID, 1}` (in both modes).
* `GetStorageState`: as in the proto; `applied_sequence` from
  `deps.GetGraftStorageSyncer().Applied`.
* `VerifyRepository`: authoritative → `EnsureFresh` first; compare
  `gitops.ReadRefs` with `MetaStore.Refs`, divergences sorted by name;
  `check_connectivity` → `git fsck --connectivity-only --no-dangling`.
* SDK: `func (r *Repo) StorageImport(ctx) (*StorageImport, error)`,
  `StorageState(ctx) (*StorageState, error)`,
  `StorageVerify(ctx, connectivity bool) (*StorageVerify, error)` with plain Go
  result structs mirroring the responses (follow `sdk/review.go`).
* CLI: `graft storage import|state|verify [--connectivity] [--json]` (follow
  `cmd/graft/review.go`).
* Tests: service tests in shadow and authoritative mode (import with
  changelog and reviews, state before/after, verify clean and with an injected
  divergence, connectivity), SDK tests with the existing fakes pattern, CLI
  tests.

### WP58 — changelog from the database

Owns: `internal/graft/corestore/dblog/**`, `internal/gitaly/service/changeset/**`.
Depends on WP51.

* `dblog.New(meta corestore.MetaStore, repoID int64) changelog.Source`: Scan
  pages through `ScanChangelog(after, 500)`; Subscribe forwards
  `corestore.SubscribeChangelog(repoID)` and a 250 ms ticker into one
  coalesced channel, stopped by the returned function.
* ChangesetService: `ListChangesets` and `WatchChangesets` read a
  `changelog.Source`: authoritative → `dblog.New` for the repository's ID
  (`GetRepository(corestore.KeyOf(repo))`; untracked → `FailedPrecondition`);
  otherwise `changelog.FileSource(repoPath)`. Behaviour otherwise unchanged
  (existing tests stay green).
* Tests: dblog unit tests on SQLite and Postgres; service tests in
  authoritative mode: lands produce dense entries with members; watch gets a
  live entry within 1 s; an entry committed by a second core (another node)
  reaches the watcher through polling.

### WP59 — reviews in the database

Owns: `internal/graft/corestore/dbreview/**`,
`internal/gitaly/service/review/server.go` (store selection) and the minimal
type changes in `internal/gitaly/service/review/*.go` that it needs.
Depends on WP51.

* `dbreview.New(ctx, meta, repoID) store.ReviewStore` with exactly the
  semantics of `*store.Store` (read `internal/graft/review/store/store.go`):
  Create sets ID, CreatedAt, UpdatedAt and clears Requirements/Approved;
  `ErrReviewNotFound` → `store.ErrNotFound`; List order and filters; Update
  re-reads and re-runs fn up to 10 times on `ErrReviewVersionConflict`, then
  returns an error.
* The review server's `openRepo` returns a `store.ReviewStore`: authoritative
  → `dbreview.New` (untracked → `FailedPrecondition`), otherwise
  `store.Open(path)`.
* Tests: one conformance suite run against both the file store and the
  database store (SQLite and Postgres); concurrent Updates from two stores on
  the same database lose no update; service tests in authoritative mode.

### WP60 — prerequisites

Owns: `internal/git/mvcc/manifest.go`, `internal/git/mvcc/cache.go` (manifest
hash check only), `internal/git/updateref/updateref.go` (`parseStderr` only),
`internal/gitaly/hook/updateref/update_with_hooks.go` (Aborted calls only),
their tests and fuzz corpora, `doc/mvcc_rpc_flow.md`.

Implement "Prerequisites" exactly, each with a regression test (fuzz corpus
entries under `testdata/fuzz/<FuzzName>/` in the `go test fuzz v1` format
for the manifest panic). `update_with_hooks.go`: when `updater.Prepare` or
the Prepared hook fails, call the Aborted hook (errors only logged) before
returning, as the Commit-failure path already does.

### WP61 — proof, benchmark and docs

Owns: `internal/graft/e2e/storage_*_test.go`, `examples/graft/demo/bench-storage.sh`,
`doc/graft/STORAGE.md`, the `[graft.storage]` section of
`doc/graft/CONFIGURATION.md`, and a phase-6 block in `the development smoke script`.
Runs after the integration of WP51–WP60.

Implement "Proof and docs" exactly. Multi-node tests are in-process (two or
three test servers sharing one database and bucket, see "Test
environment"), on SQLite and Postgres. Crash points use `voter.FaultHook`.
The benchmark script follows `examples/graft/demo/bench-batch.sh` (real
binaries, SQLite and Postgres, 1/10/50 writers, results table). STORAGE.md
covers architecture, modes, configuration, operations (import, verify,
switching shadow → authoritative and back), the honest limits (git still
needs a local repository; no GC of packs yet; Praefect unsupported in
authoritative mode; S3 untested here) and the clean-room note.

## Spec-test writers

Each WP gets an independent spec-test writer that reads ONLY this file and
the foundation files named in "Foundation map" (signatures and doc comments),
never the implementation. Tests go to
`<workspace>/spec-tests6/<WP>/<repo-relative package dir>/zz_<name>_spec_test.go`,
function names start with `TestSpec`, 4–10 focused tests on the edge cases
this plan states, in the package named in the WP's test bullet (external
`_test` packages where "Test environment" requires it). They cannot compile
the code (it is written in parallel): copy signatures exactly.

## Verified evidence (Opus, binding for WP54, WP56 and the review)

Facts checked by the orchestrator with the bundled git (`_build/bin/gitaly-git-master`,
2.56.0-rc1) and by reading the code. Use them; do not re-derive them differently.

**Hook input for symbolic references**
* `git symbolic-ref HEAD refs/heads/x`, and `symref-update HEAD refs/heads/x` run with
  `--no-deref` (what `localrepo.SetDefaultBranch` and `gitops.ApplyRefs` do), report one
  line `<zero> ref:refs/heads/x HEAD` in preparing, prepared and committed. The old value
  of a symbolic reference is always the zero OID, so the voter treats it as `ExpectAny`
  when HEAD exists (CAS table).
* Without `--no-deref`, `symref-update HEAD` follows HEAD and turns the branch HEAD points
  to into the symbolic reference; the hook then reports that branch's name
  (`<zero> ref:refs/heads/x refs/heads/main`). This is what git really changed, so the
  voter records the line literally (no special case). No caller in the tree does this:
  every `UpdateSymbolicReference` caller uses `updateref.WithNoDeref()`.
* When a branch moves, the hook reports only the branch line, never HEAD (HEAD is a
  symbolic reference in the database and follows by itself).
* git refuses one transaction that updates HEAD and the branch HEAD currently points to,
  even with `--no-deref` ("multiple updates for 'HEAD' (including one via its referent)").
  So `gitops.ApplyRefs` applies the other references first and HEAD alone afterwards; this
  split is required by git, not a workaround.
* With the files backend a deletion also produces an `aborted` state for the packed-refs
  transaction before the loose one; the voter's Aborted without a pending entry is a no-op.

**Which RPCs change references without the reference-transaction hook**
`updateref.New` installs the hook by default (`internal/git/updateref/updateref.go`
`WithRefTxHook`, unless `WithDisabledTransactions`); `UpdaterWithHooks` drives it
manually; receive-pack runs it. Every OperationService writer, ConflictsService/
ResolveConflicts, RefService/UpdateReferences and DeleteRefs, RepositoryService/WriteRef
(including HEAD via `SetDefaultBranch`), FetchRemote, FetchBundle, FetchSourceBranch,
ReplicateRepository, CleanupService/ApplyBfgObjectMapStream, and SSH/HTTP receive-pack go
through the hook. The repository-scoped RPCs that write references WITHOUT the hook are:
`RepositoryService/CreateRepositoryFromURL`, `CreateFork`, `CreateRepositoryFromBundle`
(clones with hooks disabled), `CleanupService/RewriteHistory` (its staging repository; the
live repository is then updated through a hooked fetch) and `ObjectPoolService/
FetchIntoObjectPool` (`refs/dangling/*` in the pool). None of them is in
`gate.AuthoritativeMutators`; they stay blocked in authoritative mode. Adding a fetch-type
RPC to the allowlist later still needs the WP56 proof test.

## WP55 RULE: catch-up never moves a reference backwards (binding, added by Opus after the WP55 gate)

Defect found by the gate: catch-up read the local references, then applied the
final values "as of head" unconditionally. A local writer that committed a newer
changeset N > head in between was overwritten (the branch went backwards), and the
watermark then advanced through N. A compare-and-swap on the values read just
before applying is not enough either: if the local read happens after N committed
locally but head was read before N, the CAS passes and the branch still regresses.

Binding protocol for every catch-up step (EnsureFresh and hydration's final apply
on an existing repository):

1. **Snapshot first:** read the current local values of every reference the step
   may change (`gitops.ReadRefValues`), BEFORE reading the database head used as
   the target of this step.
2. **Then read the head** `H` and the changesets `(watermark, H]`; compute the
   final value of each changed reference as of `H`.
3. **Apply with expectations:** apply only those references, each with its
   snapshot value as the expected old value, in one transaction
   (superseded: see the amendment and rulings below). Absent in the snapshot = must not exist.
4. If the transaction fails because a reference no longer has its expected value
   (`gitops.IsRefChanged(err)`), a local write committed in between: start the
   step again from 1 (bounded by `sync_timeout`; this is the correct design, a
   concurrent writer always makes progress). Lock contention (`IsLockError`)
   retries the same way.
5. Watermark: after a successful apply set it to `H`, drop every noted seq `<= H`,
   then advance through contiguous noted seqs `> H` (they are local commits that
   happened after the database commit, so the local repository contains them).

Why this is correct: the database commit of a local write happens before its git
commit. A write whose git commit is before the snapshot (1) committed in the
database before `H` was read (2), so `N <= H` and the final values include it. A
write whose git commit is after the snapshot changes a reference away from its
snapshot value, so the CAS (3) fails, or the write's own git CAS runs on top of the
`H` state; either way nothing goes backwards.

**[SUPERSEDED by the WP55 RULE amendment and the ORCHESTRATOR RULINGS below: do not add it] gitops addition (WP55 may add it; new file `internal/graft/corestore/gitops/apply_if.go`
and its tests; nothing else in gitops changes):**

```go
// ApplyRefsIf is ApplyRefs with an expected current value for every reference:
// expected[name] is the value the reference must have (empty Target = must not
// exist). It validates names and targets like ApplyRefs, applies the references
// other than HEAD in one --no-deref update-ref transaction with old values, and
// HEAD afterwards in its own transaction (git refuses HEAD together with its
// referent). A reference whose current value differs from its expectation fails
// the transaction with an error that IsRefChanged recognizes, and nothing of
// that transaction is applied. Every name in refs must have an entry in expected.
func ApplyRefsIf(ctx context.Context, repo *localrepo.Repo, refs, expected map[string]corestore.Target) error

// IsRefChanged reports whether err from ApplyRefsIf means that a reference no
// longer had its expected value (updateref.MismatchingStateError,
// updateref.ReferenceAlreadyExistsError, or git's "is at X but expected Y").
func IsRefChanged(err error) bool
```

For a symbolic expected value of HEAD use `symref-update` with an old target
(`ref` form) only if the bundled git accepts it with `--no-deref` (test it; the
orchestrator observed the `ref` old-target form being rejected without
`--no-deref`). If it does not work, apply HEAD with an expectation checked by a
read immediately before, and say so in the doc comment (HEAD changes are rare and
are not written by the land paths).

**WP55 RULE amendment (Opus review of the fix-round design, binding):** what is binding is the
INVARIANT, not the exact steps above: a catch-up never writes a value over a local reference whose
current value is the effect of a local commit with a sequence greater than the target of the
apply, and the watermark only claims sequences whose effects are in the local repository. The
fix-round design in `syncer.go` (fetch the range; under the lock read the local values; a value the
range does not explain makes it read the head again and extend the range, or repair when the head
did not move; before applying, extend the range through newer changesets of THIS node that touch a
moved reference; apply with the read values as expected old values; a changed reference restarts the
check) satisfies the invariant, because the database commit of a local write precedes its git commit
and the writer holds the reference lock from prepared to commit. It is accepted in place of the
snapshot-first steps. Binding consequences:
* ONE compare-and-swap implementation (see the ORCHESTRATOR RULINGS below: it is the syncer's own
  transaction code; gitops gets no ApplyRefsIf). No dead code: nothing unused may remain (e.g. an
  unused tickets field, doc paragraphs that describe a different algorithm than the code).
* The package documentation describes the algorithm the code implements.
* Symbolic references: see the next item (immediate targets); a two-level symref chain must not
  make every catch-up "repair" the reference.

**ORCHESTRATOR RULINGS for WP55 (override any gate finding that says otherwise; read this
main-tree copy of PLAN6.md, not the copy inside your worktree, which is older):**
1. (Revised after fix round 2 removed `gitops/apply_if.go`.) Do NOT recreate `apply_if.go`.
   The single expected-old-value implementation is the syncer's own transaction code
   (`checkedTransaction` / `applyChecked`); there must be no second one.
2. That code must check a symbolic old value UNDER THE LOCK too: a move from a symbolic value
   (in practice HEAD) is written as `symref-update <ref> NUL <new> NUL ref NUL <old-target>` (or
   the oid form for an object-ID old value) inside the `--no-deref` transaction, so a concurrent
   SetDefaultBranch/WriteRef of HEAD makes git fail the transaction (IsRefChanged-equivalent
   classification: re-check). Remove the read-then-`ApplyRefs` fallback for symbolic moves. Verify
   with the bundled git (`_build/bin/gitaly-git-master`) that this form works with `--no-deref`
   and add a regression test where HEAD changes between the check and the apply.
3. REFTABLE RULE for the syncer is still missing: a local repository that is not reftable is a stale
   cache (like a repo_id mismatch) in both `sync()` and `install()`'s restart condition (no loop),
   with the test described in the REFTABLE RULE.
4. Allowed gitops changes in this round (and nothing else in gitops): the immediate-symref-target
   change in `ReadRefs`/`ReadRefValues` (next item), reporting dangling symbolic references in
   `ReadRefValues`/`ReadRefs` (their value is `ref:<target>` even when the target does not exist),
   and `ValidateTarget` rejecting a symbolic target `ref:HEAD`. Each with a test.
5. Call `forgetPending` for the repository BEFORE the hydrated repository is renamed into place.
6. No dead code, package doc describes the code, the gate's concurrent regression test passes on
   SQLite and Postgres, and the whole syncer and gitops packages are green.

**Symbolic references in the snapshot (binding):** the snapshot and the expectations
compare the IMMEDIATE value of a reference.  and 
currently resolve chains of symbolic references (HEAD -> refs/heads/a -> refs/heads/b reports
the final target), so a CAS on such a reference can never match. WP55 may change those two
functions in  so that a symbolic reference reports its immediate target
(; verify empirically what for-each-ref %(symref) reports and
use a form that gives the immediate target), with a test of a two-level chain. This is the only
other gitops change allowed.

**Process note:** only ONE agent edits the WP55 worktree at a time. An agent that is told to stop
editing  leaves the files as they are and ends with a report.

**Also binding for WP55 (from the same gate):**
* `NoteCommitted` is a no-op unless the core is authoritative (shadow mode keeps no
  pending set and writes no watermark).
* Single flight: a caller must not be satisfied by a flight whose head read
  happened before the caller arrived (read-your-writes across nodes). Record the
  time each flight read its head; a caller that arrived later starts or joins a
  new flight after the current one ends.
* Regression test (required): the gate's repro, in package `syncer_test`, on
  SQLite and Postgres: a goroutine imitating the hook on `refs/heads/main`
  (Start/Update/Prepare, CommitChangeset with ExpectValue, Commit, NoteCommitted),
  a goroutine committing ExpectAny changesets on another reference (another node)
  every few ms, and two goroutines looping on EnsureFresh for a few seconds: zero
  CAS conflicts on `main`, and after a final EnsureFresh the local references equal
  `MetaStore.Refs` and the watermark equals the head.

## REFTABLE RULE: authoritative mode uses reftable repositories only (binding, added by Opus after the WP54 gate)

Root cause of the WP54 gate finding: the files backend deletes a reference with
two reference transactions (packed-refs half, then loose half) whose hook input
can be identical (`<zero> <zero> ref` for a forced deletion), so the hook cannot
tell a lone forced deletion from the packed-refs half of a checked one. reftable
uses one transaction. In authoritative mode the local repositories are caches the
storage core controls (hydration already creates reftable repositories), so the
format is fixed instead of guessing transaction pairs.

* **Voter (WP54), authoritative:** a repository whose backend is not reftable
  (files or MVCC) → `FailedPrecondition("authoritative storage mode requires a
  reftable repository")`; nothing is committed.
* **Voter (WP54), shadow:** files-backend repositories are processed like reftable
  ones (every transaction, including force-only ones; a forced deletion is
  `ExpectAny` and idempotent). A conflict whose actual value is absent and whose
  new value is absent (the reference was already deleted, typically by the
  packed-refs half) is not a divergence: do not increment
  `graft_storage_shadow_divergences_total` for it. MVCC is still skipped.
* **Gate (WP56), CreateRepository in authoritative mode:** if the request's
  `reference_backend` is unspecified, run the handler with
  `gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendReftables)` so the
  repository is created with `--ref-format=reftable`; if it asks for any other
  backend → `FailedPrecondition`. Test: the created repository is reftable.
* **Syncer (WP55):** in authoritative mode a local repository whose backend is not
  reftable is a stale cache, handled exactly like a `repo_id` mismatch (moved away
  into the temporary directory, then hydrated as reftable).
* **Import (WP57):** importing a files-backend repository is allowed (the pack and
  the references do not depend on the format). In authoritative mode the next
  EnsureFresh replaces the local copy with a reftable one (syncer rule above);
  document this in STORAGE.md together with `git refs migrate
  --ref-format=reftable` as the in-place alternative.
* **Tests:** WP54's sentinel test that asserted the old files-backend behaviour is
  replaced by: authoritative files-backend write → FailedPrecondition; shadow
  files-backend `DeleteRefs` without old values → the database no longer has the
  reference and no divergence is counted; reftable `DeleteRefs` (authoritative) →
  the database no longer has the reference.

## IMPORT RULE: a resumable import; head_seq 0 means "import not complete" (binding, added by Opus after the WP57 gate)

Root cause of the WP57 gate findings: the plan asked for an import in ONE transaction, but the
pack key contains the repository ID, which exists only after the registration. The import
therefore has to register first, upload, then commit; a crash in between left a registration
that could neither be retried (AlreadyExists) nor served, and the syncer could move the only
local copy away. Holding the database transaction during the upload is rejected (it would
block every SQLite writer for the whole upload); a staging pack key is rejected (the syncer
rightly refuses a pack key of another repository). The state is made explicit instead:

* **State:** a tracked repository with `HeadSeq == 0` is an import that is not complete. No other
  path creates such a row: the gate's CreateRepository always registers with changeset 1.
* **ImportRepository (WP57):**
  1. Untracked → `RegisterRepository(key, format, InitialState{Changelog: history, Reviews:
     reviews})` with NO changeset (history and reviews are recorded atomically with the row).
     Tracked with `HeadSeq == 0` → resume with its ID (history and reviews are already there; do
     not register again). Tracked with `HeadSeq > 0` → `AlreadyExists`.
  2. Build the full pack, upload it under `PackKey(id, sha)`.
  3. `CommitChangeset` changeset 1: every reference `ExpectValue` with empty `Old`, operation
     `import`, the pack key and size. A conflict here (a concurrent import resumed first) is
     returned as `AlreadyExists`.
  4. Write the local watermark `{id, 1}`.
  On any failure, nothing is rolled back and nothing is deleted: the error tells the operator to
  run ImportRepository again, which resumes. Remove `abandonImport` (and any delete-by-key).
* **Syncer (WP55 code, changed in the WP57 fix round):** a tracked repository with `HeadSeq == 0`
  → `FailedPrecondition("repository import is not complete; run ImportRepository again")`. It
  never hydrates, catches up, moves away or deletes a local repository in that state.
* **Voter (WP54 code, changed in the WP57 fix round):** a repository with `HeadSeq == 0` is
  treated like an untracked one (authoritative: `FailedPrecondition`; shadow: skipped and counted
  as untracked).
* **Allowed files for this rule in the WP57 fix round:** `internal/gitaly/service/storagecore/**`,
  `internal/graft/corestore/syncer/syncer.go` (+ a test file), `internal/graft/corestore/voter/voter.go`
  (+ a test file). Nothing else.
* **Tests:** interrupted import (registration done, no changeset) is resumed by a second
  ImportRepository on SQLite and Postgres and ends with VerifyRepository clean; EnsureFresh on a
  head-0 repository returns FailedPrecondition and leaves the local repository untouched (same
  path, same references); an authoritative write to a head-0 repository is rejected.

## STALE CACHE RULE: caches of removed repositories (binding, added by Opus after the WP56 gate)

Root cause of the WP56 gate finding: after RemoveRepository on one node, another node keeps its
cached copy; that copy was then treated as an untracked repository (FailedPrecondition "not
imported") and blocked CreateRepository (AlreadyExists). The watermark file is the proof that a
local repository is a cache managed by the storage core:

* A local repository WITH a watermark whose key is not in the database (or whose watermark names a
  repo_id the database row does not have, which is already the repo_id-mismatch rule) is a stale
  cache. A local repository WITHOUT a watermark whose key is not in the database is untracked
  (unchanged: FailedPrecondition "not imported").
* **Syncer:** EnsureFresh on a stale cache of a removed repository moves it into the storage's
  temporary directory (removed in the background, never deleted in place) and returns
  `NotFound("repository not found")`.
* **Gate, CreateRepository:** before running the handler, a stale cache at the target path (watermark
  present, key not in the database) is moved away the same way, so the creation proceeds.
* **Allowed files in the WP56 fix round:** `internal/graft/corestore/gate/**` and
  `internal/graft/corestore/syncer/syncer.go` (+ a test file). The WP57 fix round may change other
  parts of `syncer.go` for the IMPORT RULE at the same time: keep your change local to the
  untracked/stale branch of the code, so the merge of the two is mechanical.
* **Tests:** two nodes sharing a database and bucket: create on A, read on B (hydrates), remove on A;
  then on B RepositoryExists reports false (NotFound internally), CreateRepository succeeds and
  registers a new repo_id, and a write on B is visible on A after A's catch-up.

The repository key is normalized by `corestore.KeyOf` (cleaned relative path), so two spellings
of one path are one key (fixed in the foundation by the orchestrator).

## LOCK AND HISTORY RULE (binding, added by Opus after the WP61 benchmark)

### A. No slow work under the reference lock; reftable waits like packed-refs

Finding: in authoritative mode 50-84% of concurrent writes to one repository failed with
"reference is already locked". Two root causes:

1. **Configuration gap inherited from Gitaly:** `internal/git/gitcmd/command_factory.go` gives
   repository-wide reference locks a long wait (`core.packedRefsTimeout=10000`, with the comment
   that context cancellation still ends the wait early) and loose references 1 s, but never sets
   `reftable.lockTimeout`, so reftable's single, repository-wide lock waits only git's default
   100 ms. Fix: add `ConfigPair{Key: "reftable.lockTimeout", Value: "10000"}` next to the
   packed-refs one, with a comment that reftable's lock is repository-wide like packed-refs.
   This is the policy Gitaly already applies to repository-wide locks, not a new workaround.
2. **Design flaw in the write path:** the voter built the pack, uploaded it and committed to the
   database while git held the repository-wide reftable lock (Prepared phase), 40-150 ms per
   write. Fix: build and upload the pack in the **Preparing** phase, before git takes the locks
   (the hook manager calls Preparing before `updater.Prepare()` on the manual path, and the
   bundled git emits `preparing` for its own transactions):
   * `hook.StorageVoter` gains `Preparing(ctx, payload, changes) ` (no error that aborts: a
     failure is remembered and Prepared then does the work itself, as before), and the hook
     manager calls it for `ReferenceTransactionPreparing`.
   * The voter keeps the prepared pack in its pending structure keyed like the Prepared/Committed
     entries (relative path + SHA-256 of the input), with the same TTL and size bound; an entry
     that is never consumed (transaction aborted before Prepared) is dropped, its uploaded pack
     is an orphan like any other (GC is milestone 3).
   * Prepared uses the pack prepared for the same input when there is one; only the expectations
     read (`for-each-ref`, cheap) and the database commit run under the lock. Correctness: the
     pack holds the objects reachable from the new tips and not from the references at Preparing
     time; every object reachable from those references is in an earlier pack (invariant I1), so
     excluding fewer references than at Prepared time only adds objects, never loses one.
   * Shadow mode follows the same split (the shadow timeout covers both phases).
3. **Tests:** an authoritative-mode test with 10 concurrent `UserCommitFiles` onto 10 different
   branches of one repository: all succeed, and the database has all 10 branches (SQLite and
   Postgres). A voter unit test that Preparing+Prepared uploads exactly one pack and that
   Prepared without a preceding Preparing still works. A test that the Prepared phase does no
   upload when Preparing prepared the pack (count `Blobs.Put` calls with a wrapping BlobStore).
4. **Benchmark:** rerun `examples/graft/demo/bench-storage.sh` and replace the table and the
   "Concurrent writers fail" paragraph and limit in `doc/graft/STORAGE.md` with the new measured
   numbers (numbers from the real run only).

### B. Shadow history reaches the database before authoritative mode serves it

Finding: shadow mode does not append the database changelog and reviews live in files, so after
switching to authoritative the database changelog reuses the sequences of file-log entries written
in the shadow period (consumer cursors break) and shadow-period reviews are invisible.

* **ImportRepository on a tracked repository** (`HeadSeq > 0`) no longer returns AlreadyExists: it
  brings the history written since the import into the database: every file-changelog entry with a
  sequence greater than the database's `log_seq` (sequences preserved, appended in order) and every
  file review whose ID is not in the database (`next_review_id` = max + 1). References are not
  touched. It is idempotent and returns the counts in the existing response fields
  (`sequence` = current head). Operators run it once while switching (writers stopped), which
  `graft storage import` does.
* New MetaStore method (foundation interface + sqlstore implementation, both dialects):
  `ImportHistory(ctx, repoID int64, changelog []changelog.Entry, reviews []*gitalypb.Review) (entries, reviews int, err error)`
  in one transaction: entries must have sequences strictly increasing and greater than the current
  `log_seq` (else an error, nothing written); reviews whose ID already exists are skipped
  (superseded by the REVIEW AMENDMENT below).
* **Guard in authoritative mode:** the gate refuses to serve a tracked repository whose local file
  changelog has entries beyond the database's `log_seq` (`FailedPrecondition("the shadow-mode
  history of this repository is not in the storage core yet: run graft storage import")`). The
  check reads the last sequence of the file log (cheap: the changelog package's offset index) and
  is cached per repository per process once it passed.
* **REVIEW AMENDMENT (binding, added by Opus after the phase-6 review):** skipping every existing
  ID lost the shadow-mode changes of a review that the first import had copied (closed, landed,
  approved or edited in shadow mode, where the database is never written), and the guard above did
  not see review-only shadow history (the review service never writes the changelog), so a switch
  without the import let `InsertReview` hand out the IDs of shadow-mode reviews again. One rule,
  `corestore.ReviewNeedsImport(file, db *gitalypb.Review) bool` (new exported function in
  `internal/graft/corestore/reviewimport.go`), decides both: a file review needs importing when the
  database has no review with its ID, or when the database review is the same review (equal
  `created_at`: an earlier import copied it) and the file's `updated_at` is later. A database
  review changed later (authoritative mode), an unchanged one and a different review that only
  shares the ID are kept. `ImportHistory` inserts the new ones and replaces the changed ones with
  `version + 1` (`UPDATE ... WHERE version = <read version>`), counting both. The guard also lists
  the file reviews and the database reviews and refuses the repository (same message, metadata
  `review_id`) while any file review needs importing; the result is cached like the changelog
  check (authoritative mode never writes the file review store).
* STORAGE.md: replace the documented workaround with this procedure.

### Ownership for this fix round (one Opus agent, worktree IT)

`internal/git/gitcmd/command_factory.go` (one config pair), `internal/gitaly/hook/storage_voter.go`
and `referencetransaction.go` (Preparing call), `internal/graft/corestore/voter/**`,
`internal/graft/corestore/metastore.go` + `sqlstore/**` (ImportHistory), `internal/gitaly/service/storagecore/**`,
`internal/graft/corestore/gate/**` (the guard), `internal/graft/changelog/**` (only a helper that
returns the last sequence of the file log, if one is missing), `doc/graft/STORAGE.md`,
`doc/graft/CONFIGURATION.md`, tests of these packages. Nothing else.

## RULES (binding for every WP)

1. **Clean room.** Never read anything under `ee/`, and never run
   `git show`, `git log -p`, `git diff` or `git checkout` on `ee/` paths or on
   commits that touched them. Do not copy code from `_build/deps/git-mvcc`
   (GPL); reading its documentation is fine.
2. **Vendor neutral.** Only gocloud URLs for buckets and `database/sql` with
   the existing `mattn/go-sqlite3` and `jackc/pgx/v5/stdlib` drivers. No new
   go.mod dependencies.
3. **Commit point.** In authoritative mode the database commit in the
   Prepared phase is the only commit point. A changeset row never references
   a pack that is not completely uploaded.
4. **Shadow never hurts.** In shadow mode no storage-core error, timeout or
   panic may fail or noticeably delay a git write: everything runs under
   `shadow_timeout` and `recover`, and is logged and counted.
5. **CAS semantics** exactly as in the table above.
6. **No sync inside hooks.** EnsureFresh never runs for HookService or inside
   a reference transaction. Hook phases never block on the syncer's
   per-repository mutex.
7. **Watermark** = `(repo_id, seq)`, advanced only contiguously, written
   atomically after the references are applied.
8. **Typed errors** as listed: `*ErrRefConflict` → mismatching-state error;
   database unavailable → `Unavailable`; untracked → `FailedPrecondition`.
9. **Both dialects.** SQLite and Postgres pass the same conformance suite.
   Postgres tests use `GRAFT_TEST_POSTGRES_DSN` and skip only when it is
   unset; the Opus gate runs them against a Docker Postgres.
10. **Untrusted data.** Everything read from the database or the bucket is
    validated before it touches a local repository (see read path).
11. **Metrics** are named `graft_storage_*` and registered once.
12. **MVCC repositories** are skipped (shadow) or rejected (authoritative).
13. **Authoritative mode requires standalone mode** and is not supported
    behind Praefect (validated at startup, documented).
14. **Tests**: every WP has unit tests; a separate Haiku writes spec tests
    (`zz_*_spec_test.go`) from this file only.
15. **Process** (as in earlier phases): work only in your worktree, own only
    the files listed for your WP, no commits, report `needs_outside_change`.
