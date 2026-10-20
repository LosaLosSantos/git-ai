# PLAN11: land identity (design, approved 2026-10-19)

## 1. The problem

The response of a `UserLand` call can be lost after the server landed the change: a
connection reset, a node that stops, a client deadline. The client then retries, as
the `Unavailable` contract asks (LAND-BATCHING, Limits). The retry must land nothing
twice and must report what happened.

* With a merge or a fast-forward, the change itself (`commit_id`) is on the branch: the
  retry finds it reachable from the tip and answers `AlreadyLanded`.
* With a rebase (with or without `resolve_conflicts`), the branch holds a rebased copy.
  `commit_id` is not reachable from the tip, so the server cannot tell that the change
  landed. The retry replays the change; every commit comes out empty and is dropped,
  and the branch is left unchanged, but the answer is not `AlreadyLanded` and its
  `landed_commit_id` is the tip, not the copy. When a later commit reverted the change
  in between (graft-verify reverting the culprit), the replay is not empty: the retry
  lands the change again.

Until this plan was in, the SDK did not retry rebase lands by default
(commit f13ad58).

Federico's decision (2026-10-19): the server keeps an index of what landed, by the
identity of the request, with no trailer in the commits.

## 2. The identity of a land

A land request is identified on its branch by its source commit, `commit_id`: an agent
lands one change from one commit, and a later land of the same `commit_id` on the same
branch is the same change. The record of a land is:

`(branch, original commit) -> (changelog sequence, landed commit)`

The landed commit is the commit on the branch that holds the change: the rebased copy
of `commit_id`, the merge commit of a merged or resolved land, or `commit_id` itself
for a fast-forward. Only lands that rewrote the change need the index (a reachable
`commit_id` answers already), but every land is recorded, so that the record does not
depend on the strategy.

## 3. Recording it on every land path

Today the changelog records `Members` (with `OriginalCommit` and `Commit`) for batched
lands and for the unbatched lands of a lane (`UpdateSolo`), not for the classic
`landWithRetries` path, and `Members` are capped at 32 KiB in the hook payload, which
can drop members of a large batch.

* A new field of the changelog metadata, `Lands []Land{Original, Landed}` with the
  branch they landed on (`LandReference`), separate from `Members`, carries the
  identity pairs; only the entry of that branch's update carries them. It is never
  cut. The branch name travels in the hooks payload as bytes (base64 in JSON), as
  `Entry` and `Member` keep their names: a JSON string would replace the bytes of a
  name that is not valid UTF-8 (review M2). `changelog.WithLands` validates the
  identities (a branch, full object IDs, at most 100 pairs, at most 24 KiB encoded)
  and a land whose identities do not pass fails before its update rather than landing
  without its identity. A SHA-256 pair encodes in 156 bytes and `max_size` is now at
  most 100, so a full batch and a branch name of 4096 bytes (5464 in base64) fit. A
  request on a branch whose name is too long for a full batch (above about 6700
  bytes) is refused with `InvalidArgument` when it arrives, alone, before any batch.
* Every land path sets it: the batch update, `UpdateSolo` and `landWithRetries`
  (through `updateLandedReference`, which knows `commit_id` and the new tip).
* The MVCC path records after its commit (`AfterCommit`), with the same metadata.

## 4. The index, in each storage mode

The index lives where the changelog of the mode lives, so that the record and its
changeset are one durable unit.

### 4.1 Authoritative (and shadow when the database is the changelog's home)

A table, written in the changeset's transaction (`CommitChangeset`, which already writes
the changelog entries of the update):

```
graft_land_identities(
  repo_id, branch, original_commit,
  seq, landed_commit,
  PRIMARY KEY (repo_id, branch, original_commit))
```

A land of an identity that has a row and whose landed commit is still on the branch
does not land (section 5), so it writes no row. One whose landed commit is no longer on
the branch (the branch was reset past it) lands as a new change and replaces the row
with its newer sequence (`ON CONFLICT ... DO UPDATE`: every writer appends after the
last entry, so the last write is the latest land). The rows are written by every
insert of changelog entries: a changeset, the initial history of a registered
repository and the history that shadow mode imports, so a repository moved to the
authoritative mode keeps the identities of its file changelog.

