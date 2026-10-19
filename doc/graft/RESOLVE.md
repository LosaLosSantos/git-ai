# Automatic conflict resolution during land

`UserLand` can resolve merge conflicts itself. When a change cannot be
rebased or merged cleanly onto the branch tip, the server asks a **resolver**
for the merged content of the conflicting files. The resolver is a separate
program or HTTP service that the operator chooses. The Graft server never
talks to a language model and never imports a vendor SDK. AI is disabled by
default.

The reference resolver, `graft-resolve`, lives in the separate module
`tools/graft-agents`. It runs deterministic strategies out of the box. Its
optional `llm` strategy is covered in [AI resolvers](#ai-resolvers).

## Enabling it

Resolution is off unless it is configured and requested.

```toml
[graft.land.resolver]
enabled = true
command = ["/var/lib/graft/bin/graft-resolve", "--config", "/etc/graft/resolve.toml"]
# or, instead of command:
# url = "https://resolver.internal.example/v1/resolve"
# token_file = "/etc/graft/tokens/resolver"
timeout = "30s"
max_files = 20
max_file_bytes = 1048576
paths_allow = ["**"]
paths_deny = ["deploy/**", "*.lock"]
actors = ["builder", "planner"]
```

- `enabled`: master switch. When false, `resolve_conflicts` is ignored.
- `command` or `url`: exactly one. `command` runs the program with the request
  on stdin. `url` receives an HTTP POST with a bearer token read from
  `token_file`.
- `timeout`, `max_files`, `max_file_bytes`: bounds for each resolver call.
- `paths_allow`, `paths_deny`: glob lists with the same semantics as policy
  refs (`**` as a suffix matches any depth). A conflict in a denied path, or
  in a path that is not allowed, is not sent to the resolver. Apart from the
  `**` suffix, globs follow Go's `path.Match`: `*` never crosses a `/`. So
  `*.lock` denies `go.lock` but not `sub/go.lock`, and `*.env` does not deny
  `a/.env`. To deny a name at several depths, list one glob per depth
  (`*.env`, `*/.env`, `*/*/.env`) or deny whole directories (`secrets/**`).
- `actors`: globs of the actors that may request resolution. Other actors get
  the normal `merge_conflict` error. An empty list allows every caller that
  may land. The actor is the authenticated identity of the caller, which only
  agent authentication sets (see [AGENT-SECURITY](AGENT-SECURITY.md)):
  - with `[[graft.agent]]` configured, the agent name of a caller with an
    agent token, or `shared` for a caller with the shared `auth.token` (for
    example GitLab);
  - without `[[graft.agent]]`, no caller has an identity, so a non-empty list
    admits nobody. The server warns about this at startup.

  The user of the request (`gl_username`, `gl_id`) is chosen by the caller
  and never decides the actor, so listing GitLab user names has no effect.

A client asks for resolution per call:

- SDK: `LandOptions.ResolveConflicts`, and `LandResult` reports the
  resolved paths and the resolver name.
- CLI: `graft land --resolve`, `graft commit --land BRANCH --resolve`.
- RPC: `UserLandRequest.resolve_conflicts` (field 10). The response carries
  `resolved_paths` (field 7) and `resolver` (field 8).

## Flow

1. `UserLand` attempts the land as usual (rebase or merge onto the tip).
2. On a merge conflict, when every condition above holds, the server computes
   for each conflicting path the base, ours and theirs sides (merge base of
   the tip and the landed commit; blobs and modes read through the object
   reader).
3. It sends the request to the resolver (protocol below) and validates the
   answer (see [Validation](#validation)).
4. It builds the merged tree from the merge-tree result, writes the resolved
   files as new blobs or deletes them, and creates a merge commit with parents
   `[tip, commit]`. The message is `merge_message` or
   `Merge <short commit> into <branch>`. Trailers:

   ```
   Graft-Resolved-By: graft-resolve/trivial,union
   Graft-Resolved-Path: docs/CHANGELOG.md
   Graft-Resolved-Path: pkg/config.go
   ```

   The committer is the requesting user.
5. The compare-and-swap update runs as in a normal land. If the branch moved
   in the meantime, the server retries from the top and resolves again
   against the new tip.

Any failure (no resolver, denied path, invalid answer, timeout, unresolvable
conflict) returns the normal `merge_conflict` error and the reason is logged.
The branch is never left in a partial state.

Every resolution is logged at info level as `graft: conflicts of land
resolved`, with the branch, the tip, the landed commit, the merge commit, the
actor, the resolver name, the resolved paths and the `explanation` of the
answer (cut to 4 KiB). The log records the resolution, not the landing: if the
branch update then fails or is retried, a later entry follows.

## Protocol v1

Transport: the request is JSON on stdin (command) or the body of an HTTP POST
(`url`). The response is JSON on stdout or the HTTP response body.

Request (server to resolver):

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

- `ours` is the current branch tip side. `theirs` is the change being landed.
- Any side may be `null` when it is absent (for example in an add-add
  conflict the base is `null`).
- `kind` is one of `content`, `modify-delete`, `add-add`, `mode`.
- `path` is a JSON string, so it carries Unicode text, not bytes. Git paths
  can hold any byte, and a JSON encoder replaces invalid UTF-8 with U+FFFD,
  which would hand the resolver another name. The server therefore never
  sends a path that is not valid UTF-8: a conflict in such a path (for
  example a Latin-1 file name) is not resolved and keeps the normal
  `merge_conflict` error.

Response (resolver to server):

```json
{"version": 1, "resolver": "graft-resolve/trivial",
 "files": [{"path": "a/b.go", "action": "write", "mode": "100644", "content": "<base64>"}],
 "explanation": "free text"}
```

- `action` is `write` (with `mode` and `content`) or `delete`.
- `resolver` names the resolver. It goes into the `Graft-Resolved-By`
  trailer, so it must be 1 to 128 bytes of printable, single-line UTF-8.
- `explanation` is free text. It is logged with the resolution (cut to
  4 KiB) and never interpreted.

Failure signals:

- The command exits non-zero. Its stderr is the reason.
- HTTP `422` with `{"error": "..."}`. The conflict is unresolvable.
- Any other HTTP status, or invalid JSON, is a failure.

## Validation

The server accepts an answer only if all of these hold:

- `version` is 1.
- `resolver` is 1 to 128 bytes of printable, single-line UTF-8.
- Every conflicted path is answered exactly once, and no other path appears.
- Modes are one of `100644`, `100755`, `120000`.
- Every content is at most `max_file_bytes` after decoding.
- Text files contain no conflict marker lines. A marker line is one that
  starts with `<<<<<<< ` or `>>>>>>> `, or one that is `=======` followed by
  nothing but whitespace (a trailing CR is ignored). This is the rule git uses.
  Lines such as `========` or `=======x`, for example Markdown or RST heading
  underlines, are accepted by the server. Files containing a NUL byte are
  binary and are not checked for markers.
  `graft-resolve` applies the same rule to every strategy's result (including
  the `llm` strategy), so it neither sends an answer the server would reject
  nor refuses a file whose heading underlines are made of `=`.

A rejected answer is a failure, not a partial merge.

## Strategies of graft-resolve

`graft-resolve` tries an ordered list of strategies per file. The first
strategy whose result passes the configured checks wins. If none succeeds the
file is unresolved and the program exits 1 with the reason on stderr.

| Strategy  | Behavior |
|-----------|----------|
| `trivial` | One side equals the base, both sides are identical, the sides differ only in whitespace, or both sides deleted the file. |
| `union`   | Concatenates the non-overlapping changes of both sides for configured globs, for example `CHANGELOG.md`. |
| `rerere`  | Reuses a stored resolution keyed by the SHA-256 of the base, ours and theirs contents. Every resolution that passes checks is recorded. |
| `command` | Runs an external merge tool with `{base} {ours} {theirs} {out}` placeholders in temporary files. |
| `llm`     | Optional, see [AI resolvers](#ai-resolvers). |

Checks are per-glob commands run on the resolved file in a temporary
directory, with `{file}` replaced by the file name. A failing check rejects
that strategy's result, and the chain continues with the next strategy.

- The file a check sees is named `check` plus the extension of the conflicted
  path when that extension is 1 to 15 letters or digits (`check.go`,
  `check.md`), and plain `check` otherwise. The repository path never reaches
  the command line, so `{file}` is safe inside a shell string.
- Every check whose `paths` match must pass. Checks do not run for deletions.
- A check passes on exit status 0. Tools such as `gofmt -l` exit 0 even when
  they report a problem, so test their output (see the example below).
- `timeout` bounds one check run, `env` lists extra environment variables to
  pass through (see [graft-resolve subprocesses](#graft-resolve-subprocesses)).

Options of the `command` strategy:

| Option | Meaning |
|--------|---------|
| `command` | argv with the placeholders `{base}`, `{ours}`, `{theirs}` and `{out}`. |
| `output` | `"file"` (default): the tool writes the result to `{out}` and must create it. `"stdout"`: the tool's stdout is the result, for tools such as `git merge-file -p`. |
| `timeout` | bound on one run. |
| `env` | extra environment variables passed to the tool. |

Glob syntax of `paths` in `resolve.toml`: `**` alone matches every path, a
trailing `/**` matches a directory and everything below it, and a leading
`**/` matches any number of leading directories, none included (`**/*.go`
matches `main.go` and `a/b/c.go`). Otherwise `*` does not cross `/`, so
`CHANGELOG.md` matches only the top-level file; use `**/CHANGELOG.md` for
every directory.

The response `resolver` names the strategies used:
`graft-resolve/trivial,union`. Strategy names are at most 64 bytes, so
several long names could exceed the 128 bytes the server accepts. The name is
then shortened: the names that fit are listed, followed by `+N` for the `N`
strategies left out, for example `graft-resolve/union-changelog,+2`. The
`explanation` still names the strategy of every file.

Example `resolve.toml`:

```toml
timeout = "5m"                      # whole request

[[strategy]]
type = "trivial"

[[strategy]]
type = "union"
paths = ["CHANGELOG.md"]

[[check]]
paths = ["**/*.go"]
# A non-zero exit rejects the strategy's result. gofmt -l exits 0 even when
# it lists files, so the check tests the output too.
command = ["sh", "-c", "out=$(gofmt -l \"$1\") && test -z \"$out\"", "sh", "{file}"]
timeout = "30s"
```

Unknown keys are errors, so a typo in the file fails at startup.

Run modes of `graft-resolve`:

- Command mode (default): read one request on stdin, write one response.
- HTTP mode: `graft-resolve serve --listen 127.0.0.1:9300 --config resolve.toml`
  with an optional bearer token (`--token-file`). HTTP mode does not limit
  concurrent requests. Listen on loopback or a private network, and set a
  token.
- Both modes take `--max-request-bytes N` (default 128 MiB,
  `protocol.DefaultMaxRequestBytes`). A larger request is refused.
- Without `--config`, only the `trivial` strategy runs.

## AI resolvers

The `llm` strategy is registered by `tools/graft-agents` and uses an
`llm.Provider` configured in `resolve.toml`. Providers are anthropic, any
OpenAI-compatible endpoint, or any command. The strategy sends the three
versions of a file and the commit messages of both sides (the tip's and the
change's, as the intent of each side, at most 4 KiB each) to the model and
asks for a JSON answer:

```json
{"content": "<complete file>", "explanation": "..."}
```

- The model is not asked how sure it is, and nothing it says about its answer
  decides whether the answer is used: other fields in the answer are ignored,
  and acceptance comes from the checks alone. The former `min_confidence`
  option is gone; a configuration that still sets it fails at startup.
- Any answer containing conflict markers is rejected.
- The answer must parse. graft-resolve parses it with the tree-sitter grammar
  of its language, chosen by extension: TypeScript (`.ts`, `.mts`, `.cts`),
  TSX (`.tsx`), JavaScript (`.js`, `.mjs`, `.cjs`, `.jsx`), Python (`.py`,
  `.pyi`), Go (`.go`), Rust (`.rs`) and Java (`.java`). The check asks
  whether the merge broke the syntax: when both sides parse, an answer with
  any ERROR or MISSING node is rejected, and the reason gives the line and
  column of the first one. When a side does not parse, the grammar cannot
  judge the file, and the answer gets the conflict-marker rule only. Examples:
  Flow annotations in a `.js` file; files that a repository declares to be
  another language; syntax newer than the grammar, such as a Java annotation
  on varargs, or Rust's `dyn 'a + Trait`; test files that are invalid on
  purpose. This is a parse, not a build: names, types and imports are not
  checked, and a few things a compiler refuses still parse (a JSX closing tag
  that does not match its opening tag, for one). Every other file gets the
  conflict-marker rule only. The check is always on for `llm` answers and
  does not apply to the other strategies.

  Measured on the human resolutions of the benchmark corpus (3183 cases),
  the check rejects none of the valid ones. 10 human resolutions fail it,
  each with both sides parsing, and all 10 are broken: 6 were committed with
  conflict markers, and 4 do not compile (confirmed with gofmt and by brace
  count). The check gives no verdict on 83 files where a side does not parse:
  76 JavaScript (Flow, and TypeScript test baselines), 3 Go (tests that are
  invalid on purpose), 2 Java and 2 Rust (syntax newer than the grammar).
- The configured checks still run afterwards.
- For large files the strategy sends only the conflict hunks with context and
  reassembles the file locally.

The grammars are C code compiled with cgo (the MIT-licensed
`github.com/tree-sitter/go-tree-sitter` bindings and the grammar modules of
those languages, in the `tools/graft-agents` module only; the gitai server
has no such dependency). Building graft-resolve therefore needs a C compiler
and `CGO_ENABLED=1`, the default when a compiler is installed. A
graft-resolve built without cgo refuses to start with an `llm` strategy
configured, with a message naming that requirement. The syntax check itself
fails closed in such a build (it rejects every file that has a grammar), and
`graft-resolve bench run` refuses to start.

Every setting of `[strategy.llm]` is either honoured by the provider or an
error at startup. None is dropped silently:

| setting | `openai` | `anthropic` | `command` |
|---|---|---|---|
| `model` | sent | sent (default `claude-opus-5-5`) | `GRAFT_LLM_MODEL`, and the reported model |
| `base_url` | the endpoint | the endpoint | error |
| `api_key_env` | the key's variable (default `OPENAI_API_KEY`) | the key's variable | error |
| `command` | error | error | the program (argv, no shell) |
| `timeout` | per call | per call | per call, the program is killed |
| `max_tokens` | `max_tokens` | `max_tokens` (default 16000) | `GRAFT_LLM_MAX_TOKENS` |
| `effort` | OpenRouter's `reasoning: {"effort": ...}`; `minimal`, `low`, `medium`, `high`, `xhigh` | `output_config.effort`; `low` to `max` (default `high`) | `GRAFT_LLM_EFFORT`, verbatim |
| `options` | error | error | error |

The `openai` provider's `reasoning` parameter is OpenRouter's. Set `effort`
only for an endpoint that takes it.

The `command` program inherits graft-resolve's environment, and gets three
extra variables for its settings: `GRAFT_LLM_MODEL`, `GRAFT_LLM_EFFORT` and
`GRAFT_LLM_MAX_TOKENS`. The token limit is the call's, else the configured
default. Each variable is set only when its setting is, and inherited
variables of those names are removed. The program reads its own endpoint and
credentials, which is why `base_url` and `api_key_env` are errors for it. It
is expected to honour the variables. In particular, the AI budget counts a
call's output as at most `GRAFT_LLM_MAX_TOKENS`, and a command reports no
usage, so each of its calls is booked at that worst case. An llm
strategy also refuses to start in a graft-resolve built without cgo (see
above).

The provider's API keys are withheld from the model: they are read from the
environment or a file by the provider, never put in the prompt. Everything
else is sent to the provider, so the three versions of a file and both commit
messages are shared with it, and any secret that they contain goes with them. Do not send
files you would not send to that provider. The server never calls the model.
Turn AI on only if you accept that the resolution is produced by a model.
Every AI resolution carries the `Graft-Resolved-By` trailer, so it can be
audited with `graft log` and reverted with `graft-verify` if it breaks tests.

### Monthly AI spending cap

An `llm` strategy can carry a monthly cap:

```toml
[strategy.llm]
type = "openai"
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_API_KEY"
model = "openai/gpt-6.1-sol"
max_tokens = 8000                  # required with a budget: it bounds a call's output

[strategy.llm.budget]
monthly_usd = 50                   # the cap; 0 or absent: none, the spend is still tracked
input_usd_per_million = 2.0        # the model's prices; keep them current
output_usd_per_million = 10.0      # output tokens, reasoning included
request_usd = 0                    # optional: a fixed price per call
state = "/var/lib/graft/resolve-budget.json"
```

Before each model call graft-resolve books the call's worst case: its input
counted as one token per byte of the prompt (an upper bound), plus a margin,
and `max_tokens` of output. If that would take the month's spend past the
cap, the call is not made. The file then fails with the reason "AI budget
exhausted", which the server puts in the conflict package for the agent.
After the call the worst case is replaced by the cost of the usage the
provider reports. A provider that reports no usage, or a call that fails, is
left at its worst case. So the recorded spend is never below the real one.
A new calendar month (UTC) starts at zero.

The spend is kept in the `state` file (`{"month": "2026-10", "spent_usd":
...}`). Every update takes an exclusive `flock` on `<state>.lock` and
rewrites the file atomically (temporary file, fsync, rename), so the spend
survives restarts and is shared by every graft-resolve process that uses the
same file. The state file needs a platform with `flock` (Linux, macOS, the
BSDs). The prices are configuration values: graft-resolve does not look them
up, and they should be set from the provider's price page.

The spend is reported in three places:

- In HTTP mode, `GET /metrics` (with the same bearer token) serves
  `graft_resolve_ai_spend_usd` (gauge, the month's spend),
  `graft_resolve_ai_budget_usd` (gauge, the cap) and
  `graft_resolve_ai_budget_exhausted_total` (counter, the calls not made),
  labelled by strategy.
- In command mode, the spend is in the state file.
- In both modes, the answer's `explanation` gives the spend of each strategy
  that answered.

## Bounds and failure modes

| Limit | Default | Where |
|-------|---------|-------|
| files per land | 20 | `max_files` |
| bytes per file | 1 MiB | `max_file_bytes` |
| resolver time | 120 s | `timeout` |
| resolver answer (stdout or HTTP body) | 2 × `max_file_bytes` × files in the request + 64 KiB | server |
| stderr or HTTP 422 body kept as the reason | 4 KiB | server |
| request body read by `graft-resolve` | 128 MiB (`protocol.DefaultMaxRequestBytes`) | `--max-request-bytes` |
| land attempts | 20 | `max_attempts` of the land |

A resolver that hangs is killed at the timeout. Its stderr is kept in the
logged error, which is truncated. An answer larger than its bound kills the
resolver and is a failure.

Before calling the resolver, the server also checks its own limits: at most
`max_files` conflicted files, no side larger than `max_file_bytes`, every path
allowed by `paths_allow`/`paths_deny` and the actor allowed by `actors`. A
request that breaks one of them is not sent.

`graft-resolve` must be able to read the requests the server sends. The
default `--max-request-bytes` covers the default server limits (20 files of
1 MiB, three sides each, base64 encoded). Raise it when you raise `max_files`
or `max_file_bytes`.

### Command resolvers

- The command runs with a minimal environment: only `PATH`, `HOME`, `TMPDIR`,
  `LANG` and `LC_ALL` are copied from the Gitaly server. Secrets in the
  server's environment never reach the resolver. A resolver that needs other
  variables, for example an LLM provider key named by `api_key_env`
  (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`), proxy settings (`HTTPS_PROXY`) or
  `SSL_CERT_FILE`, must get them from a wrapper script or read them from a
  file, for example:

  ```sh
  #!/bin/sh
  ANTHROPIC_API_KEY=$(cat /etc/graft/secrets/anthropic) exec \
    /var/lib/graft/bin/graft-resolve --config /etc/graft/resolve.toml
  ```

- On timeout the server kills the resolver process. Processes the resolver
  started itself are not killed by the server. A resolver that starts
  children (merge tools, check commands, LLM CLIs) should put them in its own
  process group or bound them with their own timeouts. After the kill, the
  server waits at most 2 seconds for the output pipes to close.
- A resolver must not leave a background process holding its stdout after
  it exits. The server then waits for the pipe for 2 seconds and reports a
  failure, even when the answer was complete and the exit status was 0.

### HTTP resolvers

- The request is a `POST` with `Content-Type: application/json` and
  `Authorization: Bearer <token>` when `token_file` is set. The token is read
  once at startup. It must be a single line.
- Redirects are never followed, so the bearer token never travels to another
  URL. A `3xx` answer is a failure.
- Only `422` is a refusal (unresolvable). Every other non-`200` status is a
  failure. Both end in the normal `merge_conflict` error.

### graft-resolve subprocesses

Checks and `command` strategies run with a minimal environment too: `PATH`,
`HOME`, `TMPDIR`, `TEMP`, `TMP`, `LANG`, `LC_ALL`, `LC_CTYPE` and `SYSTEMROOT`,
plus the variables named in their `env` option. The provider keys in the
environment of `graft-resolve` therefore do not reach programs that run on
agent-influenced content. On timeout only the direct child is killed; see the
note on process groups above.
