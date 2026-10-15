# Praefect Serialized Writes

Praefect coordinates writes to a repository across multiple Gitaly nodes using a
voting protocol driven by the Git `reference-transaction` hook. This document
describes why writes must be serialized at the repository level, how the
serialization is implemented, what upstream Git change it depends on, and how to
turn the feature on.

## Why Serialize Writes

The voting protocol relies on Git's `reference-transaction` hook to vote on a
ref update at each phase of the transaction. The phases Git historically
exposed are:

- `prepared` — references have been locked on disk
- `committed` — updates have been persisted
- `aborted` — the transaction was rolled back

Praefect collects votes from all Gitaly nodes participating in the transaction
and only allows the write to proceed when quorum is reached.

The problem is that at the `prepared` phase, **on-disk ref locks have already
been taken**. With two concurrent transactions T1 and T2 updating overlapping
refs A and B:

1. Node 1: T1 locks A, T2 locks B
1. Node 2: T2 locks A, T1 locks B

Each transaction is now waiting for the other to release a lock, and neither
can release until quorum is reached for its own lock. The cluster is
deadlocked.

The fix is to serialize transactions before any node has taken an on-disk
ref lock. If only one transaction per repository can advance past this point at
a time, lock acquisition order is globally consistent and the deadlock above
cannot occur.

## Requirements

Serialized writes require two upstream changes: one in Git, one in Gitaly's
protocol. That together expose a phase running before any locks are taken.

### Git: the "preparing" phase

The serialization point is a new `reference-transaction` hook phase called
`preparing`, introduced upstream in:

> `60d8c1e97d62c27ef60db0bc3d5deadd6dfdb98d`
> *refs: add 'preparing' phase to the reference-transaction hook*
>
> First released in **Git v2.54.0**.

`preparing` fires before Git stages any ref update. This is the only phase
at which Praefect can take a global lock without already racing with the
on-disk locking machinery.

Gitaly's bundled Git pin is set in `Makefile`:

```makefile
GIT_VERSION_PREV   ?= ...   # default bundled Git
GIT_VERSION_MASTER ?= ...   # Git's master branch tip
```

For serialized writes to work on every code path, `GIT_VERSION_PREV` must point
to a revision that contains `60d8c1e9…`.

### Praefect: `PREPARING_PHASE`

The matching `VoteTransactionRequest_Phase` enum value `PREPARING_PHASE = 4`
was added to `proto/transaction.proto`. The Gitaly-side reference-transaction
hook handler maps the Git `preparing` state to `voting.Preparing`, and the
matching write paths in Gitaly (`update_with_hooks.go`, `repoutil/create.go`,
`repoutil/custom_hooks.go`, `repoutil/remove.go`, `objectpool/disconnect.go`,
`objectpool/fetch.go`, `transaction/voting.go::CommitLockedFile`,
`service/operations/squash.go`, `service/ref/delete_refs.go`) cast a
`voting.Preparing` vote before any ref or file lock is acquired.

## How It Works

`internal/praefect/transactions/manager.go` owns the lock lifecycle. A
repository-scoped write lock backed by PostgreSQL is acquired, renewed, and
released according to the phase of each `VoteTransaction` call:

| Phase            | Action                                                  |
|------------------|---------------------------------------------------------|
| `PREPARING_PHASE`| `WriteLockManager.Lock` — block until exclusive access  |
| `PREPARED_PHASE` | `lock.Renew` — keep the lock alive across phases        |
| `COMMITTED_PHASE`| `lock.Unlock` — release once the write is durable       |

If any phase fails before commit, the deferred
`unlockRepoForTransaction` releases the lock so the next transaction can
proceed. Transactions that are explicitly stopped or canceled
(`StopTransaction`, `cancelTransaction`) also release the lock to avoid
stranding it in PostgreSQL.

The `WriteLockManager` interface lives in
`internal/praefect/datastore/lock_manager.go`. The production implementation is
PostgreSQL-backed; a `NoopWriteLockManager` is provided for callers that don't
need serialization.

### Why PostgreSQL and not an in-process mutex

A `sync.Mutex` (or any per-process construct) only serializes within a single
Praefect. A production cluster typically runs **multiple Praefect instances**
behind a load balancer for availability and horizontal scaling, and votes for
the same transaction can arrive at different instances. With an in-process
lock, two Praefect instances would each happily believe they hold the lock for
the same repository at the same time, which would defeat serialization
entirely.

The lock therefore has to live in a store all Praefect instances share. The
existing PostgreSQL database that Praefect already uses for its metadata is a
natural fit: it gives us cluster-wide visibility, transactional semantics, and
a notification channel for lock release without introducing a new dependency.