Migration `20261020000000_graft_land_identities`, in both dialects, with the rule of
milestone 3: a node whose database has a migration it does not know refuses to start
(`unknown migrations`), so an older binary never writes changesets without the rows
once it restarts. An older binary that is still running when the new one migrates the
database keeps writing changesets without rows until it stops; the upgrade rule of the
storage core (stop every node, upgrade, start) covers it.
A repository removed by `RemoveRepository` loses its rows by `ON DELETE CASCADE`.

### 4.2 The file changelog (storage core off or shadow)

The record is part of the log entry itself: the entry of the branch update gets the
`lands` field (JSON, omitted when empty), so it is durable with the entry, in the same
write and fsync. A secondary index file next to the log,
`graft/changelog.lands`, makes the lookup cheap:

* The file is a sequence of blocks of JSON lines: the records
  `{"seq":N,"ref":"<base64>","original":"...","landed":"..."}` of the lands of some
  log entries, then a mark `{"mark":N,"ref":"<base64>","new_oid":"...","crc":C}`
  naming the last log entry the block covers and the CRC-32 of its records. The index
  is complete up to its last mark.
* It is derived data, written by the lookups, not by the appends: a lookup reads the
  durable log entries after the last mark (`Scan` from it), adds their lands to the
  index and appends them as one block. An append therefore never waits for the index,
  and a crash between the log append and the index leaves nothing to repair: the
  next lookup reads those entries.
* Reading the file stops at the first line that is not a record or a mark, at a mark
  whose CRC does not match its records, and at a mark that does not follow the one
  before; the file is cut there and caught up from the log. A torn tail, a lost or
  corrupt file are handled so. The mark proves that the index describes this log:
  the lookup reads the log entry at the mark first, and an index whose mark names
  another entry, or one past the end of the log (the repository was created again at
  the same path), is rebuilt from the whole log. The file is written without fsync,
  since whatever a crash leaves of it is either a good prefix or cut off.
* The lookup keeps the index in memory and reads the file again when another process
  changed it. The index remembers the log file it was read from (device and inode): a
  log that is another file (the repository was created again) rebuilds it, and a
  lookup whose log ends with the entry of the mark, in the same file, reads nothing.
  The idle indexes are kept within 64 and 256 MiB together, least recently used
  dropped first; an index larger than the budget is dropped when its lookup ends.
* An index file whose records are mostly superseded by later ones (the same commit
  landed again after a reset) is compacted from 1 MiB: rewritten as one block and
  renamed over the old file.

#### Intents: the window between git's commit and the changelog (review M1)

The entry of an update is written in the committed phase of the reference transaction,
after git committed the references. A crash, a full disk or a lock that times out in
between would leave a landed branch without its identity, and a retry would replay the
change (and land it again if it was reverted). The hook therefore records the identities
of a land as intents in the prepared phase, before git may commit:

* `graft/changelog.intents`, JSON lines
  `{"ref":"<base64>","original":"...","landed":"...","invocation":"..."}`, appended
  under its lock file and fsync'd before the prepared phase returns, with the directory
  entries when the write created the file or is the first of the process to this file
  (device and inode: a repository created again at the same path has another file). The
  invocation identifies the update (the invocation of its reference transaction); the
  changelog entry that the update writes once committed carries it too
  (`land_invocation`). A failure to record the intents aborts the transaction: the
  land fails instead of landing without its identity. A torn line, left by a writer that
  crashed before its fsync and whose update was therefore never committed, is cut off by
  the next writer.
* The lookup reads the intents too: it gives the land the changelog recorded first,
  then the intents of other landed commits. No intent is left out because a record came
  after it (review m1: an earlier update's entry can be written after a later update's
  intent, and the later entry lost). The land path counts an intent like a record, only
  when its landed commit is reachable from the tip, which proves the land by itself: the
  commit is the one that update would put on the branch, and only an update that puts
  that very object there makes it reachable. The update of an aborted land migrated its
  objects before it was prepared, so its commit exists, off the branch, and answers
  nothing (review m2); so does the intent of an MVCC snapshot that lost its commit. An
  answer from an intent has no changelog sequence; its detail says "landed as <oid>,
  which the changelog does not record".
