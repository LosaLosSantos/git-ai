# Observability

The four signals Gitaly emits, and where each one is read. Start here to work out which signal
answers your question, then read the third-party documentation.

| Signal              | Emitted as                              | Third-party documentation                          |
|:--------------------|:----------------------------------------|:---------------------------------------------------|
| Logs                | Structured records on `stdout`          | [Kibana](https://log.gitlab.net)                   |
| Metrics             | Prometheus endpoint                     | [Grafana](https://dashboards.gitlab.net)           |
| Distributed traces  | OpenTelemetry spans, when configured    | Jaeger, or any OTLP backend                        |
| Errors              | Sentry events                           | [Sentry](https://new-sentry.gitlab.net)            |

Git processes emit a fifth, narrower signal of their own. See [Trace2 integration](trace2_integration.md).

## Logs

Gitaly writes structured logs to `stdout`, with an access log entry per RPC and a correlation ID on
every request-scoped message. [Logging in Gitaly](logging.md) covers the configuration, what gets
logged, and how correlation IDs tie a request together. [`STYLE.md`](../STYLE.md#logging) covers the
conventions to follow when adding log statements.

## Metrics

Gitaly exposes low-cardinality Prometheus metrics. The per-RPC metrics come from
[`go-grpc-prometheus`](https://github.com/grpc-ecosystem/go-grpc-prometheus); the rest are custom
collectors registered throughout the codebase.

Keep cardinality low. A label whose values are unbounded, such as a repository path, a correlation
ID, or an object ID, multiplies series until the metric is unusable.

## Distributed tracing

Gitaly uses [LabKit v2/trace](https://gitlab.com/gitlab-org/labkit/-/tree/master/v2/trace) for
OpenTelemetry-based tracing, configured through the `GITLAB_TRACING` environment variable as a
URL-style connection string:

```shell
# OTLP over HTTP (insecure)
GITLAB_TRACING=otlp://localhost:4318

# OTLP over HTTPS (secure)
GITLAB_TRACING=otlps://collector.example.com:4318
```

When `GITLAB_TRACING` is unset or empty, tracing is disabled and costs nothing. For running a local
collector, see [Using Jaeger for Gitaly local development](jaeger_for_local_development.md).

Sampling also decides whether Gitaly attaches its Trace2 tracing exporter to the Git processes that
the request spawns. A sampled trace therefore reaches into Git itself.

## Errors

Errors are reported to Sentry, configured by the `sentry_dsn` and `sentry_environment` keys in the
`[logging]` section. The events are only visible to GitLab team members, on the
[Sentry Dashboard](https://new-sentry.gitlab.net).

## Dashboards

The Gitaly dashboards are visible to GitLab team members at <https://dashboards.gitlab.net>. Previously,
they were edited directly within the Grafana UI; however, they are now managed as code within the
[`gitlab-com/runbooks`](https://gitlab.com/gitlab-com/runbooks/-/tree/master/dashboards/gitaly)
repository. The source is written in [`grafonnet`](https://github.com/grafana/grafonnet-lib), a
[`jsonnet`](https://jsonnet.org/) library, and named `dashboards/gitaly/<dashboard name>.dashboard.jsonnet`.

Builds of that repository's default branch upload the dashboards to Grafana, **overwriting any edits
made through the Grafana web interface**. To change a dashboard, change its source `jsonnet` and open a
merge request against `gitlab-com/runbooks`. The web interface is for exploring a query before you commit
it, not for saving changes.

## Latency percentiles

The Gitaly RPC latency histograms use exponential buckets, so percentiles computed from them are
coarse and can mislead. For a more accurate picture, compute percentiles from access log entries in
[Kibana](https://log.gitlab.net) instead, where each entry carries the real duration of the request.
Open 'Visualize', start from the saved `gitaly rpc latency example` visualization, and adjust it.
