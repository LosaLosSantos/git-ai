# Graft p5w: the write path, root causes first (plan for approval)

Status: approved by the PM with the decisions and amendments of section 14;
WP91 to WP95 implemented, the acceptance measurement (section 11) waits for a
benchmark window, WP96 (stage 2) is gated on it. Owner: p5w (Opus). Branch
`p5w-write-path` (from `6a94caf`, the final phase 4b tree, plus the
committer-date skew fix `04c0776..b6058e5` and its performance fix `6aecff1`).

Phase 4b is in [PLAN8.md](PLAN8.md). Its profile left two costs outside the land
lane, shared with every other write: the quarantine migration fsyncs again files
that are already durable, and every reference update of a repository waits for
the changeset-log record of the updates before it. This plan measures the write
path of `UserCommitFiles` and of the lands, and fixes the causes it finds.

## 1. Goals and acceptance

The assignment has five goals:

* G1 `quarantine.Migrate` against the removal of empty fan-out directories by
  housekeeping (ex WP87): reproduce it deterministically and fix it where it
  belongs.
* G2 The durability contract of `Migrate`: every file of a quarantine is durable
  before the migration, for every writer, and the migration makes the new links
  durable once per directory. State the filesystem assumption and prove it holds on
  ext4 and xfs.
* G3 The write RPCs other than lands, `UserCommitFiles` first (0.3 to 0.86 s per
  request in phase 4b): measure, then fix the cause.