### Flow for one write

1. Client calls a write RPC against Praefect.
1. Praefect forwards the RPC to each Gitaly node in the replica set.
1. Each Gitaly node, in `UpdateReference` (or the equivalent write path),
   casts a `Preparing` vote before locking refs locally. The hook RPC
   reaches Praefect, which calls `lockRepoForTransaction(PREPARING_PHASE)`:
   `WriteLockManager.Lock` blocks until the per-repository lock is free.
1. Both nodes return from `Preparing` together (quorum reached).
1. Each Gitaly node locks refs on disk and votes `Prepared`. Praefect renews
   the lock.
1. Each Gitaly node writes the update and votes `Committed`. Praefect releases
   the lock.

Because step 3 is exclusive per repository, no two transactions can be in
steps 4-6 at the same time for the same repo, so on-disk locks are always
taken in a consistent order.

## Lock Manager Internals

This section covers the PostgreSQL-backed `RepoReferenceWriteLockManager` in
`internal/praefect/datastore/lock_manager.go`. It is what you get behind the
`WriteLockManager` interface in production; the `NoopWriteLockManager` in the
same file is used when the feature is disabled or no database is configured.

### Schema

The schema is created by migration
`20260224025459_repo_write_locks`
(`internal/praefect/datastore/migrations/20260224025459_repo_write_locks.go`):

```sql
CREATE TABLE repository_reference_write_locks (
    lock_id        TEXT        PRIMARY KEY,
    holder_txn_id  BIGINT      NOT NULL,
    expired_at     TIMESTAMPTZ NOT NULL
);

CREATE INDEX repository_reference_write_locks_expired_at_idx
    ON repository_reference_write_locks (expired_at);

CREATE FUNCTION notify_on_write_lock_release() RETURNS TRIGGER AS $$
    BEGIN
        PERFORM PG_NOTIFY('repository_reference_write_lock_releases', OLD.lock_id);
        RETURN NULL;
    END;
    $$ LANGUAGE plpgsql;

CREATE TRIGGER notify_on_write_lock_delete
    AFTER DELETE ON repository_reference_write_locks
    FOR EACH ROW
    EXECUTE FUNCTION notify_on_write_lock_release();
```

- `lock_id` is formatted as `virtualStorage|relativePath`
  (see `repoLockID()` in `lock_manager.go`). One row per repository.
- `holder_txn_id` is the Praefect transaction ID currently owning the lock.
- `expired_at` is the lock's deadline. If a holder crashes or never explicitly
  unlocks, another transaction can take the row over once it passes.
- The index on `expired_at` supports the background sweep below.
- The trigger fires `PG_NOTIFY` on every `DELETE` so the listener in any
  Praefect instance can wake any waiter for that `lock_id`.

### Acquire, Renew, Release

The lock TTL (`renewInterval`) is **20 seconds**, hard-coded in
`NewRepoReferenceWriteLockManager`. Every successful acquire or renew pushes
`expired_at` forward by 20 seconds.

**Acquire** — `tryLock` runs a single UPSERT and inspects whether a row came
back:

```sql
INSERT INTO repository_reference_write_locks as locks (lock_id, holder_txn_id, expired_at)
VALUES ($1, $2, NOW() + $3::interval)
ON CONFLICT (lock_id) DO UPDATE
    SET holder_txn_id = EXCLUDED.holder_txn_id,
        expired_at    = EXCLUDED.expired_at
WHERE locks.expired_at < NOW() OR locks.holder_txn_id = $2
RETURNING lock_id, holder_txn_id, expired_at;
```

The `WHERE` clause is the **expiry guard**. Four outcomes:

| Existing row state                          | UPSERT result | Caller observes |
|---------------------------------------------|--------------|-----------------|
| No row                                      | INSERT, returns row | `Acquired`     |
| Held by same `txnID` (re-entry)              | UPDATE, returns row | `Acquired` (idempotent) |
| Held by different `txnID`, **expired**        | UPDATE, returns row | `Acquired` (lock stolen) |
| Held by different `txnID`, **not expired**    | No-op, returns no row | `Acquired=false` → wait |

**How to read this table.** Each row is one possible state of the
lock row in Postgres when `tryLock` runs. Walking through them:

1. **No row.** The `INSERT` succeeds, a row is returned, the caller has
   the lock.
1. **Held by same `txnID` (re-entry).** The `WHERE` clause matches
   (`holder_txn_id = $2`), the `DO UPDATE` fires, a row is returned, the
   caller still has the lock. This is idempotent — same call, same result.