* An intent goes once the changelog entry of its own update is durable: the index keeps
  the invocations of the entries it read, and a compaction drops the intents with those
  invocations, and only those. The intents file is compacted, under its lock file, from
  64 KiB on, when at least half of its intents may go: the rest is written to a new file,
  fsync'd and renamed over the old one, with the directory fsync'd. The lookups compact,
  and so does the append: once the file doubled since the last compaction, the hook
  starts one after the update, outside the references' locks, so that the intents of
  lands that never look up (fast-forwards only) go too. The intents of updates that were
  never committed stay; there is one line, about 200 bytes, per such update.
* The cost is one write and one fsync per land update in the prepared phase, under the
  references' locks, serialized per repository by the intents' lock file, not shared
  with the changelog's group commit.

#### Measured cost (bench window, 50 agents, rebase, batching on)

* Intents (`graft_land_intents_seconds`): one per batch update, mean 9.4 ms (the same
  class as the changelog append's own fsync), 2.6% of the batch time (range 1.3–3.0%),
  about 0.4 ms per landed change at a batch size of about 21.
* The lookup (phase `identity` of `graft_land_batch_phase_seconds`), per diverged
  request: file changelog mean 0.87 ms, authoritative mode mean 0.23 ms (p99 under 5 ms).
  In the file changelog the first lookup of each batch catches the index up (its lock
  file, one block, the batch's new entry and intents) and the others take the fast path
  with no I/O; under the load of the window that first lookup costs 5–25 ms (4.6% of the
  lookups, one per batch). A later step, if it ever shows in profiles: catch the index up
  in the background after the committed append, so that the first lookup of a batch takes
  the fast path too.
* The database rows: one upsert per changeset, +0.1 ms on SQLite and +1.0 ms on Postgres
  for 10 identities (`BenchmarkCommitChangesetLands`).
* Fixed at the cause by the window: an unconditional cut of the intents file (a journal
  commit under the locks), the rescan of up to 256 log entries per lookup (the scan now
  starts at the mark's line), and one database round trip per identity.

In the authoritative mode the database row and the reference change are one transaction,
so there is no such window and no intents.

## 5. The land path

Before a land computes a diverged change (`LandDiverged`, whatever the strategy: a
rebase replays it, a merge or a resolution merges it, `STRATEGY_FF_ONLY` fails), the
land looks up `(branch, commit_id)`. A merge commit, of a merge or of a resolution,
has `commit_id` as a parent, so its retry already finds `commit_id` reachable; the
lookup answers the rebased lands, and makes the answer the same for every strategy:

* A hit whose landed commit is reachable from the current tip (one
  `merge-base --is-ancestor`): the change is on the branch. The answer is
  `AlreadyLanded`, `landed_commit_id` the recorded commit, nothing replayed, no
  resolver run, whatever happened on the branch since (a revert included). The answer
  explains itself: `UserLandResponse` gains `landed_sequence`, `reverted_by_commit_id`
  and `already_landed_detail`, which says "landed as <oid> at changelog <seq>", and
  adds "; it was reverted later by <oid>" when a revert of it is on the branch
  (section 5.1). `branch_update.commit_id` stays the tip.
* A hit whose landed commit is not reachable (the branch was reset past it): the change
  is no longer on the branch; the land proceeds as a new one.
* No hit: the land proceeds.

The lookup reads the index of the mode (4.1 or 4.2), one indexed read. A batched land
does the same for each member before the batch plans its chain.

### 5.1 The revert in the answer

Only when the answer is already `AlreadyLanded` from the index, the land looks for a
revert of the landed commit among the commits after it, with one bounded
`git log --max-count=1000 <landed>..<tip>`: the newest commit among the 1000 closest to
the tip whose message holds "This reverts commit <landed>", the line that `git revert`
("This reverts commit <id>.") and graft-verify's revert ("This reverts commit <id>,
which failed ...") write. The bound keeps a long history after the change from slowing
the answer; it runs only on the rare path of a retried or repeated land.

The revert only shapes the message, never the decision: the answer is `AlreadyLanded`
before it is looked for, and is the same whatever the search finds. A revert written
another way, or further than 1000 commits from the tip, is not named; a search that
fails is logged at Error and the answer names no revert.

This changes one behaviour on purpose: a change that landed and was reverted cannot be
landed again from the same commit. The land answers `AlreadyLanded`, because the change
did land and is still in the branch's history. Landing it again needs a new commit (a
revert of the revert, or the change recommitted), which is a new identity. A merge or a
fast-forward land behaves so already today (the reverted `commit_id` is still reachable,
so it answers `AlreadyLanded`); this plan gives a rebase land the same rule. Before
it, a rebase re-land landed the change again; the documentation of `UserLand`
(LIMITATIONS, LAND-BATCHING) and of graft-verify's revert (VERIFY) says so.

With this in, the SDK retries rebase lands by default again;
`UnavailableRetry.SkipRebaseLands` turns it off (it replaces `RebaseLands`, which was
never released: a field whose zero value is the default cannot turn a default off).
`LandResult` reports `LandedSequence`, `RevertedByCommitID` and
`AlreadyLandedDetail`.

## 6. Option: `Unavailable` that is always safe to retry (m2)

Some `Unavailable` answers are given before the land wrote anything: the storage core
could not sync before the attempt (`EnsureFresh`), the lane was closed before the
request was taken. The server can mark them with error metadata
(`graft.not_landed = true`); the SDK then retries them whatever the strategy, even
without the index. It is an addition, not a substitute: a lost response after the
update is still unmarked, and only the index answers it. Proposed as a separate step
after this plan.

## 7. Tests

* Lost response, then retry: `AlreadyLanded` with the rebased copy, for rebase,
  rebase with resolution, merge and fast-forward. (Built: a resolution lands a merge
  commit, so its retry answers by reachability, as a merge does.)
* Lost response, a revert of the change on the branch, retry: `AlreadyLanded`, the
  revert stays, nothing replayed (the case of 1).
* A batch: every member recorded, a retried member answers `AlreadyLanded`.
* A multi-commit change: the identity is `commit_id`, the tip of the change.
* Both storage modes: the table in authoritative mode, the log field and the index file
  otherwise; the classic path, a lane's solo land and a batch.
* A crash between the log append and the index append (file mode): the next lookup
  catches up and answers `AlreadyLanded`. In the database the row and the changeset are
  one transaction (a test that fails the row insert fails the changeset).
* The payload bound: a batch of `max_size` members with the longest branch name fits.
* The SDK: rebase lands retried by default again; an end-to-end retry after a forced
  lost response.
* Review M1/M2 and minors: a land whose changelog entry is lost answers from its intent;
  the intent of an uncommitted update answers nothing; a branch whose name is not valid
  UTF-8 keeps its identity through the hooks; two concurrent calls of the same land
  land it once (unbatched and batched); a branch name too long for the identity is
  refused when the request arrives; intents and index compaction; a replaced log file;
  least recently used eviction.

### 7.1 Two calls of the same land at once

A retry can reach the server while the first call still runs. Unbatched, the call that
loses the compare-and-swap retries, and its next attempt finds the other call's land (its
intent is recorded before git commits): it answers `AlreadyLanded`. Batched, a request
whose commit an earlier member of the same batch landed answers `AlreadyLanded` with that
member's commit when the batch's update holds; when it does not, the request comes after
a member and is landed again from the real tip, as every such request is.

On repositories with the MVCC reference backend, a land detects a concurrent writer only
when its snapshot is committed, after its handler returned, and it then fails after one
attempt with `FailedPrecondition` and a `reference_update` detail: nothing of that call
landed. The SDK does not retry it (it retries `Unavailable` only). Landing again is safe:
if the concurrent writer was the same change, the new call answers `AlreadyLanded` from
its intent or its record. Mapping this conflict to a retry in the SDK is left to a
separate decision, since it widens the codes the SDK retries.

## 8. Ownership needed

`internal/gitaly/service/operations` (land paths), `internal/git/gitcmd` (the payload
field), `internal/gitaly/hook` (recording), `internal/graft/changelog` (entry field and
index file), `internal/graft/corestore` and `sqlstore` (table, migration, lookup), `sdk`
(the default), docs (LAND-BATCHING, STORAGE, CONFIGURATION).
