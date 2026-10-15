# open_scion

open_scion is an open-source (MIT), vendor-neutral, agent-scale version control
server. It is a fork of [Gitaly](https://gitlab.com/gitlab-org/gitaly) that
implements the ideas Anthropic described for its internal "Scion" system in
[Redesigning source control](https://www.anthropic.com/engineering/redesigning-source-control).

> **Not affiliated with Anthropic or GitLab.** open_scion is an independent
> project inspired by a public Anthropic engineering article. It is not an
> Anthropic product, and Anthropic does not endorse or maintain it. "Scion" is
> the name Anthropic uses for its own internal system.

Every repository stays a normal Git repository, every upstream Gitaly RPC keeps
working, and the server never talks to a language model: AI features are
optional and live outside the server, behind a vendor-neutral protocol.

Inside the code base the extensions are called **Graft**: the `[graft]`
configuration section, the `graft` CLI, the `internal/graft` packages and the
`doc/graft` documentation.

## What it does

- **Server-centric, always-online clients.** Clients fetch exactly the objects
  and trees they need (`ObjectService`); history operations (blame, path history,
  search, ancestry) run on the server and are cached as derived data.
- **Landing without a merge queue.** `UserLand` rebases a change onto the tip and
  lands it with a compare-and-swap; concurrent lands on one branch are batched.
  Conflicts can be resolved automatically through a vendor-neutral resolver
  protocol (any LLM provider, or none).
- **Changeset log.** Every accepted reference update is recorded with its
  operation and actor, and can be listed or watched (`ChangesetService`).
- **Storage core.** The repository state can live in a strongly consistent
  database (SQLite or PostgreSQL) as a write-ahead log of changesets with a
  compare-and-swap per reference, with content-addressed packs in object storage
  (file, S3, GCS, Azure through gocloud). Server nodes become caches that any node
  can rebuild, so they scale horizontally. Shadow mode lets you run it alongside
  the classic storage before switching. See [STORAGE.md](doc/graft/STORAGE.md).
- **Agents as first-class users.** Per-agent tokens, an RPC allowlist, branch
  policies, an agent-friendly review service with semantic approval rules, code
  search, post-merge verification with automatic bisect and revert, and
  "superfan" monitoring agents.
- **Tiered caching** of immutable objects (`graft-cache`).

## Quick start

Requirements: Linux or macOS, Go 1.26, and the build dependencies of Gitaly
(`make` builds the bundled Git).

```shell
git clone https://github.com/feder-cr/open_scion.git
cd open_scion
make build                         # builds gitaly, graft, graft-cache, ... into _build/bin
examples/graft/quickstart.sh       # a standalone server, a cache daemon, two agents and a land, in a temporary directory
./_build/bin/graft                 # the CLI and its commands
```

To run your own server, adapt [`examples/graft/config.toml`](examples/graft/config.toml)
and start it with `_build/bin/gitaly serve config.toml`.

The [Quickstart](doc/graft/QUICKSTART.md) walks through a standalone server,
agent tokens, the cache tier and the CLI. For the storage core, start with
`[graft.storage] mode = "shadow"` and read [STORAGE.md](doc/graft/STORAGE.md).

## Documentation

- [Architecture](doc/graft/ARCHITECTURE.md): components, data flows, and how each Scion idea maps to the code.
- [Quickstart](doc/graft/QUICKSTART.md) and [Configuration](doc/graft/CONFIGURATION.md) (the `[graft]` reference).
- [Storage core](doc/graft/STORAGE.md): database + object storage, modes, import, operations, benchmark, limits.
- [Land batching](doc/graft/LAND-BATCHING.md), [derived data](doc/graft/DERIVED.md), [conflict resolution](doc/graft/RESOLVE.md).
- [Review](doc/graft/REVIEW.md), [search](doc/graft/SEARCH.md), [verification](doc/graft/VERIFY.md), [superfans](doc/graft/SUPERFAN.md) and [Slack chat](doc/graft/SUPERFAN-CHAT.md).
- [Agent security](doc/graft/AGENT-SECURITY.md), [fuzzing](doc/graft/FUZZING.md), [benchmarks](doc/graft/BENCH.md).
- [Limitations and roadmap](doc/graft/LIMITATIONS.md).
- Design documents of each phase: [PLAN.md](doc/graft/PLAN.md) to [PLAN6.md](doc/graft/PLAN6.md).

## Status

Version 0.1.0, the first public release. The features above are implemented and
tested (unit, end-to-end, multi-node, crash and fuzz tests), but interfaces may
still change. Known gaps, such as garbage collection of packs in the storage
core, are listed in [LIMITATIONS.md](doc/graft/LIMITATIONS.md) and
[STORAGE.md](doc/graft/STORAGE.md#limits).

## License

MIT, see [LICENSE](LICENSE). open_scion is based on Gitaly by GitLab Inc.,
whose MIT license and copyright notices are kept. The `ee/` directory of
upstream Gitaly (not MIT) is not part of this repository. Third-party notices
are in [NOTICE](NOTICE).

---

# Upstream: Gitaly

The rest of this file is the README of upstream Gitaly, kept for reference.

## Gitaly

![Gitaly](https://gitlab.com/gitlab-org/gitaly/uploads/509123ed56bd51247996038c858db006/gitaly-wordmark-small.png)

**Quick Links**:
  [**Roadmap**][roadmap] |
  [Want to Contribute?](#contributing) |

Gitaly is a Git [RPC](https://en.wikipedia.org/wiki/Remote_procedure_call)
service for handling all the Git calls made by GitLab.

For documentation generated from the protobuf definitions (in `proto/` directory),
see [Gitaly RPC documentation](https://gitlab-org.gitlab.io/gitaly/).

To see where it fits in please look at [GitLab architecture](https://docs.gitlab.com/development/architecture/#system-layout).

## Project Goals

Fault-tolerant horizontal scaling of Git storage in GitLab, and particularly, on [GitLab.com](https://gitlab.com).

## Current Status

All application code accesses Git repositories via Gitaly.

Besides "Git over RPC" functionality, Gitaly also offers an optional [high-availability solution](#high-availability).

We are building features according to our [roadmap][roadmap].

## Installation

Most users won't install Gitaly on its own. It is already included in [your GitLab installation](https://about.gitlab.com/install/).

Gitaly requires Go 1.26. Run `make` to compile the executables
required by Gitaly.

Gitaly uses `git`. Versions `2.47.0` and newer are supported.

## Configuration

The administration and reference guide is [documented in the GitLab project](https://docs.gitlab.com/administration/gitaly/).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and a [list of quick win issues](https://gitlab.com/gitlab-org/gitaly/-/issues/?sort=due_date&state=opened&label_name%5B%5D=quick%20win&first_page_size=20).

## Name

Gitaly is a tribute to Git and the town of [Aly](https://en.wikipedia.org/wiki/Aly). Where the town of
Aly has zero inhabitants most of the year we would like to reduce the number of
disk operations to zero for most actions. It doesn't hurt that it sounds like
Italy, the capital of which is [the destination of all roads](https://en.wikipedia.org/wiki/All_roads_lead_to_Rome). All Git actions in
GitLab end up in Gitaly.

## Design

The [Gitaly and Gitaly cluster](https://docs.gitlab.com/administration/gitaly/) documentation details the architecture
and design of Gitaly, including a list of known Gitaly consumers.

## High Availability

Gitaly offers a High Availability solution known as Gitaly Cluster ([product documentation](https://docs.gitlab.com/administration/gitaly/)).

- In its current iteration, client traffic goes through [Praefect](https://docs.gitlab.com/administration/gitaly/praefect/), which then replicates data to multiple Gitaly servers, and stores state in a PostgreSQL database (see [Design](#design) above).

## Further reading

More about the project and its processes is [detailed in the docs](doc/README.md).

## Distributed Tracing

Gitaly supports distributed tracing through [LabKit](https://gitlab.com/gitlab-org/labkit/) using [OpenTracing APIs](https://opentracing.io).

By default, no tracing implementation is linked into the binary, but different OpenTracing providers can be linked in using [build tags](https://golang.org/pkg/go/build/#hdr-Build_Constraints)/[build constraints](https://golang.org/pkg/go/build/#hdr-Build_Constraints). This can be done by setting the `BUILD_TAGS` make variable.

For more details of the supported providers, see LabKit, but as an example, for Jaeger tracing support, include the tags: `BUILD_TAGS="tracer_static tracer_static_jaeger"`.

```shell
make BUILD_TAGS="tracer_static tracer_static_jaeger"
```

Once Gitaly is compiled with an opentracing provider, the tracing configuration is configured via the `GITLAB_TRACING` environment variable.

For example, to configure Jaeger, you could use the following command:

```shell
GITLAB_TRACING=opentracing://jaeger ./gitaly config.toml
```

## Continuous Profiling

Gitaly supports Continuous Profiling through [LabKit][] using [Stackdriver Profiler](https://cloud.google.com/profiler).

For more information on how to set it up, see the [LabKit monitoring docs](https://gitlab.com/gitlab-org/labkit/-/blob/master/monitoring/doc.go).

## Training Videos

The [Gitaly Training and Resources Playlist](https://www.youtube.com/playlist?list=PL05JrBw4t0KqPMCB7xW1ExIRp6YuD_7_N)
contains videos that give an overview into how Gitaly works, as well as dive
deep into different parts of Gitaly and even Git.

## Presentations

- [TheConf talk on Scaling GitLab Git storage with Gitaly, 2019-08-16](https://speakerdeck.com/olsfer/how-gitlab-scaled-git-access-with-a-go-service)
- [Infrastructure Team Update 2017-05-11](https://about.gitlab.com/2017/05/11/functional-group-updates/#infrastructure-team)
- [Gitaly Basics, 2017-05-01](https://docs.google.com/presentation/d/1cLslUbXVkniOaeJ-r3s5AYF0kQep8VeNfvs0XSGrpA0/edit#slide=id.g1c73db867d_0_0)
- [Git Paris meetup, 2017-02-22](https://docs.google.com/presentation/d/19OZUalFMIDM8WujXrrIyCuVb_oVeaUzpb-UdGThOvAo/edit?usp=sharing) a high-level overview of what our plans are and where we are.

[roadmap]: https://handbook.gitlab.com/handbook/engineering/infrastructure-platforms/tenant-scale/gitaly/#roadmap
[LabKit]: https://gitlab.com/gitlab-org/labkit/
