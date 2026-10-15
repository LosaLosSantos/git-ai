# Superfans: monitoring agents for the changelog

A superfan is a monitoring agent that watches the changelog of a repository
and reports what matters to the people who own each part of the code. It is
a separate program (`graft-superfan`, module `tools/graft-agents`). It never
imports Gitaly, and it only reads events from the JSON lines that
`graft watch --json` prints.

## Pipeline

```
graft log --json --after N  ->  graft-superfan  ->  stdout | webhook | slack
                                 |
                                 +-- graft show --json OID (details, on demand)
```

1. `graft watch --json` (or `graft log --json`) prints one changeset per line
   (format below). `graft watch` follows the log and has `--after` and
   `--json` only; it does not take a branch, so the references to watch are
   set in the superfan configuration (`references`).
2. `graft-superfan` reads the lines from stdin or a file, or runs a command
   and reads its stdout.
3. Deterministic rules classify each changeset.
4. Events are grouped per component and window into a digest.
5. Notifiers deliver the digest. The cursor is saved so restarts do not
   repeat old events.

## Event format

`graft watch --json` and `graft log --json` print one JSON object per line:

```json
{"sequence": 42, "reference": "refs/heads/main", "old_oid": "<oid>", "new_oid": "<oid>",
 "operation": "land", "actor": "agent-7", "rewritten": true,
 "recorded_at": "2026-10-14T06:00:00.123456789Z"}
```

`operation` is the gRPC operation that moved the reference (for example
`land`, `ff_branch`, `revert`). `actor` is the authenticated identity.
Trailers such as `Graft-Auto-Revert` and `Graft-Resolved-By` are read with
`graft show --json` when a rule needs them.

### Batched lands

A batched land (see [LAND-BATCHING.md](LAND-BATCHING.md)) is one changeset with
the operation `land_batch` and the actor `graft-batch`, the identity of the
combined update. The requests it landed are listed in `members`, in landing
order:

```json
{"sequence": 43, "reference": "refs/heads/main", "old_oid": "<oid>", "new_oid": "<oid of the last member>",
 "operation": "land_batch", "actor": "graft-batch", "rewritten": true,
 "recorded_at": "2026-10-14T06:00:01Z",
 "members": [
   {"original_commit": "<oid>", "commit": "<oid>", "actor": "agent-3", "rewritten": true},
   {"original_commit": "<oid>", "commit": "<oid>", "actor": "agent-9", "resolved_paths": ["go.sum"]}
 ]}
```

The superfan judges every member as a change of its own, as it judged every
land before batching:

- the `unexpected actor` rule checks the actor of every member. `graft-batch`
  itself is not judged, so it does not need to be in `expected_actors`;
- every member counts as one operation of the `burst` rule, and the default
  operation `land` counts the members of `land_batch` events too;
- the details of every member's own commit are fetched (`{oid}` of
  `--details-cmd` is the member's `commit`), so the protected-path,
  large-change, revert and AI-resolved rules see the change of every member,
  not only the batch tip.

Findings and digest entries of a member carry the member's actor and commit,
and the batch's sequence. `events` in a digest counts each member as one
changeset.

The server bounds the size of the member list and keeps a prefix of it. When
the last listed member's `commit` is not `new_oid`, or a `land_batch` event has
no members, the changes after the list are unknown: the superfan inspects the
batch tip, reports a medium `batch-incomplete` finding, and files it like a
changeset whose details could not be read (under `unassigned` and every
component that a route names).

## Rules

Rules are deterministic and need no model:

- **large change**: more than `max_files` files or `max_lines` changed lines.
  A rename counts as one file, although `graft show --json` lists both its
  new path (status `renamed`) and its old path (status `renamed-from`).
- **revert**: the commit is a revert, or carries `Graft-Auto-Revert`.
- **AI-resolved conflict**: the commit carries `Graft-Resolved-By`.
- **protected path**: a changed path matches a configured glob.
- **burst**: `lands` or more landing events within `window`. Each member of a
  batched land is one landing event.
- **unexpected actor**: the actor matches none of the globs in
  `expected_actors`. The list is global (in `rules`), not per component. For a
  batched land, the actor of each member is checked.
- **batch-incomplete**: a batched land does not list all of its changes (see
  above).

## Configuration

The configuration is a JSON document. Unknown fields are an error, so a
misspelled key cannot silently disable a rule.

```json
{
  "window": "1h",
  "components": [
    {"name": "api", "paths": ["services/api/**"]},
    {"name": "security", "paths": ["pkg/auth/**"]},
    {"name": "platform", "paths": ["**"]}
  ],
  "routes": [
    {"component": "security", "notifiers": ["team-slack"]},
    {"component": "*", "notifiers": ["ops-webhook"]}
  ],
  "notifiers": [
    {"name": "team-slack", "type": "slack", "url": "https://hooks.slack.example/services/T000/B000/XXXX"},
    {"name": "ops-webhook", "type": "webhook", "url": "https://hooks.internal.example/graft", "token_file": "/etc/graft/tokens/superfan"},
    {"name": "console", "type": "stdout"}
  ],
  "rules": {
    "large_change": {"max_files": 50, "max_lines": 2000},
    "burst": {"lands": 20, "window": "5m"},
    "protected_paths": ["pkg/auth/**"],
    "expected_actors": ["agent-*", "verifier"]
  },
  "summarizer": {"type": "none"},
  "references": ["refs/heads/main"]
}
```

