# Derived data

Status: implemented in phase 3; phase 4a changed the commit-graph default and
added the use of the `graph` kind for ancestry (`[graft.land] graph_ancestry`).
This page describes `internal/graft/derived` (the framework, the warmers and the
kinds `graph` and `changed-paths`), `internal/graft/eager` (the work after
committed reference updates) and the `[graft.derived]` keys in
[CONFIGURATION.md](CONFIGURATION.md#graftderived). The designs are in
[PLAN3.md](PLAN3.md), section B, and [PLAN4.md](PLAN4.md) (WP36, WP38).

Derived data is a value computed from immutable commits that is expensive to
recompute and cheap to store. Values are keyed by commit ID, so a stored value is
never stale and never needs invalidation.

## Storage

Values live in `<git dir>/graft/derived/`, one file per key. Writes are atomic
(temporary file and rename), so a reader sees either no entry or a complete one.
A key is the hash of the kind name, the kind version and the commit ID. The
version is part of the key: a kind that changes its encoding or its result bumps
its version, and the entries of the old version are never read again. Deleting a
repository deletes its values.

## Kinds

| Kind | Version | Value | Computed | Eager |
|---|---|---|---|---|
| `lastmod` and `blame` | phase 1 | last commit that changed a path, and the blame of a file at that commit | on first read, by `LastCommitForPath`, `Blame` and `PathHistory` | no |
| `graph` | 1 | generation number and first-parent skip pointers | on first query, or eagerly | yes |
| `changed-paths` | 1 | paths changed against the first parent | eagerly; `PathHistory` only reads stored summaries (`changedpaths.Index` queries, which no RPC uses, compute missing ones) | yes |

The phase 1 kinds keep their code paths and their output. Only `graph` and
`changed-paths` have a registered warmer, so only they are computed eagerly.

### `graph`

The value of a commit is:

```
uint32  generation, little-endian (a root has generation 1)
uint8   n, the number of skip pointers
n raw object IDs, the skip pointers in order
```

The generation of a commit is one more than the highest generation of its
parents. Skip pointer `k` is the commit `2^k` first-parent steps back. The
package `internal/graft/derived/graph` provides `Index` with `Generation`,
`IsAncestor`, `MergeBases` and `MergeBase`. The answers equal those of
`git merge-base --is-ancestor` and `git merge-base --all`. The walks do not
recurse, so the depth of a history does not limit them. An `Index` keeps up to
100,000 entries in memory, in an LRU cache.

`UserLand` uses the `graph` kind only when `[graft.land] graph_ancestry` is true
(single and batched lands, see [CONFIGURATION.md](CONFIGURATION.md#graftland)).
With the default `false` it asks Git for ancestry. Both return the same answers.
The graph checker refuses repositories with shallow commits, grafts or replace
refs, and the land then uses Git. With `[graft.derived] disabled = true` the
checker still answers, but it computes its entries in memory for each landing
attempt and writes nothing under `graft/derived/`. No other RPC reads `graph`
yet.

### `changed-paths`

The value of a commit is a summary of the files that the commit changes against
its first parent. A root commit is compared with the empty tree, so every file it
holds is changed. A summary holds:

- the exact sorted list of changed files, up to 20,000 of them. A commit that
  changes more files is stored as an overflow. An overflow holds no paths, and a
  query is answered with a diff restricted to the path;
- a Bloom filter over every directory prefix and every path, sized at 16 bits per
  key with four hash functions (about 0.2% false positives). A negative answer is
  always correct, so the filter only skips commits.

The changed files of a path include the files below a directory. A commit that
changes `a/b/c` counts as changed for `a`, `a/b` and `a/b/c`.

`PathHistory` (`DerivedDataService.PathHistory`) always answers with the existing
walk, the stored `lastmod-firstparent` chain, so a repeated query costs about one
store read per commit it returns. When `[graft.derived] disabled` is false, the
stored summaries are a pre-filter of that walk
(`(*derived.Deriver).PathHistoryFiltered` with `(*changedpaths.Index).KnownUnchanged`):
a commit whose stored summary proves the path unchanged is skipped without reading
the path's trees. Every other commit, including one without a stored summary and
one whose path the index does not answer, is compared as the existing walk compares
it. A query only reads summaries; it never computes or stores one. Summaries come
from eager computation (see below), so with `eager` off the filter finds none and
the query costs what the existing walk costs. The commits reported are the same, in
the same order. Equivalence tests compare the two on linear, merge, delete and
re-add, rename and directory histories, with no, some and all summaries stored.

`(*changedpaths.Index).PathHistory` answers a history from the summaries alone. It
computes and stores the summaries that are missing, at most 1,024 per query, and
diffs only the path for the commits beyond that bound. The service does not use it.

## Framework

- `Store.Kind(name, version)` returns a `KindStore` with `Get(commit)` and
  `Put(commit, value)`. Kind names are lowercase letters and hyphens.
- `RegisterWarmer(name, fn)` registers the eager warmer of a kind. It is called
  from `init()` of the kind's package. It panics on a duplicate, an invalid name
  or a nil function. `Warmers()` lists the registered names in sorted order.
  Registered today: `changed-paths` (`internal/graft/derived/changedpaths`) and
  `graph` (`internal/graft/derived/graph`).
- A warmer receives a `WarmEnv` with a logger, the repository, an object reader
  from the catfile cache and the store, and computes and stores the values of the
  commits it is given. A warmer skips the values that are already stored.

## Eager computation

Eager computation is off by default. It is turned on with `[graft.derived]
eager = true`, and it never runs when `disabled = true`.

When a reference update is committed, the reference-transaction hook calls the
eager observer. The hook does this in the committed state only, so a rejected or
rolled-back update triggers nothing, and it does so whether or not the changelog
records the update. The observer queues the transaction for its listing workers,
without blocking, and a listing worker:

1. For each reference the transaction updated, lists the commits that the update
   made reachable with `git rev-list --max-count=1000 <new> ^<old>`. A reference
   created from nothing lists the history of `<new>` up to the same limit. A
   deletion lists nothing.
2. Removes the duplicates and offers the commits of the transaction to the warm
   pool as one batch, without blocking.

The listings are bounded like the warmers: `workers` listing workers run at most
that many `git rev-list` processes at once, and their queue holds `queue_size`
transactions. When it is full, the transaction is dropped and counted in
`graft_eager_dropped_listings_total`, and its commits are computed on the first
read.

The pool has `workers` background workers (default `2`) and a queue of
`queue_size` slots (default `10000`). A slot holds one batch, that is the commits
of one committed transaction, so the queue is measured in batches, not commits.
When the queue is full the batch is dropped and counted. A dropped commit is
computed on the first read, as without eager computation. A batch that is not
dropped is computed by every registered warmer.

Listing and warming never block the update, and a panic is recovered and logged.

### Commit-graph writes

Commit-graph writes are separate from eager computation and are opt-in since
phase 4a: they run only when `[graft.derived] commit_graph = true` and
`disabled` is not true, whatever the value of `eager`. With the default
(`commit_graph = false`) a server with no `[graft]` section starts no commit-graph
writer, and no commit-graph is written outside housekeeping. When enabled, after
committed updates each repository gets an incremental `commit-graph` write, made
with the housekeeping code. The writes are debounced:
a write runs after 5 seconds without a new update, and at the latest 30 seconds
after the first update of a burst. A repository has at most one write at a time,
and at most two writes run at once across repositories. A deletion of a reference
schedules a write, because it changes reachability.

The writes go through the housekeeping manager, so they never run at the same
time as housekeeping (`OptimizeRepository`) of the repository: a write that finds
housekeeping running is postponed by the debounce period and retried, up to 60
times in a row, and housekeeping that starts during a write waits for it before
optimizing. A write that finds the commit-graph lock held by another Git process
is retried the same way.

The writes are not limited to standalone servers. When `commit_graph = true`, any
Gitaly with a reference transaction hook runs them after committed updates,
including a Gitaly that is not standalone. Phase 3 wrote commit-graphs by
default; to keep that behaviour set `commit_graph = true` explicitly.
Graph-based ancestry (`graph_ancestry`) reads the `graph` kind, not the
commit-graph file, so it does not depend on this switch.

## Metrics

The server registers the pool with the Prometheus registry, so its four metrics
are exported on `/metrics` under the fixed names:

- `graft_derived_warm_queue_depth` (gauge): queued batches.
- `graft_derived_warm_dropped_total` (counter): commits dropped because the queue
  was full or the pool was closed.
- `graft_derived_warm_computed_total` (counter): commit and warmer pairs computed
  without error. Each warmer counts once per commit, so with the two warmers
  (`graph` and `changed-paths`) it is twice the number of commits.
- `graft_derived_warm_errors_total` (counter): failed warmer runs, and batches
  whose warm environment could not be opened. A batch that fails in one warmer
  still runs the others.

The server also registers the eager observer, which exports
`graft_eager_dropped_listings_total` (counter): committed transactions whose new
commits were not listed because the listing queue was full.

## Limits

- Eager computation uses CPU and disk on the Gitaly node while lands run. Lower
  `workers` on busy nodes, or leave `eager` off.
- Dropped commits are computed lazily. A full queue slows reads down; it never
  fails a land.
- `lastmod` and `blame` are computed on first read only. They do not benefit from
  eager computation yet.
- Entries of commits that become unreachable are kept until the repository is
  deleted.
