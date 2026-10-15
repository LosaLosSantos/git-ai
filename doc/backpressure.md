# Request limiting in Gitaly

In the GitLab ecosystem, Gitaly is the service that is at the bottom of the
stack for Git data access. This means that when there is a surge of
requests to retrieve or change a piece of Git data, the I/O happens in Gitaly.
This can lead to Gitaly being overwhelmed due to system resource exhaustion
because all Git access goes through Gitaly.

If there is a surge of traffic beyond what Gitaly can handle, Gitaly should
be able to push back on the client calling. Gitaly shouldn't subserviently agree
to process more than it can handle.

We employ concurrency limiting as our primary backpressure mechanism in Gitaly.

## Concurrency queue

Limit the number of concurrent RPCs that are in flight on each Gitaly node for each
repository per RPC using `[[concurrency]]` configuration:

```toml
[[concurrency]]
rpc = "/gitaly.SmartHTTPService/PostUploadPackWithSidechannel"
max_per_repo = 1
```

For example:

- One clone request comes in for repository "A" (a largish repository).
- While this RPC is executing, another request comes in for repository "A". Because
  `max_per_repo` is 1 in this case, the second request blocks until the first request
  is finished.

An in-memory queue of requests can build up in Gitaly that are waiting their turn. Because
this is a potential vector for a memory leak, two other values in the `[[concurrency]]`
configuration can prevent an unbounded in-memory queue of requests:

- `max_queue_wait` is the maximum amount of time a request can wait in the
  concurrency queue. When a request waits longer than this time, it returns
  an error to the client.
- `max_queue_size` is the maximum size the concurrency queue can grow for a
  given RPC. If a concurrency queue is at its maximum, subsequent requests
  return with an error. The queue size is per repository.

For example:

```toml
[[concurrency]]
rpc = "/gitaly.SmartHTTPService/PostUploadPackWithSidechannel"
max_per_repo = 1
max_queue_wait = "1m"
max_queue_size = 5
```

Unauthenticated traffic can be given its own tighter budget with an `unauthenticated` block:

```toml
[[concurrency]]
rpc = "/gitaly.SmartHTTPService/PostUploadPackWithSidechannel"
max_per_repo = 10
max_queue_size = 50

[concurrency.unauthenticated]
max_per_repo = 1
max_queue_size = 5
```

Because the limits above are tracked per repository, requests that spread across many repositories
can still exhaust the resources of the Gitaly node. A `global` block can be added to specify a limit
that is shared across all repositories:

```toml
[concurrency.unauthenticated.global]
max_concurrency = 50
max_queue_size = 100
```

With both blocks set, a request must fit within the budget of its repository and within the global
budget. Both limits apply to unauthenticated requests only.

A `global` block directly under `[[concurrency]]` adds the same node-wide budget for authenticated
requests:

```toml
[[concurrency]]
rpc = "/gitaly.CommitService/FindCommit"
adaptive = true
min_limit = 10
initial_limit = 20
max_limit = 50
max_queue_size = 100
max_queue_wait = "30s"

[concurrency.global]
adaptive = true
min_limit = 200
initial_limit = 500
max_limit = 1000
max_queue_size = 1000
max_queue_wait = "10s"
```

It applies to every request that isn't limited by an `unauthenticated` block. That includes the
internal clients described below and, for an RPC without an `unauthenticated` block, anonymous
requests too.

Size the authenticated global budget above the normal peak of
`gitaly_concurrency_limiting_in_progress{authenticated="true"}` for the RPC on the busiest nodes, so
it only engages when a surge goes beyond that peak.

A request is anonymous when it carries no end-user `username` gRPC metadata. Some trusted internal
callers, notably GitLab Sidekiq background jobs, authenticate with the Gitaly token but carry no
`username`. To keep that internal traffic out of the anonymous limits, list its client name (from
the `client_name` gRPC metadata) in `internal_clients`. Such requests then use the authenticated
limit.

`internal_clients` is a top-level setting. It applies to every RPC, so it is not part of any
`[[concurrency]]` block and must appear at the root of the configuration, before any table header:

