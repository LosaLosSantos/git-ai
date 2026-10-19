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
  resolved paths and the resolver name. On a conflict the server did not
  resolve, `sdk.LandConflict(err)` returns the conflict package,
  `Repo.ConflictSides` reads the sides, and `LandOptions.Resolution`
  (start from `sdk.ResolutionFor`) lands the agent's resolution. `Land`
  refuses a resolution whose request exceeds `sdk.MaxLandRequestBytes`
  (4 MiB) before sending it.
- CLI: `graft land --resolve`, `graft commit --land BRANCH --resolve`.
  `graft land --resolve` prints the package when the land fails, and with
  `--conflicts-dir DIR` writes it: `DIR/files/<path>` is the merge with git's
  diff3 markers (the clean changes of both sides applied), the sides are in
  `DIR/sides/<path>.base|.ours|.theirs`, the package in `DIR/conflict.json`.
  The files and the sides live in separate directories, so no repository
  path can collide with a side or the package. Edit the files under
  `DIR/files`, then `graft land --resolved DIR` lands them: a present file is
  written (with the tip side's mode), an absent one is deleted.
- RPC: `UserLandRequest.resolve_conflicts` (field 10). The response carries
  `resolved_paths` (field 7) and `resolver` (field 8). The re-land is
  `UserLandRequest.resolution` (field 11).

## Flow

1. `UserLand` attempts the land as usual (rebase or merge onto the tip).
2. On a merge conflict the server lists the conflicting files with their base,
   ours and theirs sides (merge base of the tip and the landed commit; blobs
   and modes read through the object reader). The whole request is refused
   when resolution is not enabled, the actor may not resolve, there are more
   than `max_files` files, or the history is criss-cross.
3. It decides for each file whether it may be sent (see
   [What is never sent](#what-is-never-sent)), sends the others to the
   resolver in one request (protocol below), and validates the answer (see
   [Validation](#validation)).
4. Only when every file is resolved, it builds the merged tree from the
   merge-tree result, writes the resolved
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

Otherwise (resolution disabled or refused, a file that is never sent, an
invalid answer, a timeout, an unresolvable file) the land fails with the
normal `merge_conflict` error, the reason is logged, and the error carries the
conflict package as a second detail (see
[The conflict package and the re-land](#the-conflict-package-and-the-re-land)).
Nothing lands partially: the branch is never left in a partial state.

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

- The command exits non-zero. Its stderr is the reason, unless its stdout is
  an error response (below).
- HTTP `422` with an error response. The conflict is unresolvable.
- Any other HTTP status, or invalid JSON, is a failure.

The error response may give the outcome of every file of the request:

```json
{"error": "unresolved conflict: ...",
 "files": [{"path": "a/b.go", "outcome": "rejected", "reason": "syntax check: ..."},
           {"path": "c.go", "outcome": "resolved"}]}
```

`outcome` is `resolved` (resolved, but the request was not resolved as a
whole), `rejected` (a result failed a rule or a check) or `failed`. The server
uses the list only when it has exactly one entry per requested path, each with
a known outcome; otherwise every sent file fails with the overall reason.
`graft-resolve` writes this response on a refusal, on stdout in command mode
and as the 422 body in HTTP mode. A resolver without `files` keeps working.

A second operation, `check`, asks the resolver to check an agent's resolution
with the checks of its own answers (see
[The conflict package and the re-land](#the-conflict-package-and-the-re-land)).
The request is a resolve request with `"operation": "check"` and the agent's
files in `resolution` (the shape of the response's `files`). The answer is:

```json
{"version": 1, "operation": "check", "resolver": "graft-resolve",
 "files": [{"path": "a/b.go", "ok": false, "reason": "syntax check: ...", "syntax": "checked"}]}
```

with one verdict per file of `resolution`, in its order. `syntax` is
`checked`, `unchecked` (a side does not parse with the grammar) or `none` (no
grammar, or a deletion). An absent `operation` means `resolve`, so existing
resolvers keep working; one that answers a `check` with anything but a check
response makes the server refuse the resolution.

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

## What is never sent

The server decides for each conflicting file, before the request, whether it
may be sent. A file that may not goes to the agent, and the conflict package
gives the reason. The other files are still sent. In order:

1. The path: a line break or NUL byte (a commit trailer cannot carry it), not
   valid UTF-8 (the protocol cannot carry it), or denied by `paths_allow` /
   `paths_deny`, for the path and for every path a side comes from (a rename
   brings a side from another path).
2. The sides: no side under the name (a file/directory conflict), no base and
   a missing side, or a side that is not a file (a submodule).
3. large: a side over `max_file_bytes`, decided from its size before it is
   read.
4. binary: a NUL byte in a side.
5. lockfile: the base name is `package-lock.json`, `yarn.lock`,
   `pnpm-lock.yaml`, `poetry.lock`, `Cargo.lock`, `go.sum`, or one of
   `lockfiles`.
6. generated: `linguist-generated` is set for the path by the tip's
   attributes (one `git check-attr --source <tip>` for all paths), or a side's
   first 10 lines match `Code generated .* DO NOT EDIT\.`, `@generated`,
   `DO NOT EDIT`, `(?i)auto-generated` or one of `generated_markers`.
7. minified: the base name matches `*.min.js`, `*.min.mjs`, `*.min.css`,
   `*.bundle.js` or one of `minified.globs`, or a side larger than 2 KiB has an
   average line longer than `minified.average_line_bytes` (300) or a line
   longer than `minified.longest_line_bytes` (5000).

Secrets are not a reason: a file's content, secrets included, goes to the
resolver when the rules above let it. Use `paths_deny` for paths that must
never leave the server. `graft-resolve bench` applies the same rules (3 to 7);
both are tested against `internal/graft/resolver/testdata/never-send.json`.

## Optional verification of a resolution

`[graft.land.resolver.verify]`, off by default, runs a command on the merged
tree of every resolution before it lands, a resolver's answer or an agent's
resolution alike:

```toml
[graft.land.resolver.verify]
enabled = true
command = ["/usr/local/bin/graft-check-build"]
timeout = "5m"
```

The server archives the merged tree (the resolution included) into a
temporary directory and runs `command` there, with a minimal environment
(`PATH`, and `HOME` and `TMPDIR` in a directory of their own). A non-zero exit or a
timeout rejects the resolution: the land fails, every file of the package is
`REJECTED`, and the reason is `verification failed:` with the exit status and
the first 4 KiB of the output.

**A verify command such as a build or a test suite executes code from the
merged tree: code that agents and the AI wrote, run as the gitaly user on the
server.** The server only limits the environment of the command (`PATH`,
`HOME`, `TMPDIR`); that is not a sandbox. The command can read what the gitaly
user can read, reach the network, and leave processes behind if it forks
away from its process group. Run it inside an isolation boundary that you
control: a container, or a separate unprivileged user without network
access. For example, a wrapper that runs the build in a throwaway container
with the checkout mounted read-only and no network:

```sh
#!/bin/sh
# /usr/local/bin/graft-check-build: run from the checkout by the server.
exec docker run --rm --network=none --read-only --tmpfs /tmp \
  --user 65534:65534 --memory 2g --pids-limit 512 \
  -v "$PWD":/src:ro -w /src golang:1.26 go build ./...
```

It also costs time on every resolution, which is why it is off by default;
the trailers and `graft-verify` remain the safety net.

## Finding and reverting AI resolutions

Every resolution carries its trailers, so it can be found and undone:

- `Graft-Resolved-By` names the resolver (`graft-resolve/llm`, `agent/<actor>`)
  and `Graft-Resolved-Path` each resolved path; `Graft-Checked-By` says that a
  resolver checked an agent's resolution. In a clone, `git log
  --grep='^Graft-Resolved-By:'` lists them; `graft show --json REV` prints the
  trailers of one commit.
- The server logs every resolution at info level (`graft: conflicts of land
  resolved`, with the actor, the resolver, the paths and the explanation).
- `graft-verify` ([VERIFY.md](VERIFY.md)) tests the tip after landing,
  bisects a failure to the commit that broke it and reverts it with
  `UserRevert`, whether it was an AI resolution or not.

## The conflict package and the re-land

When a land with `resolve_conflicts` (or with a `resolution`) fails on its
conflicts, the `merge_conflict` error carries a second detail after
`UserLandError`: a `LandConflict`. Clients that know only `UserLandError` see
the error unchanged. The package holds:

- `tip`, `commit` and `merge_base` (empty when there is none);
- both commit messages, cut to 4 KiB each, with `messages_truncated`;
- `reason`, when the server did not attempt the request at all;
- per file (at most `max_files`): the path, the kind, each side's blob OID and
  mode, the conflicting hunks, the outcome and its reason (cut to 512 bytes).

Each hunk gives line ranges of each side, 1-based and half-open (`[start,
end)`), each counting the lines of the side it names. An empty range is an
insertion point before line `start`. A file whose diff is too large to
compute (more than about 2800 changed lines on a side) has no hunks. The
outcomes are:

| outcome | meaning |
|---|---|
| `NOT_SENT` | never sent (see above); the reason says why |
| `REJECTED` | the result failed a rule or a check, e.g. a syntax error at line:column |
| `FAILED` | the resolver failed on it: a model error, `AI budget exhausted`, no strategy applied |
| `RESOLVED` | resolved, but the land failed because another file was not |
| `NOT_ATTEMPTED` | the request was not attempted; see `reason` |

The agent reads the sides by OID with `GetBlobs`, resolves every file, and
lands again with `UserLandRequest.resolution` (field 11): the `tip` and
`commit` of the package and, for every conflicting path, `write` (mode and
content) or `delete`. The server accepts it only when:

- `resolution.commit` equals `commit_id` (otherwise `InvalidArgument`);
- `resolution.tip` is the branch's current tip (otherwise the land fails
  with a fresh package for the new tip);
- it resolves exactly the conflicting paths, each once;
- every file passes the acceptance rules of an answer (modes, size, conflict
  markers);
- when a resolver is configured, the files that may be sent to it pass its
  `check`: the syntax check and the configured checks, as an `llm` answer.
  A check that cannot run fails the land, with the reason in the package.
  A file with a `syntax: unchecked` verdict lands, and is counted in
  `graft_land_resolution_syntax_unchecked_total` and the server log.

The merge commit has parents `[tip, commit]` and the trailers
`Graft-Resolved-By: agent/<actor>` (`agent` without an identity) and, when the
resolver checked it, `Graft-Checked-By: <resolver>`. Without a resolver there
is no `Graft-Checked-By`: the resolution got only the conflict-marker rule and
the server's structural validation. The resolution travels in the request, so
the whole request is bounded by the 4 MiB gRPC message limit.

## Strategies of graft-resolve

`graft-resolve` tries an ordered list of strategies per file. The first
strategy whose result passes the configured checks wins. If none succeeds the
file is unresolved: the request is refused as a whole, with the outcome of
every file in the error response (in command mode the program exits 1, writes
the error response on stdout and the reason on stderr).

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
- Both modes answer the `check` operation of the protocol as well as
  `resolve` (see [Protocol v1](#protocol-v1)).
- Without `--config`, only the `trivial` strategy runs.
- `graft-resolve bench ...` measures a configuration on real merges (see
  [Measuring a resolver](#measuring-a-resolver)).

## AI resolvers

The AI setup is the `llm` strategy alone. The deterministic strategies above
(`trivial`, `union`, `rerere`, `command`) are opt-in additions in front of it,
not part of the default AI chain:

```toml
[[strategy]]
type = "llm"

[strategy.llm]
type = "openai"
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_API_KEY"
model = "openai/gpt-6-luna"
effort = "low"
max_tokens = 8000
timeout = "180s"
```

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

## Measuring a resolver

`graft-resolve bench` measures a `resolve.toml` on conflicts that humans
resolved in real repositories. It needs the grammars (a cgo build).

- `bench mine --corpus FILE --clones DIR --manifest OUT`: clones the
  repositories of the corpus (bare, complete clones), replays every two-parent
  merge with `git merge-tree --write-tree` on its parents, and keeps each
  conflicting file of a benchmark language whose human resolution differs from
  both sides. It leaves out a merge without a single merge base, and a human
  resolution that is not a valid reference: one committed with conflict
  markers, or one that does not parse although both sides do. Each
  repository is saved as soon as it is mined, so a stopped run resumes with
  the next one. The committed corpus
  (`tools/graft-agents/resolve/bench/corpus.json`) lists well-known
  repositories under permissive licenses at a pinned commit; the manifest
  holds object IDs, never third-party code.
- `bench filter`: applies the reference rule to a manifest mined before it.
- `bench syntax`: runs the syntax check on every human resolution and reports,
  per language, the files checked, those that fail, and those the grammar
  gives no verdict on, with the reason of each.
- `bench run --config resolve.toml --manifest M --clones DIR --sample N
  --seed S --max-usd USD --answers-dir A --json-out R`: replays a sample of the
  manifest through the configured chain, as the server would build the
  request (the never-send rules included), and judges each accepted result
  against the human file. The sample is stratified by family, with Rust and
  Java apart, so a language with few cases is taken whole. A chain with a
  model needs `--max-usd`: every call books its worst case first, and a call
  that would pass the cap is not made. The model's price is read at run time
  from OpenRouter's endpoints API (`--pricing openrouter`, the highest price
  of the model's endpoints), or given as flags (`--pricing fixed`). The
  answers are kept in `--answers-dir`.
- `bench rejudge --report-in R --manifest M --clones DIR --answers-dir A`:
  judges the stored answers again with the current judge, with no model call.

The outcomes of a case are: correct (equal to the human file after
normalizing line endings and trailing whitespace; exact when byte-identical),
wrong (accepted by the checks, but different), rejected by checks, not sent,
model error, not run (cap) and abstained (no strategy applied, such as the
`llm` strategy on a modify/delete conflict). The error rate is wrong /
(correct + wrong): the share of the results that would land and differ from
the human resolution. The report counts apart the wrong results that differ
in blank lines only, and the accepted results that got no syntax verdict.

## Bounds and failure modes

| Limit | Default | Where |
|-------|---------|-------|
| files per land | 20 | `max_files` |
| bytes per file | 1 MiB | `max_file_bytes` |
| resolver time | 120 s | `timeout` |
| resolver answer (stdout or HTTP body), and a refusal's error response | 2 × `max_file_bytes` × files in the request + 64 KiB | server |
| reason of a refusal (stderr, or `error` of the error response) | 4 KiB | server |
| check answer | 64 KiB + 8 KiB per file | server |
| verification run | 5 min, output kept 4 KiB | `verify.timeout` |
| request body read by `graft-resolve` | 128 MiB (`protocol.DefaultMaxRequestBytes`) | `--max-request-bytes` |
| land request with an agent's resolution | 4 MiB, the gRPC message limit | server; the SDK refuses larger ones |
| package: files, reasons, messages | `max_files`; 512 bytes; 4 KiB each | server |
| land attempts | 20 | `max_attempts` of the land |

A resolver that hangs is killed at the timeout. Its stderr is kept in the
logged error, which is truncated. An answer larger than its bound kills the
resolver and is a failure.

Before calling the resolver, the server also checks its own limits. A request
with more than `max_files` conflicted files, or from an actor that `actors`
does not allow, is not attempted at all. A file that breaks a per-file rule
(a side larger than `max_file_bytes`, a path denied by `paths_allow` /
`paths_deny`, the other rules of [What is never sent](#what-is-never-sent))
is not sent, and the other files are. Either way the land fails with the
conflict package unless every file is resolved.

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
  failure. Both end in the normal `merge_conflict` error, with the conflict
  package.

### graft-resolve subprocesses

Checks and `command` strategies run with a minimal environment too: `PATH`,
`HOME`, `TMPDIR`, `TEMP`, `TMP`, `LANG`, `LC_ALL`, `LC_CTYPE` and `SYSTEMROOT`,
plus the variables named in their `env` option. The provider keys in the
environment of `graft-resolve` therefore do not reach programs that run on
agent-influenced content. On timeout only the direct child is killed; see the
note on process groups above.