* G4 The changeset-log ordering wait (68 to 127 ms, about 11 to 12 earlier tickets
  in phase 4b): find which invariant needs it and fix the structure, keeping every
  changelog guarantee (dense, ordered log, cursors, `WatchChangesets`, the storage
  core's changeset sequence).
* G5 The pack of a lost batch attempt and the objects of lost attempts: decide with
  evidence whether they are a real problem, and state the guarantee.

Acceptance (section 11) is per RPC: `UserCommitFiles` p50 and p99 at 50 and 100
agents, `acceptance-phase4.sh` not regressing, and durability tests with fault
injection and `fsck`.

## 2. Profiling evidence

### 2.1 Method

* Temporary probes, never committed (saved as patches in `/home/feder/p5w-work`):
  histograms around every step of `UserCommitFiles` (`cf.*`: quarantine, resolve
  the target and parent, fetch a missing commit, receive the actions, write the
  blobs, build the tree and the commit, read the old revision, update), around
  every step of `updateReferenceWithHooks` for both `UserCommitFiles` (`cf.u.*`) and
  lands (`land.u.*`: pre-receive, migrate, update hook, update-ref start,
  preparing, prepare, prepared, commit, committed, post-receive), and around the
  changeset-log record of the reference-transaction hook (`c.ticket_wait`,
  `c.append`). Counters: migrated files, earlier tickets per transaction and how
  many of them share a reference with the transaction.
* The git process metrics of the server (`gitaly_command_real_seconds_total` per
  RPC and sub-command, `gitaly_spawn_forking_time_seconds`).
* Runs: `graft-bench --mode land`, batching on, 5 changes per agent, conflict rate
  0.2, seed 1, at 50 and 100 agents, scrapes before, after and every 5 s. Each
  agent writes its changes with `UserCommitFiles` on a branch of its own and lands
  them. Load 5 to 11 during the runs.
* Experiments B and C (temporary code behind environment variables, never
  committed) to bound the gain of a design before writing it, and an
  authoritative-mode run (SQLite database, `file://` bucket) for the mode matrix.
* A micro-benchmark of the migration (ext4 `data=ordered`, the WSL2 virtual disk
  of this machine): 5 objects in 5 fan-out directories, written and fsynced as git
  writes them, migrated by 1 or 50 goroutines at once, 400 migrations per variant,
  two repetitions.

### 2.2 Where `UserCommitFiles` spends its time (run prof1, means in ms)

| step | 50 agents | 100 agents |
|---|---:|---:|
| whole RPC (gRPC handling) | 416 | 412 |
| update (`updateReferenceWithHooks`) | 262 | 202 |
| of which migrate | 140 | 87 |
| of which committed phase of the reference transaction | 110 | 102 |
| of which prepare (update-ref locks, prepared phase) | 9 | 9 |
| build the tree and the commit (`commit_git`) | 95 | 146 |
| write the blobs | 25 | 23 |
| fetch a missing commit, resolve the target, read the old revision | 19 | 23 |

Files migrated per request: 5.4 at 50 agents, 3.2 at 100 (one blob, its trees, the
commit). The gRPC histogram buckets (0.1, 0.5, 1 s) are too coarse for p50 and
p99; section 11 adds them to `graft-bench` first.

### 2.3 The committed phase is a convoy of serialized fsyncs

| | 50 agents | 100 agents |
|---|---:|---:|
| ticket wait (mean ms) | 98.8 | 91.9 |
| append (mean ms) | 7.15 | 7.23 |
| earlier tickets per transaction | 13.9 | 13.3 |
| earlier tickets that share a reference with the transaction | 0 of 3699 | 0 of 6960 |

The wait is the sum of the appends before it: 13.9 x 7.15 = 99 ms against 98.8
measured, 13.3 x 7.23 = 96 ms against 91.9. Each transaction records with its own
write and fsync, one after the other under the log's lock, and the order makes each
wait for all of them. Not one wait in about 10600 was for a transaction that
updated the same reference.

Experiment B dropped the waits for tickets that share no reference (an upper bound
only: it gives up the cross-reference order, section 3.4). The ticket wait fell to
0.01 ms, but the append rose to 171 ms at 50 agents and 126 ms at 100, and the
committed phase got slower (110 to 174 ms at 50 agents). The convoy moved into
`changelog.Append`, which serializes the fsyncs under its lock. The cost is one
fsync per transaction in series, not the order.

### 2.4 The migration fsyncs every file again, one journal commit each

Experiment B also migrated with one fsync per target directory after all links and
no fsync of the linked files. The migration fell from 140 to 58 ms at 50 agents and
from 87 to 39 ms at 100; for lands from 34 to 13 ms.

The micro-benchmark (mean ms per migration, two repetitions):

| variant | 1 at a time | 50 at once (p99) |
|---|---:|---:|
| today: link, fsync the file, fsync each directory as it is done | 14.6 / 19.6 | 36.0 / 41.4 (93 / 254) |
| fsync the source file before the link, links, each directory once | 6.4 / 9.2 | 17.7 / 20.8 (43 / 94) |
| fsync all source files in parallel, links, each directory once | 7.5 / 7.3 | 16.4 / 17.4 (58 / 44) |
| links, then each directory once | 3.3 / 4.0 | 9.6 / 12.1 (34 / 35) |

A link changes the inode of the file (its link count), so the fsync of the linked
file commits the journal once per file, and each directory fsync done between links
commits it again. The fsync of a file that is already clean still costs about 1 ms
(a cache flush of the device).

### 2.5 The tree and the commit cost one git process per object

Experiment C wrote the objects of `UserCommitFiles` without fsync and fsynced the
quarantine files in parallel before the links. Writing the blobs went from 25 to 17
ms at 50 agents, but from 23 to 32 ms at 100; building the tree and the commit did
not change (95 and 135 ms); the migration got slower (78 and 52 ms against B's 58
and 39). Write-time fsync is not what `UserCommitFiles` pays for.

Git process time per `UserCommitFiles` request at 50 agents (real seconds of the
run divided by 251 requests): `hash-object` 80 ms, `update-ref` 121 ms (with the
hooks and the migration), `rev-parse` 27 ms, `commit-tree` 14 ms, `ls-tree` 5 ms.
`hash-object` runs once per blob and once per tree that changes (`localrepo`
`TreeEntry.Write` writes each tree with its own `hash-object -t tree`): about 4.4
processes per request, about 18 ms each under this load. A fork alone costs 2.5 ms
at 50 agents and 3.2 ms at 100 (`gitaly_spawn_forking_time_seconds`); the rest is
exec, git's start-up and the scheduler under load. The cost of item G3 is the number
of processes, not durability.

### 2.6 The mode matrix (authoritative run, SQLite)

In authoritative mode the reference-transaction hook records nothing in the file
log (the storage core writes the changelog in its database transaction), takes no
ticket and does not wait: no `c.*` observation in the run, and the committed phase
costs 5.1 ms at 50 agents and 2.5 ms at 100. That mode has its own, larger cost,
outside this plan: `UserCommitFiles` takes 1312 and 1258 ms, of which 634 and 721 ms
in the prepare step of update-ref, while the voter's `Prepared` (database commit
under the reference locks) averages 16.5 and 14 ms and its `Preparing` (pack build
and upload) 49.5 and 45 ms. About 570 to 650 ms of the prepare step are not
explained by these histograms. This is the storage core's write path (m3); it is
reported to the PM and not touched here.

