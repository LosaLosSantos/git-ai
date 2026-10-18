# Land batching

Status: implemented in phase 3 (standalone mode only), extended in phase 4a
(batch latency: a streaming rebase engine, early completion, adaptive batch
sizes and a batch of one as an ordinary land) and in phase 4b (one range query
per request and the objects of a batch made durable once, as one pack; see
[PLAN8.md](PLAN8.md)). This page describes the code in
`internal/gitaly/service/operations/land_batch*.go`, `internal/graft/landlane`,
`internal/graft/rebase` and `internal/graft/ancestry`. The designs are in
[PLAN3.md](PLAN3.md), section A, and [PLAN4.md](PLAN4.md).

## Why

A land is a compare-and-swap (CAS) of a branch from its old tip to a new one.
When many agents land on one branch, most CAS attempts lose to another writer,
the change is rebased again and the attempt is repeated. In the phase 2
benchmark (20 agents, one branch) a change needed about 10 attempts, p99 latency
was 26 s, and changes failed at `max_attempts = 20`.

Batching moves the contention from the branch to an in-memory queue in the
Gitaly process. Several changes are then published with one reference update.

## When a land is batched

A `UserLand` request lands through its lane only if all of these hold:

- `[graft] standalone = true`. Batching is not used with GitLab.
- `[graft.land.batch] disabled` is not `true`.
- The request is not on an MVCC snapshot (`repository` reference backend).
- The request has no own git object directory and no alternate object
  directories. Such a request lands alone, because the batch quarantine cannot
  see the objects that only its directories hold.
- The branch exists. A request that creates a branch lands alone.

Any other request runs the unbatched loop, which retries the CAS up to its own
`max_attempts`. Unbatched responses report `batch_size = 1` and
`batch_position = 1`.

## Lanes

A lane is one `(storage, relative path, branch)` in one Gitaly process, held by
`landlane.Registry`:

- Requests enter the lane in FIFO order.
- A lane runs one batch at a time. Lanes of different branches run in parallel.
- A batch is formed when `window` has passed since the first queued request, or
  as soon as `max_size` requests are queued, whichever comes first. A batch that
  waited already starts at once, so with `window = "0s"` a batch is whatever
  queued while the previous batch ran.
- A request whose context is done before its batch starts is removed from the
  queue, and its caller gets `Canceled`.
- A request cancelled while its batch runs still lands. Its caller gets
  `Canceled` at once, and the outcome is discarded. The lane context carries the
  values of the first request of the batch, never its cancellation, and per
  request identity is read from each request's own context.

## What a batch does

One batch attempt:

1. Opens one quarantine on the repository and reads the branch tip.
2. Lands each request in order onto the running tip. For each request:
   - The commit ID is resolved. An invalid ID fails that request with
     `InvalidArgument`.
   - The standalone access policy is evaluated for the request's actor on the
     update the request would make (see *Policy* below). A denied request fails
     alone with `PermissionDenied` and an access-check error.
   - If the commit is already reachable from the running tip, the request
     reports `already_landed` and changes nothing.
   - If the running tip is an ancestor of the commit, the commit becomes the new
     running tip (fast-forward).
   - Otherwise the commit is rebased onto the running tip, or merged for
     `STRATEGY_MERGE`. `STRATEGY_FF_ONLY` fails that request with a
     `not_ancestor` error.
   - A merge conflict fails only that request with `merge_conflict`. When the
     request sets `resolve_conflicts` and the resolver is enabled, the conflict is
     resolved first (see [RESOLVE.md](RESOLVE.md)).
   - A request whose outcome is final before the CAS (see *Early completion*)
     is answered at once.
3. Publishes the final running tip with one CAS from the original tip. The
   changelog entry of that update has operation `land_batch` and one member per
   landed request (see *Changeset log*). A batch with exactly one landed member
   is published as a plain `land` instead (see *A batch of one*).

A batch that changes nothing publishes nothing. Its requests report their own
outcomes.

### How a request is decided and rebased

Since phase 4b ([PLAN8.md](PLAN8.md)), each request of an attempt costs one
short git process, and one more when it diverges from the tip:

1. The commit ID is peeled to a commit by an object info process of the
   attempt's quarantine (`git cat-file --batch-check`), taken from the catfile
   cache for each peel, so a process that dies fails the request that met it
   and at most the next one.
