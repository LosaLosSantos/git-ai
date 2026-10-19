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

Until this plan is in, the SDK does not retry rebase lands by default
(`UnavailableRetry.RebaseLands`, commit f13ad58).

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

* A new field of the changelog metadata, `Lands []LandRecord{Original, Landed}`,
  separate from `Members`, carries the identity pairs. It is never cut: a pair is
  under 200 bytes, a batch has at most `max_size` (50) members, so the field is at most
  about 10 KiB. A request whose pairs would not fit the payload budget fails before
  its update rather than landing without its identity (it cannot happen with the
  configured limits; the check makes the bound an invariant).
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
with its newer sequence (`ON CONFLICT ... DO UPDATE` when the new sequence is greater).

Migration `2026102xxxxxxx_graft_land_identities`, in both dialects, with the rule of
milestone 3: a node whose database has a migration it does not know refuses to start
(`unknown migrations`), so an older binary never writes changesets without the rows.
A repository removed by `RemoveRepository` loses its rows by `ON DELETE CASCADE`.

### 4.2 The file changelog (storage core off or shadow)

The record is part of the log entry itself: the entry of the branch update gets the
`lands` field (JSON, omitted when empty), so it is durable with the entry, in the same
write and fsync. A secondary index file next to the log,
`graft/changelog.lands`, makes the lookup cheap:

* JSON lines `{"seq":N,"branch":"...","original":"...","landed":"..."}`, appended
  after the log append, with its own fsync, and a header line recording the log
  sequence it is complete up to.
* It is derived data. A crash between the log append and the index append leaves the
  index behind the log; the first lookup after it reads the log after the index's
  sequence (`Scan` from that cursor) and appends what is missing. A torn last line is
  cut off, as in the log. A missing or corrupt index is rebuilt from the log.
* The lookup loads the index once per process and keeps it in memory, updated by the
  appends of the process and caught up from the log when the log's durable end is past
  the index (another process appended).

## 5. The land path

Before a rebase replays a diverged change (`LandDiverged` with the rebase strategy),
the land looks up `(branch, commit_id)`:

* A hit whose landed commit is reachable from the current tip (one
  `merge-base --is-ancestor`): the change is on the branch. The answer is
  `AlreadyLanded`, `landed_commit_id` the recorded commit, nothing replayed, no
  resolver run, whatever happened on the branch since (a revert included). The answer
  explains itself: its message says "landed as <oid> at changelog <seq>", and adds "it
  was reverted later by <oid>" when a revert of it is on the branch (section 5.1).
* A hit whose landed commit is not reachable (the branch was reset past it): the change
  is no longer on the branch; the land proceeds as a new one.
* No hit: the land proceeds.

The lookup reads the index of the mode (4.1 or 4.2), one indexed read. A batched land
does the same for each member before the batch plans its chain.

### 5.1 The revert in the answer

Only when the answer is already `AlreadyLanded` from the index, the land looks for a
revert of the landed commit among the commits after it: one
`git log --format=%H -n1 --fixed-strings --grep="This reverts commit <landed>" <landed>..<tip>`,
the line that `git revert` ("This reverts commit <id>.") and graft-verify's revert
("This reverts commit <id>, which failed ...") write. The walk covers only the
commits landed after the change, it runs on the rare path of a retried or repeated land,
and its result only adds to the message: a revert written another way is not named, and
the answer is the same.

This changes one behaviour on purpose: a change that landed and was reverted cannot be
landed again from the same commit. The land answers `AlreadyLanded`, because the change
did land and is still in the branch's history. Landing it again needs a new commit (a
revert of the revert, or the change recommitted), which is a new identity. A merge or a
fast-forward land behaves so already today (the reverted `commit_id` is still reachable,
so it answers `AlreadyLanded`); this plan gives a rebase land the same rule. Today a
rebase re-land lands the change again; the documentation of `UserLand` and of
graft-verify's revert says so with this plan.

With this in, the SDK retries rebase lands by default again (`RebaseLands` stays, to
turn it off).

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
  rebase with resolution, merge and fast-forward.
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
* The SDK: `RebaseLands` on by default again; an end-to-end retry after a forced lost
  response.

## 8. Ownership needed

`internal/gitaly/service/operations` (land paths), `internal/git/gitcmd` (the payload
field), `internal/gitaly/hook` (recording), `internal/graft/changelog` (entry field and
index file), `internal/graft/corestore` and `sqlstore` (table, migration, lookup), `sdk`
(the default), docs (LAND-BATCHING, STORAGE, CONFIGURATION).
