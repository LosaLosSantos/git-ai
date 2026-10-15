# AGENTS.md

Entrypoint for agents working on Gitaly.

This document follows the [AGENTS.md](https://agents.md) format. [Agent Skills](https://agentskills.io) are documented under the `skills/` directory.

## Context loading

Read the relevant context before starting a task:

- Working in the codebase: layout, build/test/lint commands, Go/error/proto conventions →
  [doc/development.md](doc/development.md), [STYLE.md](doc/contributing/STYLE.md)
- Adding or changing a CLI command or flag (`gitaly`/`praefect`) → [doc/help_text_style_guide.md](doc/help_text_style_guide.md)
- Commits, merge requests, and code review → [CONTRIBUTING.md](CONTRIBUTING.md), [REVIEWING.md](doc/contributing/REVIEWING.md), [STYLE.md](doc/contributing/STYLE.md)
- Running or writing tests → [doc/development.md](doc/development.md), [STYLE.md](doc/contributing/STYLE.md)
- Observability: logs, metrics, traces, and Git-level instrumentation →
  [doc/observability.md](doc/observability.md) for which signal answers which question, then
  [doc/logging.md](doc/logging.md) or [doc/trace2_integration.md](doc/trace2_integration.md).
  Logging *conventions* live in [`STYLE.md`](doc/contributing/STYLE.md#logging) instead
- Architecture → [doc/DESIGN.md](doc/DESIGN.md), [doc/design_ha.md](doc/design_ha.md)

## Always-on rules

- Apply the smallest change that solves the problem; prefer the standard library and already-vendored
  dependencies over new ones, and deletion over addition; follow the existing patterns in the file you edit.
- Read [CONTRIBUTING.md](CONTRIBUTING.md#commits) before writing a commit message and follow the
  commit hygiene it describes. This applies to every commit, including amends and rebases.
- Open agent-authored Draft merge requests with `[skip ci]` at the end of the title to skip
  unnecessary merge request pipelines while iterating. When ready for CI, remove `[skip ci]` and
  push a commit. Editing the title alone starts no pipeline, and a manually triggered one can still
  be skipped: the merge ref keeps the old title until it is regenerated, which a push guarantees.

## Project notes

- Gitaly is a gRPC server that provides RPC access to Git repositories by running Git close to disk.
- **Gitaly can run in two distinct deployment modes:**
  - *Standalone (sharded):* GitLab connects to Gitaly nodes directly and shards repositories across
    them. A complete, common deployment with no Praefect.
  - *Gitaly Cluster (Praefect):* Praefect proxies and replicates across multiple Gitaly nodes for
    high availability, backed by a PostgreSQL state store.