2. One listing of the commits that the change adds to the running tip
   (`ancestry.Checker.List`, `git rev-list --parents commit ^tip`) decides the
   landing (`rebase.Plan`): the change is already landed when the list is
   empty; it fast-forwards the tip when the tip is a parent of a listed commit;
   otherwise it diverges. Both facts are exact even when committer dates are
   skewed. A skewed history can make `git rev-list` list commits that the tip
   already has (its walk stops early when the commits it still has to visit are
   older than those it listed), never fewer. So a diverging change has its
   listing made exact first (`revrange.Refine`): one
   `git merge-base --is-ancestor` per bottom of the listing (a listed commit
   none of whose parents is listed), usually one. If a bottom is reachable from
   the tip, the listing held extra commits: it is listed again below the exact
   merge bases (`git merge-base --all`), and the bottoms that the tip reaches
   are removed one check at a time (counted by
   `graft_revrange_skewed_listings_total` and
   `graft_revrange_skew_checks_total`). For the rebase strategy, a merge commit
   in the exact range refuses the change (`InvalidArgument`, use
   `STRATEGY_MERGE`), a range that ends at a root commit means unrelated
   history (`InvalidArgument`), and otherwise its commits, oldest first, are
   the commits to replay.
3. The commits are replayed by the `rebase.Engine` of the attempt, which keeps
   one `git merge-tree --stdin` process and one `git hash-object --stdin-paths`
   process running for the whole attempt. The replay has the rules, errors and
   conflict contents of the repository's `Rebase` with reapplied cherry-picks.
   If the commit writer cannot write a commit (`ErrCommitWriterUnsupported`, for
   example when signing is configured), that commit is written with the
   ordinary commit writer of the repository.

With `[graft.land] graph_ancestry = true` the listing walks the stored `graph`
index in process instead of running `git rev-list`; both return the same
commits. The graph checker refuses repositories with shallow commits, grafts or
replace refs, because git walks their history differently; for those it logs a
warning and uses `git`. A land that is not batched is decided, rebased and
published the same way, by the same code (`landingAttempt`): it only differs in
its update, which runs with the request's own identity and hooks.

### Durability of the objects of a batch

The merge-tree and hash-object processes of an attempt write their objects into
the quarantine of the attempt without fsync. Before the compare-and-swap, the
attempt packs every loose object of its quarantine into one pack with
`git pack-objects`, which writes and fsyncs the pack and its index, and removes
the loose files and their fan-out directories. The quarantine migration of the
update then moves the pack files into the repository, fsyncing them again,
before the reference changes. So every object that the new tip reaches is on
disk before the branch points to it, with a fixed number of fsyncs per batch
instead of about three per object. A batch that only fast-forwards writes no
object and packs nothing.

If packing fails, the update of the attempt fails: the requests that changed the
running tip fail with the error, as for any failed update (see *Outcomes of the
CAS*), and the branch does not move. Each published batch adds one pack to the
repository, like a push; housekeeping repacks them as it repacks the packs of
pushes.

A land that is not batched writes the few objects of its change with the server's
fsync configuration and publishes them as loose objects, without a pack: a pack
would cost a process and as many fsyncs for so few objects, and under contention
every attempt that loses the compare-and-swap would leave a pack in the repository.
In both cases every object file of the quarantine is durable before the quarantine
migration moves it.

### Hooks and identity of the combined update

The combined update is made as the synthetic user `graft-batch` (name `Graft`,
email `graft@localhost`), with a context that carries no request identity. The
hooks judge the update as that user, and the changeset records the update with
the actor `graft-batch`. The members carry the real actors.

Policy is therefore checked per request, in process, before the request joins
the running tip. For a combined update that carries members, both the access
check of the pre-receive hook (in standalone mode, the policy) and the policy
check of the reference-transaction hook are skipped, because the members were
checked already. Only an update with `operation = land_batch` and members is
skipped. Any other update is still checked, and custom server hooks still run
for the combined update.

