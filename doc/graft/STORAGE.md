# Graft storage core

The storage core keeps the state of every repository in a strongly consistent
database plus object storage, so that Gitaly servers become caches that any
node can rebuild. It is configured in the `[graft.storage]` section (see
[CONFIGURATION.md](CONFIGURATION.md#graftstorage)) and is off by default.

This page covers the architecture, the modes, the configuration, the operations
(import, verify, switching modes), the benchmark and the limits. The binding
design is [PLAN6.md](PLAN6.md).

## Architecture

```plaintext
            clients (SDK, graft CLI, git over SSH/HTTP)
                 |                       |
          +------v------+         +------v------+
          |   node A    |         |   node B    |      any node serves any repository
          |  gate       |         |  gate       |      (authoritative mode)
          |  voter      |         |  voter      |
          |  syncer     |         |  syncer     |
          |  local git  |         |  local git  |      local repositories are caches
          +--+-------+--+         +--+-------+--+
             |       |               |       |
   changesets|       |packs          |       |
             v       v               v       v
       +-----------+  +--------------------------+
       | database  |  | object storage (bucket)  |
       | SQLite or |  | repos/<id>/packs/        |
       | Postgres  |  |   <sha256>-<nonce>.pack  |
       +-----------+  +--------------------------+
```

- **Changesets.** Every reference change is a changeset appended to a
  per-repository write-ahead log in the database (`graft_changesets`). Its
  sequence is dense per repository and starts at 1. The current references are
  in `graft_refs`, `HEAD` included (a symbolic reference is stored as
  `ref:<target>`).
- **Packs.** The new objects of a changeset are uploaded **before** the
  changeset is committed, as one immutable pack under a key of its own,
  `repos/<repo_id>/packs/<sha256>-<nonce>.pack`: the SHA-256 of its bytes,
  checked on every write and read, and a random nonce drawn for that upload.
  A pack object belongs to exactly one changeset: two writes with byte-identical
  packs store two objects, and a write never relies on an object it did not
  upload. Objects are created only if their key is absent, atomically: on S3,
  GCS and Azure by a conditional request (untested here); on file buckets by
  link(2), because gocloud's file driver does not make its own create-only write
  atomic (it checks that the key is absent, then renames onto it, so two writers
  of one key would both succeed). A file bucket therefore needs a filesystem
  with hard links. A changeset never references a pack that is not completely
  uploaded. Changesets written by v0.1.0 keep their keys
  (`repos/<repo_id>/packs/<sha256>.pack`, shared by identical packs), which
  stay readable; the database refuses such a key in a new changeset, so every
  node must run the same version.
- **Write path (the voter).** Gitaly runs the reference-transaction hook for
  every reference update. In the Preparing phase, before git locks the
  references, the voter builds the pack of the new objects and uploads it: the
  objects that no local reference reaches yet in authoritative mode, where
  every local reference value went through the database, and the objects that
  no value recorded in the database reaches in shadow mode, where some writes
  are not recorded. In the Prepared phase, while git holds the
  reference locks, it only reads the expected old values (one `for-each-ref`)
  and commits the changeset with a per-reference compare-and-swap against
  `graft_refs`, in one database transaction. The Preparing phase never fails a
  write: when it did not prepare the pack (it failed, or the transaction did not
  run it), the Prepared phase builds and uploads the pack itself. Gitaly's own
  reference updates (`OperationService` writers, `UserLand` included) run the
  Preparing phase in both modes. For the transactions that git runs the hook
  for itself (pushes, `WriteRef`, `UpdateReferences`, `DeleteRefs`),
  `gitaly-hooks` forwards git's `preparing` state in authoritative mode, so
  their pack is built before the lock too; in shadow mode it does not forward
  it (as upstream Gitaly, which keeps Praefect's votes unchanged), so their
  pack is built in the Prepared phase, under the lock. In authoritative mode
  the database commit is the commit point: if it fails, git aborts the
  transaction. A compare-and-swap conflict is reported to the land path like a
  lost git race, so `UserLand` catches up and retries.
- **Read path (the syncer).** Before a request is served, the node catches its
  local repository up to the database head: it downloads and verifies the
  packs of the missing changesets, indexes them, and moves the changed
  references to their final values without hooks. A node without the
  repository hydrates it: a new bare reftable repository built from changeset
  1, renamed into place. A watermark file `<gitdir>/graft/storage/applied`
  (`{"repo_id":N,"seq":M}`) records how far the local copy is.
- **The gate.** In authoritative mode, gRPC interceptors on every listener
  catch the repository up before every repository-scoped RPC, register
  repositories that `CreateRepository` creates, delete the database row before
  `RemoveRepository` runs, and reject the mutators whose reference changes do
  not go through the hook (see [RPCs in authoritative mode](#rpcs-in-authoritative-mode)).
- **Changelog and reviews.** In authoritative mode the Graft changelog
  (`ChangesetService`, `graft log`, `graft watch`) is written in the same
  database transaction as the reference change, and reviews are stored in the
  database. A watcher on another node sees new entries by polling every 250 ms.
  The head and base commits of a review must be reachable from the references
  in the storage core: `CreateReview`, and `UpdateReview` when it moves the
  head, fail with `FailedPrecondition` for a commit that only the serving
  node's copy has (push it to a branch first). An open review keeps its
  commits alive after their branch is deleted; a closed or landed one does not.
- **Repository IDs.** A repository is identified by its storage name and
  relative path, and gets an ID that is never reused: a repository removed and
  created again is a new repository, and the old caches are recognized as
  stale by the ID in their watermark.

Everything read from the database or the bucket is validated before it touches
a local repository: reference names with git's rules, targets with the
repository's object hash, and every pack against the SHA-256 in its key.

## Modes

`[graft.storage] mode` selects one of three modes.

| Mode | Source of truth | What the storage core does |
|---|---|---|
| `off` (default, also empty) | local git | Nothing. No database or bucket is opened. |
| `shadow` | local git | Every reference transaction of an imported repository is also committed to the database, with its pack. The database never rejects a write: a compare-and-swap mismatch is recorded as a forced changeset and counted as a divergence. Any storage core error, timeout or panic is logged and counted, and the git write proceeds. Reads, the file changelog and the file review store are unchanged. Works with or without Praefect. |
| `authoritative` | database | A reference transaction commits only if its database compare-and-swap succeeds. Every request first catches the local repository up. The changelog and the reviews are served from the database. Requires `[graft] standalone = true`. |

Shadow mode is for validating the storage core next to a running deployment:
writes are mirrored, nothing is served from the database, and `graft storage
verify --connectivity` tells whether the database and the bucket follow git:
equal references, and every object they reach in the packs of the bucket. A
git write that shadow mode did not record (a counted shadow error, or a write
that landed while the repository's import was not complete) shows up as a
divergence until a later write records the reference as a forced changeset;
its objects are uploaded with the pack of the next write that builds on them.

Shadow mode never fails or noticeably delays a git write: the storage-core work
of one reference transaction (building and uploading its pack, then recording
it in the database) runs under `shadow_timeout`. A transaction whose work takes
longer is **not recorded**: the failure is counted in
`graft_storage_shadow_errors_total` and logged at error level, and the git write
proceeds. The database misses that write until the next write of the same
reference, which is recorded as a forced changeset (counted in
`graft_storage_shadow_divergences_total`); until then `graft storage verify`
reports the reference as diverging. Recording every write is therefore
guaranteed only while the shadow work of every transaction fits the budget; on a
heavily loaded machine some writes may not fit. Switch to authoritative mode only
when those counters and `graft_storage_untracked_writes_total` are at zero and
`graft storage verify --connectivity` is consistent on every repository (see
[Switching](#switching-from-shadow-to-authoritative)).

Authoritative mode serves reftable repositories only. Repositories created
through `CreateRepository` are created with the reftable backend, and the syncer
treats a local copy that is not reftable as a stale cache and hydrates a
reftable copy instead. A request for another reference backend is rejected.

### RPCs in authoritative mode

Repository-scoped RPCs that only read, and maintenance RPCs, run after the
catch-up. Mutators run only if their reference changes go through the
reference-transaction hook; the allowlist is
`internal/graft/corestore/gate/policy.go`:

- `OperationService`: every `User*` writer, including `UserLand`.
- `ConflictsService/ResolveConflicts`.
- `RefService/UpdateReferences` and `DeleteRefs`.
- `RepositoryService/WriteRef` (including `HEAD`), `CreateRepository`,
  `RemoveRepository`.
- `SSHService/SSHReceivePack` and `SmartHTTPService/PostReceivePack` (pushes).
- `ReviewService` writers.

Every other mutator fails with `FailedPrecondition` ("... is not supported in
authoritative storage mode"). This includes the RPCs that write references
without the hook: `CreateRepositoryFromURL`, `CreateFork`,
`CreateRepositoryFromBundle`, `CleanupService/RewriteHistory`,
`ObjectPoolService/FetchIntoObjectPool`, and the fetches (`FetchRemote`,
`FetchBundle`, `FetchSourceBranch`, `ReplicateRepository`). `HookService` and
`StorageCoreService` RPCs are never intercepted.

A repository that exists on disk but was never imported is answered with
`FailedPrecondition` ("repository is not imported into the storage core").

## Configuration

```toml
[graft]
standalone = true                               # required by authoritative mode

[graft.storage]
mode = "authoritative"                          # off | shadow | authoritative
database = "sqlite:///var/opt/graft/meta.db"    # or postgres://graft@db.internal:5432/graft?sslmode=verify-full
bucket = "file:///var/opt/graft/objects"        # gocloud URL: file://, s3://, gs://, azblob://
node_id = "node-a"                              # default: the host name
sync_timeout = "10s"                            # catch-up budget per request
shadow_timeout = "5s"                           # shadow work budget per transaction
```

Every key is described in [CONFIGURATION.md](CONFIGURATION.md#graftstorage).
Credentials are never in the TOML file: Postgres reads the standard `PG*`
environment variables and `.pgpass`, and buckets use the standard environment
of their cloud SDK (`AWS_*`, `GOOGLE_APPLICATION_CREDENTIALS`, `AZURE_*`).

All nodes of a deployment use the same `database` and `bucket` and different
`node_id` values. SQLite is for one node (the tests also run several servers
on one SQLite file, at the cost described in [Limits](#limits)); a deployment
with more than one node needs Postgres and a shared bucket. The database schema is
created and migrated at startup.

Startup fails when the mode is unknown, authoritative mode is set without
`standalone = true`, the database URL is missing or neither `sqlite:///` nor
`postgres://` (`postgresql://`), the bucket is missing or not a URL, `node_id`
contains spaces or slashes, or a timeout is not a positive Go duration.

## Operations

The `graft storage` commands are the operator's view. They take the server
from the workspace, or `-address`, `-storage` and `-repo` without one, and
`GRAFT_TOKEN` should be the shared token (`[auth] token`).

```shell
graft storage import -address unix:/var/opt/graft/run/gitaly.sock -repo group/project.git
graft storage state  -address unix:/var/opt/graft/run/gitaly.sock -repo group/project.git
graft storage verify -address unix:/var/opt/graft/run/gitaly.sock -repo group/project.git --connectivity
```

`--json` prints one JSON object. `verify` exits with an error when a reference
diverges or the connectivity check fails.

### Import

`graft storage import` (`StorageCoreService/ImportRepository`) registers an
existing repository: changeset 1 with operation `import` holds every reference
(`HEAD` included) and one pack with every object they reach, and the file
changelog and the file reviews are copied with their sequences and IDs, so
that the cursors of changelog consumers stay valid. It works in shadow and in
authoritative mode.

On a repository that is already imported, the import brings in the history
written into the files since then (shadow mode writes the file changelog and
the file review store only): every file changelog entry with a sequence greater
than the database's, with its sequence, every file review whose ID the database
does not have, and every review that an earlier import copied and that was
changed in the files since then (same creation time, later update time; the
database copy is replaced and gets a new version). A review that the database
changed later than the file is kept. References are not touched, and running it
again imports nothing new. The response reports the current head sequence and the
numbers of entries and reviews imported (no references and no pack), and
`graft storage import` prints `already imported at changeset N: imported K
changelog entries and R reviews written since`. It only reads the local copy
that was imported into that registration (its watermark names the repository
ID); another local repository at the same path is answered with
`AlreadyExists`.

The import is resumable. It registers the repository first (with its changelog
and reviews), then uploads the pack, then commits changeset 1. If it fails in
between, nothing is rolled back: the repository stays registered with head
sequence 0, which every node treats as an import that is not complete (requests
fail with `FailedPrecondition`, writes are refused in authoritative mode, and
the local repository is never touched). `graft storage state` shows such a
repository as tracked with head sequence 0. Run the import again; it resumes
with the same repository ID and commits changeset 1.

A files-backend repository can be imported. In authoritative mode the next
request replaces its local copy with a reftable copy hydrated from the storage
core; the old copy, with its file changelog and file reviews, is deleted. The
replacement waits until the database holds that history: while the file
changelog has entries beyond the database's last sequence, or a file review
created or changed in shadow mode is not in the database yet, requests fail
with `FailedPrecondition` ("the shadow-mode history of this repository is not
in the storage core yet: run graft storage import") and the copy is left
alone. To convert the repository in place
instead, which keeps its files, run `git refs migrate --ref-format=reftable`
on it before switching to authoritative mode.

A repository with the MVCC reference backend cannot be imported.

### State and verify

`graft storage state` shows the mode, whether the repository is tracked, its
ID, the database head sequence, the sequence this node's copy has applied, and
the node ID. It does not catch up, so it shows a node that is behind.

`graft storage verify` compares every local reference with the database and
lists the divergences. With `--connectivity` it also runs `git fsck
--connectivity-only` on the local copy and checks the bucket: it indexes the
pack of every changeset into an empty repository in the storage's temporary
directory (a dry hydration: it downloads every pack of the repository) and
checks that every object the database references reach is there. A failure of
either check is reported as a connectivity error; a bucket problem starts with
`storage core objects:`. In authoritative mode it catches the local copy up
first, so a divergence means a real inconsistency. In shadow mode it compares
git with what the database recorded.

If the bucket check fails (objects of a git write that the storage core did not
record, left out of the packs of the writes on top of it by a version before
the shadow packs excluded the database references), the database state of the
repository cannot be rebuilt on another node. Repair it in shadow mode, with
the writers of the repository stopped: delete the repository's row from
`graft_repositories` (`DELETE FROM graft_repositories WHERE storage = '<storage>'
AND relative_path = '<relative path>'`; its changesets, references, changelog
and reviews go with it) and run `graft storage import` again. The new
registration gets a new repository ID and a full pack, and takes the changelog
and the reviews from the files again, so do this before switching to
authoritative mode.

### Switching from shadow to authoritative

1. Run every node in shadow mode and import every repository
   (`graft storage import`). Writes that commit while an import is running
   (between its read of the references and the commit of changeset 1) are not
   recorded: they are counted in `graft_storage_untracked_writes_total` and
   logged. Stop the writers of a repository while it is imported, or check
   that counter afterwards and run `graft storage verify` on the repositories
   that were written to.
2. Watch `graft_storage_shadow_divergences_total`,
   `graft_storage_shadow_errors_total` and, once every repository is imported,
   `graft_storage_untracked_writes_total`; all must stay at zero. Run `graft
   storage verify --connectivity` on every repository: it must report
   `consistent`. (The connectivity check covers the local copy and the bucket,
   see [State and verify](#state-and-verify). The bucket stays complete even
   after a write the storage core did not record: shadow mode builds each pack
   against the values the database records, not against the local references,
   so the next recorded write of a reference uploads every object its value
   reaches that no recorded value reaches.)
3. Stop the writers and run `graft storage import` once more for every
   repository, on the node whose files hold its shadow-mode history: it brings
   the changelog entries and the reviews written in shadow mode since the import
   into the database (see [Import](#import)).
4. Set `standalone = true` (if Praefect is in use, remove it: authoritative mode
   does not support Praefect) and `mode = "authoritative"` on every node, and
   restart. Each node catches its copies up on first use; files-backend copies
   are replaced by reftable copies, which deletes their file changelog and file
   reviews (they are in the database after step 3).

Shadow mode does not write the database changelog, so without step 3 the
database changelog would reuse the sequences of the file entries written in
shadow mode, and the reviews created in shadow mode would not be served. In
authoritative mode the gate therefore refuses a repository whose local file
changelog has entries beyond the database's last sequence, or whose file review
store has a review created or changed in shadow mode that is not in the
database yet, with `FailedPrecondition` ("the shadow-mode history of this
repository is not in the storage core yet: run graft storage import"), until the
import brought them in. The check runs before the catch-up, on the local copy
as it is (the catch-up may replace a files-backend copy, files included), reads
the last sequence of the file log and the reviews, and is remembered per
repository and process once it passed. The catch-up itself also refuses, with
the same error, to replace a local copy whose file changelog or file reviews
hold such history, also for the requests that do not go through the gate
(`VerifyRepository`, the retries of the land paths), so a forgotten step 3
never deletes that history.

### Switching back (rollback)

Authoritative mode keeps complete git repositories (objects and references) on
every node that served a repository, so the references and objects need no
export:

1. Stop the writers. On the node that will keep serving, run `graft storage
   verify` for every repository: in authoritative mode it catches the copy up
   to the database head.
2. Set `mode = "shadow"` (or `"off"`) on that node and restart it. Without the
   storage core in authoritative mode there is no catch-up between nodes, so
   serve each repository from one node again (or put Praefect back in front of
   the nodes).

The Graft changelog and the reviews are a different matter: after the
rollback they are served from the files of the local copy again, and there is
no export from the database into those files. What the files hold depends on
how the copy came to be:

- **A copy that was never replaced** (a reftable repository imported in shadow
  mode on this node, or a files-backend one converted in place with `git refs
  migrate` before the switch) has the file changelog and the file reviews as
  they were when authoritative mode started. Everything of the authoritative
  period (changelog entries and reviews) is in the database only and is not
  served.
- **A hydrated copy** (every copy on a node other than the one that imported
  the repository, a copy rebuilt after a removal or a repository ID change,
  and every files-backend repository that authoritative mode replaced by a
  reftable copy, also on the importing node) has no file changelog and no
  file reviews at all. After the rollback its changelog is empty and its
  reviews are gone, also those from before authoritative mode; the next write
  gets file changelog sequence 1 and the next review ID 1, so the cursors of
  changelog consumers are invalid and review IDs are reused.

Choose, for every repository, a serving node whose copy was never replaced if
the changelog and the reviews matter, and tell changelog consumers to reset
their cursors otherwise. The database keeps everything of the authoritative
period until the repository's row is deleted.

Switching to authoritative mode again after a rollback does not merge
histories. The database's changelog continues from its own last sequence, and
`graft storage import` brings in only the file entries with a greater sequence,
and the guard compares only the last sequences. When the file log after the
rollback stays at or below the database's last sequence (a hydrated copy that
restarted at 1, typically), the guard passes and the entries written after the
rollback are never imported. When it goes beyond it, the guard refuses until
the import, which then appends only the entries above the database's sequence,
whose sequences have a different history in each log. Reviews follow the import
rule of [Import](#import): a file review whose ID the database already has for
another review (one created in the authoritative period) is skipped, and a
review that both sides have from before the first switch is replaced by the
file's copy when the file's update time is later, which drops the changes the
authoritative period made to it in the database. Before switching back, compare `graft log` with the files,
or re-import the repository from scratch (see
[State and verify](#state-and-verify) for deleting its row).

### Failures

- **Database or bucket unavailable.** Authoritative mode: writes fail with
  `Unavailable` and git aborts them; requests that need a catch-up fail with
  `Unavailable` after `sync_timeout`. Shadow mode: writes proceed, the failure
  is logged and counted.
- **A node dies after uploading a pack and before the database commit.** The
  pack is an orphan: no changeset references it, no node serves it, and the
  write can be retried. Orphan packs are not collected yet (see limits).
- **A node dies after the database commit and before git committed.** The
  database has the write, other nodes serve it, and the node's own copy is
  behind until its next request catches it up. The counter
  `graft_storage_local_commit_failures_total` counts the transactions that git
  aborted after the database commit.
- **A node with an empty disk.** Its first request for a repository hydrates
  it.
- **A repository removed on another node.** The local copy has a watermark
  whose key the database no longer has; it is moved into the storage's
  temporary directory, and the repository is reported missing on that node too.

These behaviours are proved by the end-to-end tests
`internal/graft/e2e/storage_*_test.go`, with several real servers sharing one
database and one bucket, on SQLite and Postgres: writes on one node and reads
on another, concurrent lands on one branch from two and three nodes (no lost
update, every land in the changelog exactly once), a node that joins with an
empty disk, a repository created on one node and removed on another, crash
injection at both fault points, and the benchmark's mixed load in shadow mode
without a single divergence.

### Metrics

| Metric | Description |
|---|---|
| `graft_storage_changesets_total{mode,result}` | Reference transactions handled by the write path; results `committed`, `noop`, `conflict`, `error`, `skipped`. |
| `graft_storage_preparing_seconds` | Time of the write path in the Preparing phase, before git locks the references (pack build and upload). |
| `graft_storage_prepared_seconds` | Time of the write path in the Prepared phase, while git holds the reference locks (expected values and database commit; also the pack when the Preparing phase did not prepare it). |
| `graft_storage_pack_bytes_total` | Bytes of packs uploaded. |
| `graft_storage_shadow_divergences_total` | Shadow mode changesets recorded despite a mismatch. |
| `graft_storage_shadow_errors_total` | Shadow mode failures; the git write proceeded. |
| `graft_storage_untracked_writes_total` | Shadow mode writes of repositories that are not imported. |
| `graft_storage_mvcc_skipped_total` | Shadow mode transactions of MVCC repositories, skipped. Authoritative mode rejects every repository that is not reftable (MVCC included) with `FailedPrecondition` and does not count it here. |
| `graft_storage_local_commit_failures_total` | Transactions committed to the database that git then aborted. |
| `graft_storage_sync_total{result}` | Catch-ups before a request; results `fresh`, `caught_up`, `hydrated`, `error`. |
| `graft_storage_sync_seconds` | Duration of the catch-up before a request. |
| `graft_storage_sync_changesets_total`, `graft_storage_sync_bytes_total` | Changesets applied and pack bytes downloaded by catch-up and hydration. |

## Benchmark

`examples/graft/demo/bench-storage.sh` runs standalone servers built from the
example configuration (batched landing on), identical except for
`[graft.storage]`: off, authoritative with SQLite, and authoritative with
Postgres, each with a file bucket. graft-bench lands 5 changes per writer with
1, 10 and 50 concurrent writers on one branch per run, and the script reports
throughput, latency and the storage core histograms of each run.

One run on 2026-10-15, after the LOCK AND HISTORY RULE of PLAN6.md (the pack is
built and uploaded in the Preparing phase, before git locks the references, and
git waits up to 10 s for the reftable lock, like for `packed-refs`): WSL2 on an
Intel Core Ultra 7 255H (16 threads), with other test suites running on the
same machine (load average 27 to 28 during the whole run, more than twice that
of the earlier run below, so the absolute times are inflated), a local Postgres
in Docker, conflict rate 0, seed 1. "Preparing" is the
`graft_storage_preparing_seconds` histogram (pack build and upload, outside the
lock), "locked" the `graft_storage_prepared_seconds` histogram (expected values
and database commit, while git holds the repository-wide reftable lock), "sync"
the `graft_storage_sync_seconds` histogram (once per catch-up check before a
request), all as the difference of the scrapes before and after the run (the
script's report has the preparing, locked and sync columns; the preparing call
counts equal the voter transactions and are left out of the table below). Every voter transaction of the run went
through the Preparing phase (equal counts), and no server logged a "reference
is already locked" error or a failed Preparing phase.

| run | landed / changes | lands/min | p50 (ms) | p95 (ms) | voter transactions | preparing mean (ms) | locked mean (ms) | syncs | sync mean (ms) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| off, 1 writer | 5 / 5 | 156.7 | 405.4 | 458.7 | 0 | – | – | 0 | – |
| sqlite, 1 writer | 5 / 5 | 65.1 | 568.5 | 2126.4 | 11 | 41.62 | 17.22 | 17 | 0.42 |
| postgres, 1 writer | 5 / 5 | 13.4 | 1158.1 | 11815.2 | 11 | 293.89 | 21.58 | 17 | 1.73 |
| off, 10 writers | 50 / 50 | 227.3 | 2281.1 | 3806.7 | 0 | – | – | 0 | – |
| sqlite, 10 writers | 50 / 50 | 145.7 | 3148.9 | 6092.7 | 84 | 79.38 | 29.65 | 136 | 17.94 |
| postgres, 10 writers | 50 / 50 | 185.4 | 3073.8 | 4458.2 | 83 | 63.20 | 18.74 | 123 | 13.98 |
| off, 50 writers | 250 / 250 | 373.4 | 7291.1 | 10921.9 | 0 | – | – | 0 | – |
| sqlite, 50 writers | 250 / 250 | 308.9 | 8640.2 | 13210.3 | 370 | 45.00 | 17.60 | 625 | 7.61 |
| postgres, 50 writers | 250 / 250 | 319.8 | 8101.3 | 13681.3 | 369 | 52.75 | 16.90 | 566 | 12.54 |

The earlier run on the same day, before the fix (load average about 12; its
"voter mean" was the whole voter run, pack and upload included, under the
lock):

| run | landed / changes | lands/min | p50 (ms) | p95 (ms) | voter transactions | voter mean (ms) | syncs | sync mean (ms) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| off, 1 writer | 5 / 5 | 207.0 | 299.5 | 333.7 | 0 | – | 0 | – |
| sqlite, 1 writer | 5 / 5 | 145.7 | 436.8 | 477.9 | 11 | 41.61 | 17 | 0.36 |
| postgres, 1 writer | 5 / 5 | 113.5 | 542.5 | 612.6 | 11 | 58.04 | 17 | 1.94 |
| off, 10 writers | 50 / 50 | 332.5 | 1673.3 | 2066.3 | 0 | – | 0 | – |
| sqlite, 10 writers | 25 / 50 | 171.8 | 1974.6 | 2412.8 | 43 | 82.07 | 110 | 11.39 |
| postgres, 10 writers | 27 / 50 | 74.0 | 4166.3 | 10786.9 | 48 | 155.69 | 96 | 43.71 |
| off, 50 writers | 250 / 250 | 304.8 | 8186.1 | 14935.5 | 0 | – | 0 | – |
| sqlite, 50 writers | 52 / 250 | 277.5 | 3008.7 | 4253.2 | 69 | 63.79 | 323 | 5.32 |
| postgres, 50 writers | 41 / 250 | 284.3 | 2023.7 | 2623.2 | 56 | 66.67 | 189 | 8.34 |

Lands per minute and latencies are over the landed changes only. What the runs
show:

- **Concurrent writers no longer fail.** Before the fix, 50 to 84% of the
  changes of the concurrent authoritative runs failed in graft-bench's commit
  step (`UserCommitFiles` onto each change's own branch) with
  `FailedPrecondition: reference update: reference is already locked`: reftable
  has one reference lock per repository, the voter held it while it built and
  uploaded the pack and committed to the database, and git waited only its
  default 100 ms for it. Now every change of every run landed (50 of 50 and 250
  of 250 on SQLite and Postgres), with one attempt each. The time under the
  lock is the "locked" column, 17 to 30 ms on average, and the pack work runs
  before the lock is taken.
- **The concurrent runs are now comparable with the off runs**, since they
  land the same changes. Under this load they reach 64 to 82% of the off
  server's lands per minute with 10 writers and 83 to 86% with 50 writers.
- **One writer:** the run had a load average of 27, and its single-writer
  numbers are dominated by it (in the Postgres run the slowest of the five changes
  took 11.8 s, and its Preparing mean is 294 ms).
  The earlier, less loaded run is the better measure of the per-land overhead:
  about 140 ms (SQLite) and 240 ms (Postgres) on a land of about 300 ms. A
  catch-up check that finds the copy fresh costs well under 2 ms.

## Limits

- **Git still needs a local repository.** "Stateless" means that the local
  repository of a node is a cache that the node can rebuild from the database
  and the bucket, not that nodes keep no local state. Every request runs git
  on a local copy, a node's first request for a repository downloads all of its
  packs, and there is no virtual filesystem.
- **No garbage collection of packs yet.** Packs are never deleted: neither the
  orphans of writes that died before the database commit nor the packs of
  removed repositories. There is no checkpoint compaction either: hydration
  replays every changeset from 1. Both are planned after the first release.
- **Praefect is not supported in authoritative mode.** Configuration validation
  requires `[graft] standalone = true` for authoritative mode. Shadow mode
  works with or without Praefect.
- **S3, GCS and Azure are untested here.** The bucket is any gocloud URL, but
  the test suite and the benchmark use `file://` and `mem://` buckets only;
  the S3 tests run only when `GRAFT_TEST_S3_URL` points at a test bucket.
- **File buckets need a filesystem with hard links** (every local Linux or
  macOS filesystem, and NFS): objects are published with link(2), see Packs. On
  a filesystem without hard links every write fails with an error that says so.
- **Concurrent writes to one repository share one lock** in authoritative
  mode: reftable has one reference lock per repository. The voter holds it only
  for the expected values and the database commit (the pack is uploaded before
  the lock is taken), and git waits up to 10 s for it, so concurrent writes
  queue instead of failing; a write that waits longer than 10 s still fails with
  "reference is already locked". See [Benchmark](#benchmark).
- **SQLite serves one node.** SQLite has a single write lock. The writes of
  one gitaly process take it in the order they arrive, but several processes on
  one SQLite file (several nodes on one host) meet in SQLite's own lock wait,
  which keeps no queue: under steady contention an unlucky write can wait past
  the 10 s busy timeout and fail. A deployment with more than one node uses
  Postgres; SQLite is for one node, or for tests.
- **Some mutators are refused in authoritative mode**, see
  [RPCs in authoritative mode](#rpcs-in-authoritative-mode).
- **Switching back from authoritative mode** has no export of the changelog
  and the reviews into the files: those of the authoritative period stay in
  the database only, and a hydrated copy (every copy on the other nodes, and
  every replaced files-backend copy) serves an empty changelog and no reviews,
  whose sequences and IDs then restart at 1, see
  [Switching back](#switching-back-rollback). The switch to authoritative mode
  brings the shadow-mode history in with `graft storage import`. After a
  rollback, the file review store can hand out an ID that a review of the
  authoritative period already has in the database; on the next switch the
  database review is kept and that file review is not imported.
- **No per-directory permissions** in the storage core; access is decided by
  `[[graft.policy]]` as before.

## Clean-room note

The storage core adopts ideas that Anthropic described publicly for its
internal "Scion" system: repository state in a consistent database plus object
storage, and servers that are re-creatable caches (see
[ARCHITECTURE.md](ARCHITECTURE.md)). Scion's internal design and code are not
public and are not reproduced. The implementation was written from the
public description and the existing Gitaly code base only: nothing under the
`ee/` directory of GitLab was read or used, and no code was taken from the
GPL-licensed `git-mvcc` sources in `_build/deps/git-mvcc`. The storage core
uses only `database/sql` with the existing SQLite and Postgres drivers and
gocloud buckets, so it is not tied to one vendor.