### 2.7 The migration race (G1), reproduced

`migrate` creates a target fan-out directory (accepting one that exists) and then
links the object into it. `git prune-packed`, which housekeeping runs after a
repack (and `git repack -d` and `git prune` do the same), removes empty fan-out
directories with `rmdir`. A test that runs `migrate` (256 fan-out directories, one
object each, all target directories present and empty) while a goroutine
`rmdir`s the empty target directories in a loop, as prune-packed does, failed 300
of 300 runs: `finalizing object file: rename ...: no such file or directory` (the
link fails with ENOENT, and so does the rename fallback). git's own writers do not
have the race because they create their temporary object file inside the target
fan-out directory, which pins it. The quarantine is a separate directory, so the
target directory is empty between the `mkdir` and the link. Housekeeping triggers
this concurrently with writes: the housekeeping middleware (every 20th mutator RPC,
asynchronously), the daily optimization, `OptimizeRepository` and object-pool
fetches.

### 2.8 The writers of a quarantine (G2 audit)

Every Gitaly write into a quarantine is a git process started by the command
factory, which sets `core.fsync=objects,derived-metadata,reference` and
`core.fsyncMethod=fsync`: blobs, trees (`hash-object`), commits (`commit-tree`,
`CommitWriter`), tags (`mktag`), merges (`merge-tree --write-tree`), fetches into a
quarantine (`index-pack`, `unpack-objects`), the conflict resolver's results, and
the rebase engine in loose mode. The batch attempts write without fsync
(`WithoutObjectFsync`) and pack every loose object into one fsynced pack, deleting
the loose files, before the update. Quarantines are created by 18 RPCs (operations,
conflicts, repository fetches, diff); `Migrate` is called by
`updateReferenceWithHooks`, `UserSquash`, `UserRebaseToRef`, `FetchSourceBranch`
and `FetchRemote`. Pushes are not involved: `git receive-pack` has its own
quarantine and migrates it itself. Under WAL transactions `Migrate` fsyncs nothing
and the transaction manager owns durability; that does not change.

Two exceptions:

* The administrator's `[git]` configuration is applied last, so an administrator
  who sets `core.fsync` decides for every writer (documented, unchanged).
