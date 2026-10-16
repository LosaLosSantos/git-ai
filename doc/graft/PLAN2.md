# Graft — phase 2 plan (binding contract)

Phase 1 (doc/graft/PLAN.md) is integrated. Phase 2 adds automatic conflict
resolution during land, post-merge verification with auto-revert, monitoring
agents ("superfans"), agent RPC hardening and a benchmark/demo.

## Principle: vendor-neutral, AI opt-in

**The Gitaly server never talks to an LLM and never imports a vendor SDK.**
It speaks small, versioned, documented JSON protocols to components the
operator chooses (an external program or an HTTP endpoint). Graft ships
reference components in a separate Go module, `tools/graft-agents` (own
`go.mod`, so vendor SDKs never enter the server's dependency graph), with
pluggable backends: deterministic strategies (no AI), any OpenAI-compatible
endpoint (OpenAI, Ollama, vLLM, llama.cpp, LM Studio, OpenRouter, Mistral,
...), Anthropic (official Go SDK), or any command. AI is disabled by
default, bounded (sizes, paths, timeouts), validated (conflict markers,
check commands) and auditable (commit trailers, logs).

## Foundation (done, read-only for WPs)

- `proto/operations.proto`: `UserLandRequest.resolve_conflicts` (10),
  `UserLandResponse.resolved_paths` (7) and `resolver` (8); generated code is
  current. Do NOT edit protos or run `make proto`.
- `internal/gitaly/config/config_graft.go`: `Graft.Land.Resolver`
  (`GraftResolver`: `Enabled`, `Command []string`, `URL`, `TokenFile`,
  `Timeout`, `MaxFiles`, `MaxFileBytes`, `PathsAllow`, `PathsDeny`, `Actors`,
  helpers `TimeoutOrDefault`, `MaxFilesOrDefault`, `MaxFileBytesOrDefault`,
  validated in `Graft.Validate`) and `Graft.AgentRPCs []string`.
- `tools/graft-agents/` module (`github.com/feder-cr/git-ai/tools/graft-agents`,
  stdlib only so far) with `llm/llm.go`: `Provider` interface (`Name`,
  `Complete(ctx, Request{System, Prompt, JSONSchema, SchemaName, MaxTokens})`
  → `Response{Text, JSON, Model, Usage}`), `Config` (`type`, `model`,
  `base_url`, `api_key_env`, `command`, `timeout`, `max_tokens`, `effort`,
  `options`), registry (`Register`, `New`, `ErrDisabled`), `ExtractJSONObject`.
- Everything in PLAN.md "Environment and commands" applies (helpers
  `wt.sh`, `gtest.sh`, `gx.sh`, CRLF fixing, no quotes in wsl arguments,
  PGHOST failures ignored). For the separate module run:
  `gx.sh @WPnn go -C tools/graft-agents build ./...`,
  `gx.sh @WPnn go -C tools/graft-agents vet ./...`,
  `gx.sh @WPnn go -C tools/graft-agents test ./...` (only WP14 may add a
  third-party dependency to that module, the official Anthropic Go SDK; run
  `go -C tools/graft-agents mod tidy` after adding it).

## Resolve protocol v1 (normative; document it in doc/graft/RESOLVE.md)

Request (server → resolver), JSON:
```json
{"version": 1,
 "repository": {"storage": "default", "relative_path": "x.git"},
 "branch": "main", "tip": "<oid>", "commit": "<oid>", "merge_base": "<oid>",
 "messages": {"tip": "<tip commit message>", "commit": "<landed commit message>"},
 "actor": "agent-7",
 "files": [{"path": "a/b.go", "kind": "content",
            "base":   {"oid": "<oid>", "mode": "100644", "content": "<base64>"},
            "ours":   {"oid": "<oid>", "mode": "100644", "content": "<base64>"},
            "theirs": {"oid": "<oid>", "mode": "100644", "content": "<base64>"}}],
 "limits": {"max_file_bytes": 1048576}}
```
`ours` = the current branch tip side, `theirs` = the change being landed;
any side may be `null` (absent). `kind` ∈ `content`, `modify-delete`,
`add-add`, `mode`. Response (resolver → server):
```json
{"version": 1, "resolver": "graft-resolve/trivial",
 "files": [{"path": "a/b.go", "action": "write", "mode": "100644", "content": "<base64>"}],
 "explanation": "free text"}
```
`action` ∈ `write`, `delete`. Unresolvable: command exits non-zero (stderr =
reason) or HTTP 422 with `{"error": "..."}`. Any other HTTP status or invalid
JSON is a failure. The server accepts the answer only if: version is 1,
every conflicted path is answered exactly once and no other path appears,
modes are valid (`100644`, `100755`, `120000`), sizes ≤ `max_file_bytes`,
text files contain no conflict marker lines (`<<<<<<< `, `=======`,
`>>>>>>> ` at line start). `resolver` is 1 to 128 bytes of printable,
single-line UTF-8 (it becomes a commit trailer). Paths travel as JSON strings,
so the server never sends a conflicted path that is not valid UTF-8; such a
conflict keeps `merge_conflict`.

## Work packages

Worktree names: WP11 … WP20 (`wt.sh new WP11`).

### WP11 — Server resolver client
Owns `internal/graft/resolver/**`.
Types for the protocol (request/response, base64 content), `Client` with
`Resolve(ctx, Request) (*Response, error)` over `command` (exec via Gitaly's
command package or os/exec with ctx, JSON on stdin, stdout bounded to a sane
multiple of max_file_bytes×files, stderr captured into the error, timeout)
and `url` (HTTP POST, bearer token from TokenFile read at construction,
timeout, response size bound, 422 → unresolvable error type).
`Validate(req, resp) error` implementing every server acceptance rule above.
Path policy helper `Allowed(path, allow, deny []string) bool` (same glob
semantics as policy refs, `**` suffix) and actor matching. `New(cfg
config.GraftResolver) (*Client, error)` (nil client when disabled). Tests:
fake command (a test helper binary or `sh -c` script written to t.TempDir)
and httptest server; every validation rule; timeouts; oversized output; 422.

### WP12 — Land integration + SDK/CLI
Owns `internal/gitaly/service/operations/land*.go` (and new
`land_resolve*.go`), `sdk/land*.go`, the land command in `cmd/graft`, and
the land call site in `internal/graft/workspace`.
In `UserLand`, when the attempt hits a merge conflict and
`resolve_conflicts` is set, the resolver is enabled, the actor matches
`Actors` and every conflicting path is allowed and within `MaxFiles`:
compute per-file base/ours/theirs (merge-base of tip and commit; read blobs
and modes via the object reader), call the resolver (WP11), validate, build
the merged tree = merge-tree result with the resolved files written
(new blobs) or deleted, create a merge commit with parents [tip, commit],
message = `merge_message` or default `Merge <short commit> into <branch>`,
plus trailers `Graft-Resolved-By: <resolver>` and one
`Graft-Resolved-Path: <path>` per file, committer = request user; then the
normal CAS + retry loop (a retry re-resolves against the new tip). Response:
`rewritten=true`, `resolved_paths`, `resolver`. Any failure → the normal
`merge_conflict` error (log the reason). Construct the resolver client once
per server from `deps.GetCfg().Graft.Land.Resolver`. SDK: `LandOptions.ResolveConflicts`,
result fields. CLI: `graft land --resolve` and `graft commit --land B --resolve`.
Tests: fake resolver command resolving a real conflict end to end (content
of the landed file, trailers, parents), invalid answers rejected, denied
path, actor not allowed, disabled resolver, resolver timeout, retry with
concurrent writer.

### WP13 — graft-resolve framework
Owns `tools/graft-agents/resolve/**` (except `resolve/llm*.go`),
`tools/graft-agents/cmd/graft-resolve/**`, `tools/graft-agents/protocol/**`.
`protocol` package: resolve protocol v1 types (independent of the server's,
same JSON). `graft-resolve` reads a request on stdin and writes the response
(command mode), or `graft-resolve serve --listen ADDR` (HTTP mode, optional
bearer token). Config TOML (`--config`): ordered `[[strategy]]` list and
`[[check]]` list. A `Strategy` interface `Resolve(ctx, File, Request)
(Resolution, bool, error)`; built-ins: `trivial` (one side unchanged vs
base, both sides identical, whitespace-only difference, both deleted),
`union` (concatenate non-overlapping sides for configured globs, e.g.
CHANGELOG.md), `rerere` (store/reuse resolutions keyed by sha256 of the
three contents in a directory; records every resolution that passed checks),
`command` (external merge tool with `{base} {ours} {theirs} {out}`
placeholders, temp files). Strategy `llm` is registered by WP19 via an
exported registration hook (`RegisterStrategy(name, factory)`). Checks:
per-glob commands run on the resolved file in a temp dir with `{file}`
placeholder; a failing check rejects that strategy's result and the chain
continues. Each file is resolved independently; if any file stays
unresolved the program fails (exit 1, reason on stderr). Response
`resolver` = `graft-resolve/<strategy names used, comma-separated>`, shortened
to `graft-resolve/<names that fit>,+N` when it would exceed 128 bytes.
Tests for every strategy, checks, chain order, CLI and HTTP modes.

### WP14 — LLM providers
Owns `tools/graft-agents/llm/{anthropic,openai,command}/**` and
`tools/graft-agents/go.mod`/`go.sum`.
- `anthropic`: the official Go SDK `github.com/anthropics/anthropic-sdk-go`.
  BEFORE writing it, read the Claude API guide at
  the Claude API Go SDK documentation
  (and `tool-use.md` in the same directory for structured outputs) and
  follow it exactly: default model `claude-opus-5-5`, adaptive thinking,
  `output_config.effort` from Config.Effort (default `high`), structured
  output via `output_config.format` JSON schema when Request.JSONSchema is
  set, streaming for large outputs, check `stop_reason` (refusal,
  max_tokens) before reading content, API key from `APIKeyEnv` (default
  SDK resolution, `ANTHROPIC_API_KEY`), base URL override. Do not guess SDK
  names: write, compile, fix from compiler errors.
- `openai`: OpenAI-compatible `POST {base_url}/chat/completions` over
  net/http (no SDK): model, messages (system+user), `max_tokens`,
  `response_format` `{"type":"json_schema",...}` when a schema is given
  with automatic fallback to `{"type":"json_object"}` and then to plain
  text + `llm.ExtractJSONObject` if the server rejects it (400); API key
  from `APIKeyEnv` (default `OPENAI_API_KEY`, optional for local servers);
  usage parsing; retries on 429/5xx with backoff (max 3).
- `command`: run Config.Command, write `{"system":..,"prompt":..,"json_schema":..}`
  on stdin, read the answer text on stdout (extract JSON when a schema is
  set).
Each registers via `llm.Register` in `init()`. Tests use httptest fakes
(no network, no real keys): request shapes, schema handling, fallbacks,
errors, refusals.

### WP15 — Agent RPC allowlist and policy hardening
Owns `internal/gitaly/server/auth/**` (allowlist), `internal/gitlab/standalone_client.go`
(messages), `internal/graft/requestdefaults/**` (if needed), and the
reference-transaction policy check (find it with git grep EvaluatePolicy).
- Callers authenticated with an agent token (`identity.SourceAgent`) may
  only call an allowlist of RPCs; everything else → PermissionDenied. The
  built-in allowlist must contain exactly what the SDK (`sdk/`), the CLI
  (`cmd/graft`), `graft-cache` pass-through use from agents, and the
  read-only/accessor RPCs useful to agents (grep sdk/ and cmd/graft for every
  gRPC method they call; include ObjectService, ChangesetService,
  DerivedDataService, UserLand, UserCommitFiles, UserCreateBranch,
  UserDeleteBranch (policy-checked), Find*/List*/Get* accessors of
  Commit/Ref/Blob/Tree/Diff services, ServerInfo, health); plus
  `cfg.Graft.AgentRPCs`. Explicitly NOT allowed: RepositoryService mutators
  (CreateRepositoryFrom*, RemoveRepository, ReplicateRepository, WriteRef,
  FetchRemote, SetCustomHooks, RestoreCustomHooks, ...), RefService
  UpdateReferences/DeleteRefs, CleanupService, ObjectPoolService,
  HookService, InternalGitaly, PartitionService.
- Policy covers `HEAD` (symbolic ref updates) too.
- Policy denials surface as PermissionDenied (not Internal) on RPCs without
  pre-receive; standalone denial messages do not start with "GitLab:".
- Tests for every point, and an exhaustive table test proving every
  registered RPC (protoregistry) is either allowlisted or denied for agents
  and that the SDK/CLI paths still work.

### WP16 — graft-verify core
Owns `internal/graft/verify/**` (except `bisect*.go`, `revert*.go`),
`cmd/graft-verify/**`. Built on `sdk/` and `internal/graft/workspace`.
Config TOML: address, token, storage, repository, branch, test command
(argv, run in the materialized tree, env, timeout), sparse prefixes,
batch window, green ref name (default `refs/graft/green/<branch>`),
state file (cursor). Loop: WatchChangesets from the cursor; collect
changesets on the branch; after the batch window, materialize the current
tip (workspace clone/update into a work dir), run the tests; on success move
the green ref with UserUpdateBranch/UpdateReferences-equivalent CAS allowed
by WP15 (use RefService only if the caller is not an agent; document) —
simplest: the verifier runs with the shared token (operator credential);
on failure call the `Bisector` interface (implemented by WP17); persist the
cursor; graceful shutdown; `--once` mode for CI. Tests with an in-process
Gitaly and a fake test command (script that fails when a marker file is
present).

### WP17 — graft-verify bisect + auto-revert
Owns `internal/graft/verify/bisect*.go`, `internal/graft/verify/revert*.go`.
Flake check (re-run the failing tip once); first-parent commit list since
the last green; binary search running the test command at each midpoint
(materialized via workspace); culprit = first failing commit; revert it with
`UserRevert` on the branch (retry via UserLand semantics if the branch moved:
revert onto the new tip), message `Revert "<subject>"` + trailer
`Graft-Auto-Revert: <culprit oid>`; never revert a commit that is itself an
auto-revert; cap reverts per hour (config); dry-run; notification via a
`Notifier` interface (stdout and webhook JSON implementations here).
Tests: culprit in middle/first/last, flaky test, cap, dry-run, revert of
revert refused.

### WP18 — graft-superfan core
Owns `tools/graft-agents/superfan/**` (except `summarize*.go`),
`tools/graft-agents/notify/**`, `tools/graft-agents/cmd/graft-superfan/**`,
and the `--json` output of the `watch`, `show` and `log` commands in
`cmd/graft` (add it if missing; one JSON object per line for watch/log).
The module must stay independent of the Gitaly module (no import of it):
consume events from the JSON lines produced by `graft watch --json` (document
the format; read from a command's stdout or a file/stdin) and fetch details
with `graft show`/`graft cat` commands when needed, via an `EventSource`
interface. Components: config maps path globs → component names and
notifier routes. Deterministic rules: large change (files/lines over
threshold), revert or auto-revert trailer, AI-resolved conflict trailer,
protected path touched, burst of lands (N per window), actor outside an
expected set. Digest per component per window. Notifiers: stdout, webhook
(JSON POST), slack (incoming webhook payload). Summarizer interface
(implemented by WP19); `none` default. Cursor persistence. Tests with fake
event streams and httptest notifiers.

### WP19 — LLM strategy + superfan summarizer
Owns `tools/graft-agents/resolve/llm*.go`, `tools/graft-agents/superfan/summarize*.go`.
- Resolve strategy `llm`: uses an `llm.Provider` from the strategy config;
  prompt with the three versions (full files when small; otherwise the
  conflict hunks with context and instructions to return the whole file is
  replaced by hunk-level answers reassembled locally), JSON schema
  `{"content": string, "confidence": "high"|"medium"|"low", "explanation": string}`;
  reject `low` confidence (configurable), reject any answer with conflict
  markers; checks still run afterwards (WP13). Prompt must state clearly:
  preserve both sides' intent, never invent unrelated changes, output the
  complete file.
- Superfan summarizer `llm`: digest → concise summary + risk level (JSON
  schema), bounded input size.
- Tests with a fake `llm.Provider` (no network): prompts contain the right
  material, schema answers parsed, low confidence rejected, markers rejected,
  large-file hunk mode reassembles correctly.

### WP20 — Bench, demo, docs, phase-2 e2e
Owns `cmd/graft-bench/**`, `examples/graft/demo/**`,
`doc/graft/{RESOLVE,VERIFY,SUPERFAN,AGENT-SECURITY,BENCH}.md`, the phase-2
sections of `README.md` and `doc/graft/LIMITATIONS.md`,
`internal/graft/e2e/phase2_test.go`.
- `graft-bench`: N agents (goroutines via `sdk/`) on one branch, each
  committing K changes; conflict-rate knob (fraction of changes touching a
  shared file region); modes: `land` (UserLand rebase), `land-resolve`
  (with a deterministic resolver, e.g. graft-resolve with `union`/`trivial`
  for the shared file), `queue` (simulated serialized merge queue: test
  duration D per change, one at a time); report lands/min, p50/p95/p99
  latency, attempts, conflicts, resolutions, failures; JSON + table output.
  It may start an in-process Gitaly (testserver) with `--inprocess` for
  quick runs, or target an address.
- Demo script running graft-bench in the three modes with 50 agents and
  printing a comparison table.
- e2e: resolve a real conflict through `graft-resolve` (trivial/union, no
  network) via UserLand; agent denied a non-allowlisted RPC; graft-verify
  `--once` detecting a failing commit and auto-reverting it; superfan
  receiving events from `graft watch --json` and posting to an httptest
  webhook.

## Milestone acceptance
Main module: `go build ./...`, vet/gofmt clean on touched packages,
`make lint-proto`, `gtest.sh` green on all Graft packages + regression list
(PGHOST failures only), phase-1 and phase-2 e2e green. Separate module:
`go -C tools/graft-agents build/vet/test ./...` green, no network needed for
tests.