1. **Held by a different `txnID`, expired.** The `WHERE` clause matches
   (`expired_at < NOW()`), the `DO UPDATE` overwrites `holder_txn_id`, a
   row is returned, the caller has taken the lock over from the previous
   holder.
1. **Held by a different `txnID`, not expired.** The `WHERE` clause is
   false, the `DO UPDATE` is skipped, no row is returned, the caller has
   to wait.

The contended caller registers a notification channel before running the
INSERT and waits on it; that ordering closes the race window in which a
release could fire between a failed INSERT and the caller starting to listen.

**Release** — `unlock` deletes the row, conditional on still holding it:

```sql
DELETE FROM repository_reference_write_locks
WHERE lock_id = $1 AND holder_txn_id = $2;
```

The conditional `holder_txn_id = $2` is important: if the lock expired and was
stolen by another transaction, this DELETE is a no-op and the new holder is
not disturbed. If the DELETE is a no-op, the trigger does not fire and no NOTIFY is sent;
waiters remain blocked until the new holder releases the lock or the
background sweep removes the expired row.

`unlock` uses its own 3-second timeout context, not the caller's, because the
lock must be released even if the request context was cancelled (e.g. client
disconnect).

**Renew** — `renew` extends the expiry of a held lock:

```sql
UPDATE repository_reference_write_locks
SET   expired_at = NOW() + $3::interval
WHERE lock_id = $1 AND holder_txn_id = $2
RETURNING expired_at;
```

If the row is gone (already stolen / unlocked), renew returns an error to the
caller. In the phase table above, renew is called at `PREPARED_PHASE` to keep
the lease alive while Git takes its on-disk ref locks.

### Notification fan-out

A single `ResilientListener` (`internal/praefect/datastore/listener.go`) per
Praefect process subscribes to the
`repository_reference_write_lock_releases` channel. When the trigger fires a
NOTIFY, the payload (`lock_id`) reaches `lockReleaseDispatcher.Notified`,
which closes every channel currently registered for that `lock_id` and
removes them from the in-memory waiter map.

The dispatcher also handles listener reconnects: on `Connected()`, it wakes
all current waiters so they retry their UPSERT and re-register. This covers
the case where a NOTIFY was emitted while the listener was momentarily
disconnected and would otherwise have been missed.

### Expired-lock sweep

A background goroutine started in `NewRepoReferenceWriteLockManager` runs:

```sql
DELETE FROM repository_reference_write_locks WHERE expired_at < NOW();
```

every **40 seconds** (`2 * renewInterval`). This is purely a hygiene job:
contending callers can already take expired rows themselves via the UPSERT
above, so the sweep is not on the critical path. Its real purpose is to
make sure the `DELETE` (and therefore the NOTIFY) fires even on locks that
no live caller is currently trying to acquire, so any stragglers from a
crashed Praefect don't linger in the table forever.

The sweep logs `clean up expired repository reference write locks` with a
`count` field showing how many rows it removed.

## Metrics

All metrics are registered on the manager (`Collect` / `Describe` on
`RepoReferenceWriteLockManager`), so they show up automatically alongside
other Praefect metrics.

| Metric                                                            | Type      | Labels                                                       | Meaning                                                                                  |
|-------------------------------------------------------------------|-----------|--------------------------------------------------------------|------------------------------------------------------------------------------------------|
| `gitaly_praefect_repo_reference_write_lock_acquired_total`        | Counter   | `virtual_storage`, `result` (`new_acquisition`/`contended`/`error`) | Outcome of every `tryLock` attempt. `contended` is the signal that callers had to wait. |
| `gitaly_praefect_repo_reference_write_locks_held`                 | Gauge     | `virtual_storage`                                            | Locks currently held by this Praefect instance.                                          |
| `gitaly_praefect_repo_reference_write_lock_hold_duration_seconds` | Histogram | `virtual_storage`                                            | Time from acquire to unlock. The natural SLI for serialization overhead.                 |
| `gitaly_praefect_repo_reference_write_lock_operation_duration_seconds` | Histogram | `virtual_storage`, `operation` (`trylock`/`unlock`/`renew`) | Database round-trip per operation. Spikes here mean the DB, not lock contention.        |
| `gitaly_praefect_lock_release_listener_reconnects_total`          | Counter   | `state`                                                      | LISTEN/NOTIFY listener reconnects. Sustained non-zero rate indicates an unhealthy DB connection. |

On-call rule of thumb:

- A sudden rise in `acquired_total{result="contended"}` without a matching
  rise in `hold_duration_seconds` means many writes are arriving for the
  same repository — that's the feature working as intended.
- A rise in `hold_duration_seconds` means writes are *being held* longer
  than usual; check Gitaly-side ref-update latency, not Praefect.