A changeset belongs to every component that has a glob matching one of its
changed paths, not only to the first one. In the example above, a change to
`pkg/auth/` goes into the digests of both `security` and `platform`, because
`**` matches every path: a catch-all component sees every changeset, and a
route of each of the two components is notified. A changeset that matches no
component is filed under the component `unassigned`. `routes` decide which
notifiers receive the digest of each component (`"*"` matches every
component). The summarizer is `none` by default, so no model is involved
unless it is configured.

`references` restricts the analysis to the references that match its globs.
Events on other references are skipped: they are neither analyzed nor fetched,
and they open no window, but the cursor still moves past them.

## Digests

A digest covers one component and one window: the number of its changesets,
the findings of the rules on them (at most 200, the rest counted) and the
highest severity. Notifiers:

- `stdout`: human-readable text, one finding per line.
- `webhook`: a JSON POST of the digest (payload below), with
  `Authorization: Bearer <token>` when `token_file` is set.
- `slack`: an incoming-webhook payload with a single `text` field in Slack
  mrkdwn, and no blocks.

The webhook payload is one JSON object per digest:

```json
{
  "kind": "digest",
  "component": "security",
  "window_start": "2026-10-14T06:00:00Z",
  "window_end": "2026-10-14T07:00:00Z",
  "events": 3,
  "risk": "high",
  "summary": "optional text of the summarizer",
  "findings": [
    {"rule": "protected-path", "severity": "high",
     "summary": "touches protected paths: pkg/auth/token.go",
     "sequence": 42, "reference": "refs/heads/main", "actor": "agent-7",
     "oid": "<oid>", "recorded_at": "2026-10-14T06:12:00.123456789Z"}
  ],
  "omitted": 0
}
```

| Field | Meaning |
|---|---|
| `kind` | Always `digest`. |
| `component` | The component name, or `unassigned`. |
| `window_start`, `window_end` | The window, RFC 3339. Windows are aligned on multiples of `window` in event time. |
| `events` | The number of changesets of the component in the window. Each member of a batched land counts as one. |
| `risk` | The highest severity: `none`, `low`, `medium` or `high`. A summarizer may raise it. |
| `summary` | The summarizer's text. Absent without a summarizer. |
| `findings` | The findings, in event order. `null` in a quiet digest (`include_quiet`), which has none. |
| `omitted` | The number of findings left out of `findings`. Absent when zero. |

Each finding has `rule` (`large-change`, `revert`, `auto-revert`,
`ai-resolved`, `protected-path`, `burst`, `unexpected-actor`,
`details-unavailable`, `batch-incomplete`), `severity`, `summary` (one line of plain text),
`sequence`, `reference`, `recorded_at`, and `actor` and `oid` when they are
known. Text fields come from commits and clients: treat them as untrusted.

Each notifier receives the digest once per window. There is no retry or
backoff: a digest that a notifier fails to take is logged as an error, and
the cursor stops at the sequence before that window. The cursor does not
advance past it for the rest of the run, so after a restart the window is read
again and its digest is sent again. Digests of later windows may then be
repeated too. Make the receiving endpoint idempotent, or restart the superfan
only after the endpoint is back.

A window with no findings sends no digest, unless `"digest": {"include_quiet":
true}` is set in the configuration. Then every window of every component is
sent, which is useful for a heartbeat or for tests.

## Running

```sh
graft-superfan --config superfan.json \
  --watch-cmd "graft log --json --after {after}" \
  --details-cmd "graft show --json {oid}" \
  --tick 30s
```

Flags:

- `--config PATH` (required): the configuration above.
- `--events PATH|-`: read the JSON lines from a file, or from standard input
  when `-` (the default). Use it to replay a saved log. It cannot be combined
  with `--watch-cmd`.
- `--watch-cmd CMD`: run CMD and read its standard output as the event
  stream. CMD is split on spaces, no shell is involved, so paths in it must not
  contain spaces. `{after}` is replaced by the cursor, the sequence of the last
  changeset handled. `graft log --json` lists at most `--limit` changesets
  (default 100) and then ends, so a backlog longer than the limit needs a
  larger `--limit` or a series of runs. `graft watch --json` keeps running
  until it is stopped.
- `--details-cmd CMD`: fetch the details of a commit. CMD is split on spaces
  too. `{oid}` is replaced by the new commit of the event, and for a batched
  land it runs once per member with the member's commit. The command prints
  one `graft show --json` object. Without it, rules that need files do not run
  and every changeset is filed under the component `unassigned`.
- `--cursor PATH`: the cursor file. It overrides `cursor` in the configuration.
- `--tick DURATION` (default `30s`): how often an idle stream is checked for
  elapsed windows. `0` turns the check off, which is what a replay needs: the
  run ends at the end of the input and flushes the open window.

Exit status: 0 on success or interrupt, 1 on a failure, 2 on a usage error.

For example, with the workspace of a clone of the repository (`graft clone`
writes the server address into the workspace, and `GRAFT_TOKEN` supplies the
token):

```sh
graft-superfan --config superfan.json \
  --watch-cmd "graft log -C /srv/graft/ops-workspace --json --after {after}" \
  --details-cmd "graft show -C /srv/graft/ops-workspace --json {oid}" \
  --cursor /var/lib/graft/superfan.cursor
```

## Summaries

The `llm` summarizer turns a digest into a short summary and a risk level
(`low`, `medium`, `high`) using a JSON schema. Its input is bounded. It is
optional and off by default. The summarizer lives in `tools/graft-agents`
and talks to the configured provider, never to the server.

## Status

This document describes the phase-2 contract. The core, the deterministic
rules and the notifiers are phase-2 work package WP18. The `llm` summarizer
is WP19. See `doc/graft/PLAN2.md`. The handling of batched lands follows the
changeset members of phase 3 (`doc/graft/PLAN3.md`, WP23).