A `[[graft.policy]]` rule whose `actor` matches `graft-batch` therefore has no
effect on batches with two or more members (a batch of one is judged as its
requester's land): it neither rejects a batch nor makes its requests land
alone. Restrict who may land on a branch with rules for the agents themselves,
which apply to every request of a batch. To keep a branch out of batching
altogether, the only switch is `[graft.land.batch] disabled`, which applies to
the whole server.

### Outcomes of the CAS

When the combined update is not published, the requests that changed the
running tip (the *members*) did not reach the branch. A request after the first
member was landed onto a tip that holds members, so its outcome does not hold
for the branch either: it may be "already landed" or "unchanged" only because of
a member's commits, or it may have conflicted with a member only. Such a request
never reports the outcome of that attempt. The requests before the first member
landed onto the real tip and keep their outcomes.

- **Success.** Every request of the batch gets its response.
- **Lost to an external writer.** The branch moved during the attempt. The batch
  is rebuilt on the new tip: every request is landed again, and the attempt is
  counted. A batch is rebuilt at most 10 times (11 attempts). The batch
  does not use the request's `max_attempts`. After the last rebuild, every
  request from the first member on fails with the same `reference_update` error
  as an unbatched land that runs out of attempts. The requests before the first
  member still succeed (already landed or unchanged), with their batch fields
  set. Rebuilds wait with the backoff of unbatched lands.
- **Rejected by a hook.** A custom server hook, for example a custom pre-receive
  hook, rejected the combined update. The policy does not reject it: its access
  checks are skipped for the combined update (see above), so a policy rule for
  `graft-batch` cannot cause this outcome. The rejection
  cannot be attributed to one request from the combined update, so every request
  is landed alone through the unbatched loop, with its own identity and its own
  `max_attempts`. Each response then reports `batch_size = 1`.
- **Other failures.** The members fail with the error of the update, which a
  rebuild does not fix. The requests before the first member keep their outcomes,
  and their batch fields still describe the batch as submitted (see
  *Responses*). The other requests after the first
  member are landed again in a new attempt on the real tip, without the failed
  members, and report the outcome of that attempt (its attempt number included).
  Every such attempt drops at least one member, so this ends.
- **Shutdown.** A running batch is cancelled when the graceful shutdown period of
  the server has elapsed, and its requests fail.

## Early completion

A request whose outcome is final before the CAS is answered as soon as it is
decided, not when the batch is published. An outcome may be answered early only
if no other request of the batch can change it. There are two kinds:

Member-independent outcomes, final wherever the request is in the batch:

- a policy denial. It depends only on the actor and the reference;
- an invalid commit ID. It does not depend on the tip.

Tip-dependent outcomes, final only while no earlier request of the batch has
changed the running tip, that is, while the running tip is still the tip the
batch started from:

- an already landed answer, when the commit is reachable from the starting tip;
- an unresolved merge conflict;
- an `InvalidArgument` that depends on the tip, for example a range with merge
  commits or unrelated history.

Once an earlier request has changed the running tip, these requests are not
answered early. They stay in the batch and are decided again by the later
attempts, rebuilds and the fallback, exactly as before early completion existed.

The stepper hands the result to the lane (`landlane.CompleteEarly`). The caller
returns with its response, with the batch fields as for every other request of
the batch, while the batch is still running. The executor's later result
for that request is ignored. A request that is completed early is not a member
of the update, and it does not wait for a slow CAS or for hooks.

Requests that land on the running tip still wait for the CAS. If the batch is
rebuilt or fails, those requests keep the outcome rules of *Outcomes of the
CAS*; early answers are not revised.

## A batch of one

When exactly one request changes the branch in a batch, the update is not a
`land_batch`. It is made as an ordinary `land` with the real identity of that
request, so the policy and the pre-receive access check apply to it as they do
for a land that is not batched. The changelog entry has operation `land` and
carries that single member (`original_commit` → `commit`), so the mapping from
the requested commit to the landed commit is kept. The reference-transaction
policy skip applies only to `land_batch` updates with members, so this `land` is
policy-checked like any unbatched land. Responses still report the batch fields,
as for every batch: `batch_size` is the number of requests of the batch, and
`batch_position` the position of the request in it. Failed and early-answered
requests are counted too, so a batch of one that shares its batch with
conflicting requests reports a `batch_size` greater than 1, although only one
request landed.

## Adaptive sizing

Each lane has an effective maximum batch size, which starts at `max_size`. After
each batch the lane compares the batch duration with `target_latency`:

- A batch slower than `target_latency` halves the effective maximum (never
  below 1).
- A batch faster than half of `target_latency` raises it by a quarter of its
  value (at least 1), never above `max_size`.

A slow publish (for example a slow hook or a large repository) therefore gets
smaller batches, and a fast lane grows back to `max_size`. `target_latency =
"0s"` turns the adaptation off, and the lane always uses `max_size`. The default
target is 500 ms.

A lane is retired as soon as its queue is empty, but its key (storage,
repository and branch) keeps the effective maximum for 5 minutes
(`landlane.KeyStateTTL`) after that, so the next lane of the key starts from
it. A key without a lane for 5 minutes is forgotten: its next lane starts at
`max_size` again, and the memory it used is released. The sum of the effective
maxima of the keys that have a lane or were used in the last 5 minutes is
exported as `graft_land_lane_effective_max_size`. A key that grows back to
`max_size` keeps counting with `max_size` until it is forgotten, and the gauge
falls to 0 once no key has been used for 5 minutes.

## Responses

Every request gets its own response. Batch fields:

| Field | Meaning |
|---|---|
| `batch_size` | Number of requests of the batch as submitted. Requests that landed, were already landed, failed or were answered early are all counted. Field 9 of `UserLandResponse`. |
| `batch_position` | 1-based position of the request in the batch, in submission order, counting every request of the batch (failed ones included). Field 10. |
| `attempts` | Batch attempts used, the first one included: 1 plus the rebuilds. |

Also set on batched responses: `landed_commit_id`, `old_tip`, `rewritten`,
`already_landed`, `resolved_paths` and `resolver`, as for an unbatched land.

A change that landed alone reports `batch_size = 1` and `batch_position = 1`.

The client libraries expose the batch:

- The SDK sets `LandResult.Batch` to `sdk.BatchInfo{Size, Position}`.
  `BatchInfo.Batched()` reports `Size > 1`. A server without the batch fields
  yields the zero value.
- `graft land` prints `landed at <commit> (batch 3/17, 1 attempt)` when the
  change shared its batch: the position of the change in the batch, the batch
  size, and the attempts the land used (`2 attempts` after a rebuild). When the
  server reports no attempts the note is `(batch 3/17)`. A change that landed
  alone prints no batch note.
- `LandResult` in `internal/graft/workspace` carries the same fields as
  `BatchSize`, `BatchPosition` and `Attempts`.

## Changeset log

A combined update writes one changeset with operation `land_batch` and the
actor `graft-batch`. A batch of one is an operation `land` with its real actor
and one member (see *A batch of one*). Its `members` list has one entry per
landed request, with the keys of `changelog.Member`:

| Key | Meaning |
|---|---|
| `original_commit` | The commit the request asked to land. |
| `commit` | The commit it landed as inside the batch. Equal to `original_commit` when it was not rewritten. |
| `actor` | The agent name of the request, or else its GitLab user ID. |
| `rewritten` | True if the change was rebased or merged. |
| `resolved_paths` | Files the resolver resolved for this change. |

The encoded members are limited to 32 KiB, because they travel in the
environment of the git processes of the update. Beyond that the resolved paths
are dropped first. If the list is still too long it is cut to the longest prefix
that fits. The changeset then has `members_truncated: true`. Changesets of
unbatched lands have no members and operation `land`. A batch of one has
operation `land` and its single member.

`graft log` prints `batch of N`, then one `actor original→landed` line per
member. `graft log --json` writes the members in the same form as the log.
Control characters in member actors and resolved paths are escaped in the text
output (for example `\x1b`), because they come from clients and from file
names; `--json` writes them as JSON escapes.

`graft-superfan` judges every member of a `land_batch` changeset as a land of
its own: it checks each member's actor against `expected_actors`, counts each
member toward the `burst` rule, and fetches the details of each member's
commit for the file rules. `graft-batch` is not judged as an actor. A batch
whose member list was cut gets a `batch-incomplete` finding. See
[SUPERFAN.md](SUPERFAN.md#batched-lands).

## Configuration

```toml
[graft.land.batch]
disabled = false          # true: every request lands alone, as in phase 2
window = "10ms"           # collect time after the first queued request; "0s" = no wait
max_size = 50             # largest number of requests in one batch
target_latency = "500ms"  # batch duration the lane aims for; "0s" = no adaptation

[graft.land]
graph_ancestry = false    # true: ancestry from the stored graph index
```

The keys are documented in [CONFIGURATION.md](CONFIGURATION.md#graftlandbatch)
and [CONFIGURATION.md](CONFIGURATION.md#graftland). `window` and
`target_latency` must be non-negative Go durations. `max_size` must not be
negative; zero means the default.

## Metrics

`internal/graft/landlane` defines these metrics, the names of which are fixed:

- `graft_land_lane_queue_depth` (gauge): queued requests, summed over lanes.
- `graft_land_batch_size` (histogram): requests per executed batch.
- `graft_land_lane_wait_seconds` (histogram): time from submission to the start
  of the execution, per request.
- `graft_land_lane_effective_max_size` (gauge): effective maximum batch size,
  summed over the keys that have a lane or were used in the last 5 minutes (see
  *Adaptive sizing*).

The batch executor (`internal/gitaly/service/operations`) defines:

- `graft_land_batch_phase_seconds{phase}` (histogram): time per phase of a batch
  attempt. The phases are `quarantine` (opening the quarantine and the processes
  of the attempt), `tip` (reading the branch tip), `land_member` (one
  observation per request the stepper lands, in every attempt, denied,
  conflicting and already-landed requests included), `plan` and `replay` (parts
  of `land_member`: the range listing and decision of a request, and the replay
  of a rebased request), `pack` (packing the objects of an attempt before its
  update), `update` (the CAS and its hooks) and `total` (one observation per
  execution of the batch, which covers all of its attempts, rebuilds and
  fallbacks).
- `graft_land_batch_packed_objects_total` (counter): objects that attempts
  packed before their update.
- `graft_land_batch_members_total{outcome}` (counter): requests by outcome, with
  the outcomes `landed`, `already_landed`, `conflict`, `denied` and `error`.

The server registers the lanes of the process (`landlane.Shared`) and these
metrics with the Prometheus registry, so they are exported on `/metrics`. The
phase breakdown of a benchmark run is in [BENCH.md](BENCH.md#batching-benchmark).

## Policy

The standalone policy ([CONFIGURATION.md](CONFIGURATION.md#graftpolicy)) decides
each request on the update that request would make, with the same actor the
pre-receive hook would use for that request alone. A denied request never enters
the running tip, so it cannot delay or block the other requests of its batch.

## Measured results

### Phase 4b

The runs of `examples/graft/demo/acceptance-phase4.sh` on the phase 4b trees
(three benchmark windows; full tables, phases and the machine in
[BENCH.md](BENCH.md#measured-phase-4b-results)) **meet the phase 4a acceptance**,
the stretch goal included: one attempt per landed change, only planned conflicts
fail, and p99 with batching on is below off at every size. The final tree
(`9bf4742`, window 3):

| p99 landed (ms), run 1 / run 2 | 20 agents | 50 agents | 100 agents |
|---|---|---|---|
| batching on | 819 / 748 | 1880 / 9171 | 4640 / 5390 |
| batching off | 4707 / 4813 | 8267 / 17868 | 14641 / 26692 |
| lands/min, batching on | 1366 / 1476 | 1690 / 620 | 1459 / 1678 |

The stretch goal (p99 on at 100 agents at most half of off) passed by 37% and 60%
(in window 1 by 3.7% and 51%). The runs vary a lot, and the benchmark saturates the
machine; the 50 agents of window 3, run 2, are an outlier on both sides.
A batched request now costs about 8 ms (one range listing and a replay without
fsync per object, see *How a request is decided and rebased*), and most of a batch
is its publish: the pack of its objects and the update.

### Phase 4a

The phase 4a acceptance run of record (`examples/graft/demo/acceptance-phase4.sh`,
20, 50 and 100 agents of 5 changes, conflict rate 0.2, seed 1; full tables, the
other runs and the machine in [BENCH.md](BENCH.md#measured-phase-4a-results))
does **not** meet the phase 4a acceptance: attempts and failures pass at every
size, but p99 with batching on is above off at 50 and 100 agents. An earlier run,
before the final fixes, passed narrowly (by 76 ms at 50 agents); two runs of the
final tree failed at 50 and 100 agents.

| metric | off-20 | on-20 | off-50 | on-50 | off-100 | on-100 |
|---|---|---|---|---|---|---|
| attempts/landed | 11.39 | 1.00 | 27.94 | 1.00 | 56.94 | 1.00 |
| landed / failed | 62 / 38 | 78 / 22 | 99 / 151 | 200 / 50 | 115 / 385 | 392 / 108 |
| p50 landed (ms) | 1518 | 1989 | 2097 | 8959 | 4073 | 17292 |
| p99 landed (ms) | 10023 | 2789 | 9939 | 12658 | 13368 | 23253 |

With batching on, every failure is a planned conflict; with batching off, most
failures are lands that gave up after 20 attempts, and they are not in the off
latencies. In phase 3 the p99 values with batching on were 9868, 15317 and
21808 ms.

The phase breakdown shows where a batch spends its time: landing the members
(`land_member`, about 80 to 115 ms per member with the rebase engine, one after
another) takes 56 to 72% of it, and the reference update with its hooks
(`update`) 27 to 42%. The lane is busy for almost the whole run, so with many
agents most of the latency is time spent queued behind earlier batches. Turning
adaptive sizing off does not change that. The profile of phase 4b found where the
per-member time went (fsyncs of loose objects and nine short git processes per
member) and removed it; see [PLAN8.md](PLAN8.md).

## Limits

- Batching is per Gitaly process. Two nodes that accept lands for the same
  branch still race on the reference, as before.
- The lane lives in memory. Requests queued when a Gitaly process stops are not
  landed. When the lane registry is closed, queued requests fail with
  `Unavailable`, and callers retry.
- A land that arrives alone waits up to `window` before its batch runs. Set
  `window = "0s"` to remove the wait, or `disabled = true` to remove batching.
- Requests that do not qualify (see *When a land is batched*) run unbatched and
  get the contention behaviour of phase 2.