- `operation_duration_seconds{operation="trylock"}` p99 should sit close to
  a normal database round-trip (single-digit ms on healthy SaaS). Sustained
  growth here is a database problem.
- `lock_release_listener_reconnects_total` should be effectively flat. Any
  steady increase means the LISTEN connection is flapping and waiters are
  relying on the reconnect-wake fallback rather than NOTIFY, which costs
  one extra UPSERT round-trip per release.

## Performance Trade-offs

Serializing writes is not free. By design, this feature trades some
write throughput for correctness — the alternative is the cross-node
ref-lock deadlock described in [Why Serialize Writes](#why-serialize-writes).
Two costs are inherent to the design:

- **Per-write overhead on the uncontended path.** Every write through
  Praefect now performs at least one extra Postgres round-trip (the
  UPSERT at `PREPARING_PHASE`), plus a DELETE at `COMMITTED_PHASE`.
  This applies even when there is no contention at all.
- **Serialization of concurrent writes to the same repository.** Writes
  that previously could run in parallel on different Gitaly nodes now
  run one at a time per repository. This is the entire point of the
  feature, but it means single-repo write throughput is upper-bounded
  by `1 / lock_hold_duration` rather than by the underlying Gitaly
  capacity.

Writes to *different* repositories are unaffected: the lock is keyed by
`virtualStorage|relativePath`, so concurrent writes against distinct
repos never contend.

The expected impact and rollout budgets are being measured under
<https://gitlab.com/gitlab-org/gitaly/-/work_items/7260>. On-call signals
to watch are documented in the [Metrics](#metrics) section above —
`hold_duration_seconds` for serialization tax, `operation_duration_seconds`
for raw DB cost, and `acquired_total{result="contended"}` for how often
callers actually had to wait.

## Enabling It

Serialized writes are gated by the
`praefect_serialized_write` feature flag. It defaults to **disabled** so
existing deployments are not affected.

Turn it on the same way as any other Gitaly feature flag:

- **Via gRPC metadata**: pass the header
  `gitaly-feature-praefect-serialized-write: true` on outgoing RPCs.
- **In Gitaly's own tests**: the test helper at
  `internal/testhelper/testhelper.go` enables the flag on the test context with
  `featureflag.ContextWithFeatureFlag(ctx, featureflag.PraefectSerializedWrite, true)`,
  so every test that derives its context from the helper runs with the feature
  on.
- **In GitLab Rails tests**: GitLab Rails spawns Gitaly and Praefect with
  `GITALY_TESTING_ENABLE_ALL_FEATURE_FLAGS=true` (see
  `spec/support/helpers/gitaly_setup.rb`), which forces every feature flag,
  including this one, to evaluate as enabled regardless of context.
- **In GitLab Rails for production rollout**: register the flag in
  `config/feature_flags/` once it is ready to ship.

## Limitations

Some Gitaly write paths run `git update-ref` or `git fetch` directly. For
example `FetchSourceBranch`, `Replicate`, `ResolveConflicts`,
`UserCommitFiles`, `UserRevert`, `RebaseToRef`, `MergeToRef`, and anything
using `localrepo.UpdateRef` / `localrepo.FetchInternal` /
`setDefaultBranchWithUpdateRef`. On these paths, Git itself drives the
`reference-transaction` hook rather than Gitaly's manual driver in
`update_with_hooks.go`.

On a bundled Git that contains `60d8c1e9…`, Git emits the `preparing` phase
for these paths too and the serialization is complete.

On a bundled Git without that commit, Git only emits `prepared` and
`committed`. Praefect tolerates this by skipping the lock check when no
`PREPARING_PHASE` was recorded for the transaction. The transaction completes
normally, but **serialization is dormant for that write**. The deadlock
described above remains possible on those paths until `GIT_VERSION_PREV` is
bumped past `60d8c1e9…`. Once it is, the tolerance branch becomes unreachable
and can be removed alongside a startup-time minimum Git version check.

## References

- Deadlock issue (root cause):
  <https://gitlab.com/gitlab-org/gitaly/-/issues/7059>
- Performance investigation:
  <https://gitlab.com/gitlab-org/gitaly/-/work_items/7260>
- Upstream Git patch series:
  <https://lore.kernel.org/git/20260317023624.43070-1-eric.peijian@gmail.com/>
- Feature flag definition:
  `internal/featureflag/ff_praefect_serialized_write.go`
- Lock manager: `internal/praefect/datastore/lock_manager.go`
- Phase handling: `internal/praefect/transactions/manager.go`
- Benchmarking framework: `_support/benchmarking/README.md`
