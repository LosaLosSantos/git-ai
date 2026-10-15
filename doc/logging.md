# Logging in Gitaly

How Gitaly emits logs. The conventions to follow when writing log statements, such as
context-based logging, snake case fields, and the `log.Logger` interface, are in
[`STYLE.md`](../STYLE.md#logging).

## Where logs go

Gitaly writes structured logs to `stdout`. There is no log directory to configure.
Processes Gitaly spawns, Git hooks included, send their logs back to Gitaly through a
pipe, and Gitaly emits them as part of its own log stream.

The `[logging]` section of the configuration file controls the output:

| Key      | Values                                                                    |
|:---------|:--------------------------------------------------------------------------|
| `format` | `text` (default) or `json`. Anything else is a startup error              |
| `level`  | `trace`, `debug`, `info` (default), `warn`, `error`, `fatal`, or `panic`  |

`[logging]` also carries the Sentry DSN and environment. See
[Observability](observability.md#errors).

## What Gitaly logs

- **An access log entry for every RPC**, produced by the gRPC logging middleware in
  [`internal/grpc/middleware/loghandler`](../internal/grpc/middleware/loghandler). Other middlewares
  contribute fields to it, so a single entry carries the RPC name, status code, timings, and
  request metadata.
- **Global messages**, from startup, shutdown, and background work such as housekeeping.
- **Standard error from Git.** Many RPCs spawn Git processes that write warnings or errors to
  `stderr`. Gitaly captures that output and logs it.

## Correlation IDs

Every request-scoped message carries the request's correlation ID, which is how you separate one
request's messages from everything happening concurrently. The ID propagates from the client, through
Gitaly, into the Git processes Gitaly spawns. Trace2 events are tagged with it through
`GIT_TRACE2_PARENT_SID`. See [Trace2 integration](trace2_integration.md).

Filtering by correlation ID is the first step in almost any investigation: it turns an interleaved
stream into the story of one request.

## Reading logs

- In production, query logs through [Kibana](https://log.gitlab.net) by selecting
  `pubsub-gitaly-inf-gprd*` in the Data view.
- To analyze a log file from a support package locally, see
  [Setting up a local ELK stack](setup_local_elk_for_downloaded_logs.md).