* Custom hooks run with the quarantine as `GIT_OBJECT_DIRECTORY` but without the
  server's git configuration (`command.AllowedEnvironment`), so objects that a
  custom pre-receive hook writes are fsynced only as the repository's own
  configuration says (git's default leaves loose objects out). See question Q1.

### 2.9 The changeset log lets readers see what is not durable

`changelog.Append` writes the entries with `WriteAt` and fsyncs them afterwards.
`Scan` and `LastSequence` read the file, so they can return an entry between the
two. After a power loss the entry is gone and the next append reuses its sequence.
A reader that saw it continues after it and skips the new entry forever. A
temporary test reproduces it: Append entry 1; write entry 2 as Append does up to
its fsync; `Scan` returns sequence 2; truncate the file to the last fsynced end
(the power loss) and drop the cached state (the restart); the next Append gets
sequence 2 for another reference; `Scan` after cursor 2 returns nothing. The
readers are `ListChangesets` and `WatchChangesets` (through `changelog.FileSource`),
the storage core import (`Scan`), and the storage core gate and syncer
(`LastSequence`, m3's code). This is independent of the ticket order and existed
before phase 4b.

### 2.10 Lost attempts (G5)

prof1 left 4 packs in the repository after both runs. A batch attempt loses its
compare-and-swap only to a writer outside its lane (another node, an unbatched
writer of the same branch), so the single-node acceptance runs had none
(attempts/landed 1.00 in the three windows of phase 4b). The objects of any update
that fails after the migration stay in the repository unreferenced: a lost batch
attempt (one pack), an unbatched land that loses its compare-and-swap (loose
objects), a `UserCommitFiles` whose expected old revision is stale, and a crash
between the migration and update-ref. All of them passed the pre-receive hook
before they were migrated (`updateReferenceWithHooks` migrates only after it).

### 2.11 Root causes

* R1 (G4) One write and one fsync per transaction, in series, in the changeset
  log. The order of the tickets makes every transaction wait for the fsyncs of all
  earlier ones; without the order they queue on the log's lock instead (B).
* R2 (G2) `Migrate` fsyncs every linked file again, and each directory as soon as
  it is done: one journal commit per file and per directory, although the writers
  made the files durable.
* R3 (G1) `Migrate` relies on a fan-out directory that it created or found staying
  in place until its link, which git's housekeeping does not promise: an empty
  fan-out directory may disappear at any time.
* R4 (G3) `UserCommitFiles` starts a git process per object it writes and per
  revision it resolves.
* R5 (separate) Changeset-log readers read past the durable end of the log.

## 3. Design

### 3.1 Group commit of the changeset log (fixes R1)

The order does not change: entries are recorded in the order in which their
transactions reached the prepared phase, for the whole repository, as today. That
keeps the order of the updates of every reference and the order between references
that a later transaction may have read (a writer of B that read A's new value is
recorded after A).

The ticket of a transaction carries its records once its committed phase has run.
The committed phase marks its ticket ready and then:

1. If no group is being written for the repository, it becomes the group writer.
   It takes the longest prefix of the repository's queue whose tickets are ready,
   writes all their entries with one write and one fsync
   (`changelog.AppendGroup`), wakes each member of the group with the result, and
   removes their tickets. It repeats while the head of the queue is ready, then
   stops being the writer.
2. Otherwise it waits until its ticket is done, or until the ticket at the head of
   the queue expires (as today), and then writes as in 1. The wait is detached from
   the request's context: the updates are already committed in git, so their
   entries are always written, in order, within the expiry of the tickets ahead of
   them. (Today a member whose context ends records at once, out of order.)

A ticket that is ready behind one that is not waits for it, as today, but no longer
for each earlier fsync in turn: when the head becomes ready, everything ready
behind it is written in the same fsync. The expected wait is the time until the
earlier transactions commit in git plus one append, instead of the sum of the
earlier appends.

Contract:

* Writer: the committed phase that finds no group being written. It writes only
  ready tickets from the head of the queue, in queue order, so the entries are
  written in ticket order and get consecutive sequences in that order.
* Bound: none on the count or the size of a group. A group holds the ready
  transactions at the head of the queue, which are already in memory (each
  committed phase holds its records) and bounded by the concurrent writes of the
  repository. A cap would only add fsyncs. Each group is one `write` of the encoded
  entries (a few hundred bytes per update).
* No RPC returns before its entry is durable: the committed phase returns only
  after its ticket is done; a ticket is done only after `AppendGroup` returned,
  that is after the write, the fsync of the log and, when needed, of its
  directories; git returns from the transaction only after the hook; the RPC
  returns only after git.
* Failure: if the write or the fsync of a group fails, every member gets the error
  and logs it as the single append does today ("recording changesets failed"), the
  durable end (3.2) does not move, the cached state is reset (rescan on the next
  append, as today), and no member is reported durable. As today, recording never
  fails the reference update. The tickets of the group are released, so later
  transactions proceed.
* Expiry: a head ticket whose transaction never reports its outcome expires after
  `changesetTicketTimeout` (5 s), as today; a waiting ready ticket is woken at the
  deadline of the head and then writes as the group writer.
* Across processes: the flock of the log is kept, taken once per group.
* MVCC reference backend: unchanged. Its entries are recorded after the MVCC commit
  of the request (`AfterCommit`), outside the ticket order, as today (section 13).

Mode matrix:

| mode | tickets | file log | change |
|---|---|---|---|
| storage core off (default) | yes | yes | group commit |
| shadow | yes | yes | group commit; the voter's `Committed` runs after the record, as today, now sooner |
| authoritative | none | not written (the core writes the changelog in its database transaction) | none; measured: no wait, committed phase 2.5 to 5 ms (2.6) |
| `[graft.changelog] disabled = true` | none | none | none |

The hook calls the storage core exactly as today, in the same phases and order. In
shadow mode `Voter.Committed` only calls `NoteCommitted`, which does nothing outside
authoritative mode; in authoritative mode it takes sequences as a set (the syncer's
pending map). Nothing in the voter depends on when the file log is written.

### 3.2 Readers bounded by the durable end (fixes R5)

The state of a log keeps a durable end: the offset after the last entry whose
fsync returned, and its sequence. `AppendGroup` advances it after the fsync, at
the same point where the members of the group are woken, before `WatchChangesets`
is notified. `Scan`, `List` and `LastSequence` read only up to it. When the state
is not loaded (a reader before any append of this process), loading it fsyncs the
file before taking its valid end as durable: after a crash of the process, bytes
written before the crash may still be only in the page cache, and the fsync makes
them durable before a reader sees them. A torn tail beyond the durable end is never
read and is truncated by the next append, as today.

Group commit (3.1) and the readers agree on one boundary: an entry is visible
exactly when its RPC may return.

### 3.3 The migration: links first, one fsync per directory, empty fan-out directories may vanish (fixes R2 and R3)

The contract, stated in `quarantine`: every file in a quarantine is durable when
`Migrate` starts (its data and its inode, made so by its writer); `Migrate` makes
the new names durable and nothing else.

`migrate` becomes two passes:

1. Links, in the order of today (`sortEntries`: `.keep`, `.pack`, `.rev`, `.idx`
   last within a pack directory). For each target directory it opens a descriptor
   of the directory after it exists. A link that fails with ENOENT while its
   source exists means that the target directory was removed after it was created
   or found: `migrate` creates it again, opens it again and links again. This is
   the protocol of git's object directories, not a retry of a failed operation: an
   empty fan-out directory may vanish at any time, and a link into it pins it once
   it succeeds. Each round needs a concurrent removal in the window between the
   `mkdir` and the link, so the loop ends; it also stops when the context ends. A
   link that fails with ENOENT because its source is gone is an error, as today.
2. Durability, only when `storage.NeedsSync` (not under WAL, as today): one fsync of
   each target directory descriptor that received a link, then of the objects
   directory. No fsync of the linked files. A directory that housekeeping removed
   after the link still has its descriptor, whose fsync succeeds; housekeeping
   removes a fan-out directory only once it is empty, which for a migrated object
   means that `prune-packed` removed it because a pack holds it.

Filesystem assumption (local ext4 in any `data=` mode, or xfs; not NFS): a new hard
link is durable once its directory is fsynced (POSIX); a link is one journaled
metadata operation that changes the directory entry and the inode's link count
together; both filesystems commit journal (log) transactions in order, and the
fsync of a directory forces the journal up to the directory's last change. So:

* the content and inode of each file are durable before the link (contract);
* after pass 2 every link is durable, because each directory that received one was
  fsynced after it;
* a crash in between leaves any prefix of the links, in link order: if the `.idx` of
  a pack is durable, its `.pack`, linked earlier, is durable too (same directory,
  earlier or same transaction);
* the first directory fsync of pass 2 commits the journal with all links, so the
  others find their changes committed and return quickly (2.4: 9.6 to 12.1 ms
  against 36 to 41 ms at 50 concurrent migrations).

The rename fallback (when a link is not possible) keeps the same rules: the target
directory is fsynced in pass 2.

### 3.4 `UserCommitFiles`: one git process per object type, not per object (R4, gated stage 2)

Today a request writes each blob with `hash-object --stdin`, each changed tree with
`hash-object -t tree --stdin`, the commit with `commit-tree`, and resolves
revisions with `rev-parse`. Stage 2 writes the objects of a request through
long-lived writers, one per object type (`hash-object -w --stdin-paths`, the
`CommitWriter` of phase 4b generalized to blobs and trees), and resolves revisions
through the request's object info process (`cat-file --batch-check`, cached). It
keeps byte-identical objects (same content, same encoding) and the same errors.
Expected from 2.5: from about 10 processes per request to about 5. Stage 2 is gated
like stage 2 of PLAN8: first an exact count of the processes per request by type
(through `command.count`, no benchmark window), then the decision with the PM.

### 3.5 What does not change

The order of the changeset log and its sequences; the storage core's calls and
phases; the migration under WAL; when `Migrate` runs (after pre-receive, before the
update hook and update-ref); batch publishing (pack mode) and unbatched publishing
(loose mode); the objects a write produces.

## 4. Invariants (each has a test, section 10)

* L1 The changeset log is dense and ordered: sequences are consecutive, assigned in
  ticket order, across groups and across processes.
* L2 No RPC returns before its entries are durable, and no reader returns an entry
  that is not durable.
* L3 A reader with a cursor never skips an entry, including across a crash before
  an fsync (the reproduction of 2.9 becomes a passing test).
* L4 A failed group write reports no member durable and loses no later entry.
* L5 `WatchChangesets` and `ListChangesets` return the same entries, in the same
  order, as before for the same updates.
* M1 After `Migrate` returns (outside WAL), every migrated name is durable, and a
  crash at any point leaves a prefix of the links in link order.
* M2 `Migrate` succeeds while housekeeping removes empty fan-out directories.
* M3 Every writer into a Gitaly quarantine leaves durable files (asserted by the
  existing `/proc` argument test for the helpers and by a writer inventory test).
* S1 The storage core sees the same calls in the same order in every mode.

## 5. Failure and race cases

| case | behaviour |
|---|---|
| group write or fsync fails | every member logs the error; durable end unchanged; tickets released; updates already committed in git (as today) |
| crash before a group's fsync | its entries are lost and were never visible; sequences are not reused by a reader that saw them, because none did |
| crash after the fsync, before the members return | entries durable and visible; RPCs fail with the process |
| head ticket never reports (process died) | expires after 5 s as today; the waiting ready tickets write then |
| context of a waiting member ends | its RPC fails as cancelled, as today; the hook keeps waiting, detached and bounded by the tickets' expiry, and writes its entries in order (they are committed in git) |
| a member's committed phase never runs | its ticket expires, as today |
| housekeeping removes a fan-out directory between `mkdir` and link | created again, linked again (M2) |
| housekeeping removes a fan-out directory after the link | its descriptor is fsynced; the objects are in a pack |
| crash during pass 1 | any prefix of the links; the quarantine is removed; unreferenced objects at most, as today |
| crash during pass 2 | links durable up to the last committed journal transaction, in order |
| reader before the first append of the process | loads the state, fsyncs the file, reads up to its valid end |

## 6. Config and API changes (exact)

No configuration key and no default changes. Code:

```go
// package changelog
// Record is the updates of one transaction with their metadata.
type Record struct {
	Meta    Meta
	Updates []Update
}

// AppendGroup records the records in order with one write and one fsync, and returns their
// entries. On an error no entry is durable or visible.
func AppendGroup(repoPath string, records []Record) ([][]Entry, error)

// Append(repoPath, meta, updates) stays and calls AppendGroup with one record.
```

The ticket queue of `internal/gitaly/hook/changeset_order.go` gains the records and
the group writer; `recordChangesets` calls it instead of `changelog.Append`. In
`quarantine`, `migrate` changes as in 3.3; no API change. `graft-bench` gains per-RPC
latency (count, p50, p95, p99 of `UserCommitFiles` and of `UserLand`) in its JSON
and Markdown output.

## 7. Metrics

* `graft_changelog_group_entries` (histogram): transactions per group write.
* `graft_changelog_append_seconds` (histogram): time of a group write with its
  fsync.
* `graft_changeset_ticket_wait_seconds` (histogram): time from the committed phase
  to its entry being durable.

## 8. Work packages

* WP91 (Opus): `graft-bench` per-RPC latency; then the baseline of section 11 on
  `6a94caf` plus the skew fix, in the first window (needed before any number of
  this plan can be judged).
* WP92 (Opus): `changelog.AppendGroup`, the durable end and the readers; the
  reproduction of 2.9 as a test; crash tests.
* WP93 (Opus): group commit in the ticket queue; tests of L1 to L5 and S1.
* WP94 (Opus): `migrate` in two passes with directory descriptors and the ENOENT
  protocol; the deterministic race test and the stress test with `git
  prune-packed`; the durability tests of M1.
* WP95 (Opus + Haiku draft): the writer inventory test of M3 and the docs
  (LAND-BATCHING, LIMITATIONS, and CONFIGURATION if a line changes; STORAGE.md is
  m3's).
* WP96 (gated, stage 2): `UserCommitFiles` process count, then the decision on 3.4.

## 9. Ownership needed

p5w owns `internal/git/quarantine/**`. This plan also changes files that are not in
p5w's list, and asks the PM to grant them for these changes only:
`internal/graft/changelog/log.go` and its tests (AppendGroup, the durable end, the
readers), `internal/gitaly/hook/changeset_order.go` and the `recordChangesets` part
of `internal/gitaly/hook/referencetransaction.go` (group commit), and
`cmd/graft-bench` (per-RPC latency). The storage core's readers of the log
(`LastSequence` in the gate and the syncer) need no change: the bound is inside
`LastSequence`; m3 should know that it returns the durable sequence.

## 10. Test plan

* `changelog`: `AppendGroup` gives consecutive sequences in record order, one write
  and one fsync (counted through a test hook on the file), entries identical to
  those of `Append`; a fault hook between the write and the fsync: no reader sees
  the entries before the fsync; the crash test of 2.9 (truncate to the durable end,
  drop the state, append, read after the cursor: the new entry is returned); a
  failed fsync (fault hook): no member durable, the next append rescans and
  succeeds; concurrent appenders and readers under `-race`; the existing fuzz test.
* `hook`: group commit with transactions committing out of their ticket order
  (deterministic gates on the committed phases): the log is in ticket order, groups
  have the expected members, each committed phase returns only after its group's
  fsync; expiry of a head ticket; a member whose context ends (its entries are
  still written, in order); packed-refs cleanups; the existing ticket tests, except
  the case of `TestReferenceTransactionHook_changesetOrder` that expects the wait to
  give up when the caller's context is cancelled, which changes with the rule of
  3.1.
* `quarantine`: the race of 2.7 made deterministic with a test hook between the
  `mkdir` and the link (the directory is removed there): the migration succeeds,
  every object is linked; a stress test running `git prune-packed` in a loop on the
  target repository during 200 migrations; the order of pack files kept; fsync
  calls counted through a test hook: one per directory that received a link plus
  the objects directory, none per file, none under WAL; the rename fallback.
* Durability with fault injection: a crash between the passes and inside pass 2,
  simulated by stopping the migration at a fault point in a child process killed
  with SIGKILL, then `git fsck --connectivity-only` and the expected prefix of the
  links. A true power cut cannot be simulated here; the assumption of 3.3 is stated,
  not tested.
* Writers (M3): every git command that the command factory starts on a quarantine
  carries `core.fsync` with `objects`, or is one of the listed exceptions
  (`WithoutObjectFsync` helpers, which are packed before the update); checked
  through the existing `/proc` argument test and a table of the writers of 2.8.
* `operations`, `e2e`: every existing test; `TestPhase4*`, `TestChaosLanding`
  (fsck after the chaos), `TestStorage*` in shadow and authoritative mode.
* `-race` on `internal/graft/changelog/...`, `internal/gitaly/hook/...`,
  `internal/git/quarantine/...` and the land and `UserCommitFiles` tests.

## 11. Acceptance of this plan

Measured in a PM window with `graft-bench` per-RPC latency (WP91), batching on, the
profile's settings (50 and 100 agents), against the baseline of WP91 on the same
machine and window:

* `UserCommitFiles` p50 and p99 at 50 and 100 agents: lower than the baseline by at
  least the migration and the ticket wait that 2.3 and 2.4 measure. The targets are
  set from the baseline of WP91, as mean minus 150 ms at 50 agents and minus 100 ms
  at 100 (2.2: migrate 140 and 87 ms, ticket wait 99 and 92 ms, of which B kept 58
  and 39 ms, and one append is left); p99 must not be higher than the baseline.
* The committed phase of a `UserCommitFiles` transaction (probe-free metric of
  section 7): mean at most 20 ms at 50 and 100 agents.
* `acceptance-phase4.sh` twice: A1, A2, A3 and S pass at 20, 50 and 100 agents, and
  T (lands per minute) does not drop below the phase 4b runs of record.
* All tests of section 10 green, `go vet`, `gofmt -l` clean, no probe left.

Predicted, not promised: `UserCommitFiles` around 250 ms mean at 50 agents
(416 - 82 for the migration - about 85 for the wait); the next cost is the tree
and commit build (95 to 146 ms), stage 2.

## 12. Rejected alternatives

* Waiting only for tickets that share a reference: removes the wait in B, but the
  appends then queue on the log's lock (171 ms) and the committed phase gets
  slower; it also drops the order between references that a later writer read.
* Assigning the sequence at ticket time and writing the entries as transactions
  commit: entry N+1 could become visible before N, and a reader at cursor N+1
  would skip N.
* Writing a quarantine without fsync and fsyncing it in `Migrate` (experiment C):
  slower migration, and `UserCommitFiles` does not get faster (2.5).
* A defensive fsync of every file in `Migrate` before its link: 6 to 8 ms more per
  migration at 50 concurrent (2.4), for writers that already fsync; see Q1 for the
  only writer that does not.
* Recreating the directory on ENOENT without the descriptor: the fsync of pass 2
  would then meet ENOENT for directories that housekeeping removed after the link,
  and would need a special case.
* A lock between housekeeping and writes: housekeeping runs git commands that
  follow git's protocol (empty fan-out directories may be removed); a writer that
  follows the same protocol needs no lock, and a lock would serialize every write
  of a repository with its repacks.
* `syncfs` once per migration: flushes the whole filesystem, the writes of every
  other repository included.
* Migrating after update-ref has locked and checked the references, so that a lost
  compare-and-swap migrates nothing (G5): update-ref checks that the new objects
  exist when it prepares, and `updateReferenceWithHooks` migrates first for that
  reason, as git's receive-pack does. The cost of today's order is unreferenced
  objects that passed the hooks; see 2.10 and Q3.

## 13. Risks and open questions

* Q1 Custom hooks write into a quarantine without the server's fsync
  configuration. Options: (a) pass the server's `core.fsync` and
  `core.fsyncMethod` to custom hooks (`GIT_CONFIG_COUNT`/`KEY`/`VALUE`), so that
  their git writes keep the contract (recommended: the contract holds at the
  writer); (b) a parallel fsync of every file in `Migrate` when the repository has
  custom hooks (2.4: about 6 ms at 50 concurrent). A hook that writes objects other
  than through git stays outside the contract either way, documented.
* Q2 Ownership of section 9.
* Q3 G5: the guarantee is "objects that passed the pre-receive hook may stay in the
  repository unreferenced when the update fails afterwards, until housekeeping
  prunes them; they are readable only by object ID". They are not a correctness
  problem: no reference points to them, and they passed the same hooks as a
  successful update. Space: at most one pack per lost batch attempt, which needs a
  writer outside the lane. Proposed: keep, document in LIMITATIONS.md, and count
  the unreferenced packs after each acceptance run (`packcount.sh`) to confirm.
* MVCC: the entries of an MVCC transaction are recorded after its commit, outside
  the ticket order (the ticket is released when the hook returns). That is the
  behaviour of today and is kept; the MVCC backend is experimental.
* Authoritative mode (2.6): about 600 ms per `UserCommitFiles` in the prepare step
  of update-ref, unexplained by the voter's histograms. Reported to the PM for m3.
* `UserSquash` (seen during the audit, not p5w's code): when the merge fails with
  an error that is not a conflict, the error is dropped and the RPC votes and
  migrates with an empty commit ID (`squash.go`, after `s.merge`). Reported to the
  PM, not changed here.
* `cmdCfg.worktreePath` stays: this plan does not touch the command factory.