```toml
internal_clients = ["gitlab-sidekiq"]

[[concurrency]]
rpc = "/gitaly.SmartHTTPService/PostUploadPackWithSidechannel"
max_per_repo = 10
[concurrency.unauthenticated]
max_per_repo = 2
```

This defaults to `["gitlab-sidekiq"]`. Set it to an empty list to disable the exemption, so every
request without a `username` uses the anonymous limit.

## Pack-objects concurrency

`git-pack-objects` is limited separately from the per-RPC queues above, because it is the
expensive part of serving a clone or fetch. This limiter is keyed by client IP address rather than
by repository:

```toml
[pack_objects_limiting]
max_concurrency = 200
max_queue_size = 200
```

Anonymous traffic, meaning requests that carry no end user, can be given its own tighter budget:

```toml
[pack_objects_limiting.unauthenticated]
max_concurrency = 5
max_queue_size = 10
```

Note that the authenticated and unauthenticated limiters hold separate tokens, so the two
`max_concurrency` values add up rather than sharing a budget.

### Bounding anonymous traffic node-wide

Because the limits above are granted per client IP address, the total work they admit grows with
the number of addresses that connect. A clone flood spread across many addresses can therefore
still start an unbounded number of pack-objects processes. Add a `global` block to place a second
limit on unauthenticated requests, one shared by all of them regardless of where they come from:

```toml
[pack_objects_limiting.unauthenticated]
max_concurrency = 5
max_queue_size = 10

[pack_objects_limiting.unauthenticated.global]
max_concurrency = 50
max_queue_size = 100
```

With both blocks set, a request must fit within the budget of its address and within the shared
budget, and is rejected by whichever of the two runs out of room first. The example above admits
at most five concurrent clones from any single address and at most fifty across all anonymous
traffic.

Either block works on its own. Configuring only the outer block limits per address alone, which is
the behavior when no `global` block is present. Configuring only the `global` block places a
node-wide ceiling on anonymous traffic without isolating addresses from one another.

Both limits apply to unauthenticated requests only. Authenticated traffic is limited per address
by `[pack_objects_limiting]` and is unaffected by the `global` block.

## Note on Rate Limiting

Rate limiting has been removed from Gitaly. For more information about why and the alternatives, please see [issue #5011](https://gitlab.com/gitlab-org/gitaly/-/issues/5011).

## Errors

With concurrency limiting, Gitaly responds with a structured
gRPC `gitalypb.LimitError` error with:

- A `Message` field that describes the error.
- A `BackoffDuration` field that provides the client with a time when it is safe to retry.
  If 0, it means it should never retry.

Gitaly clients (`gitlab-shell`, `workhorse`, Rails) must parse this error and
return sensible error messages to the end user. For example:

- Something trying to clone using HTTP or SSH.
- The GitLab application.
- Something calling the API.

## Metrics

Metrics are available that provide visibility into how these limits are being applied.
See the [GitLab Documentation](https://docs.gitlab.com/administration/gitaly/monitoring/) for details.

The `gitaly_pack_objects_*` metrics carry an `auth` label naming the limiter that reported them.
Alongside `authenticated` and `unauthenticated`, a node-wide limit for anonymous traffic reports
`unauthenticated-global`. A request admitted by both anonymous limiters is counted once under each,
so summing across the label double counts it.

The `gitaly_concurrency_limiting_*` metrics of the per-RPC limiters carry an `authenticated` label
in the same fashion. Alongside `true` and `false`, a node-wide limit for anonymous traffic reports
`false-global`. A request admitted by both anonymous budgets holds both at the same time, so
summing `in_progress` across the label double counts it. Group by the label instead of summing
across it. The two waits happen one after the other, so the `queued` gauges and the drop counters
never double count.

When only the `global` block is configured, the per-repository limit is not enforced. The
`authenticated="false"` series are still exported but stay at zero, and all activity is reported
under `false-global`.

The node-wide limit for authenticated traffic reports `true-global` and relates to `true` in the
same way. A request waiting for the `true-global` budget already holds its per-repository token, so
it's counted as in progress under `true` and as queued under `true-global`.
