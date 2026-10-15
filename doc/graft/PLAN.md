# Graft — plan for turning Gitaly into an agent-scale, server-centric VCS

Graft is an MIT-licensed fork of [Gitaly](https://gitlab.com/gitlab-org/gitaly)
that adopts the ideas Anthropic described for its internal "Scion" system
([Redesigning source control](https://www.anthropic.com/engineering/redesigning-source-control)):
always-online clients that fetch exactly what they need, history operations on
the server ("source control as an API"), landing changes without a merge queue,
a changeset log, derived data computed once and reused, and strict cache tiers.
Graft is not affiliated with Anthropic or GitLab. It stays Git-compatible: every
repository is a normal Git repository and every upstream RPC keeps working.

This document is the binding contract for the first milestone. Each work
package (WP) lists the files it owns. **Only edit the files your WP owns.**
If you need a change elsewhere, describe it in your report instead.

## Foundation (done, read-only for WPs)

- Protos (do NOT edit protos and do NOT run `make proto`; generated code in
  `proto/go/gitalypb` is current):
  - `proto/object.proto` — `ObjectService.GetObjects`, `ObjectService.GetTree`.
  - `proto/changeset.proto` — `ChangesetService.ListChangesets`,
    `ChangesetService.WatchChangesets`, message `Changeset`.
  - `proto/derived.proto` — `DerivedDataService.Blame`,
    `DerivedDataService.PathHistory`.
  - `proto/operations.proto` — `OperationService.UserLand` with
    `UserLandRequest` (+ `Strategy` enum), `UserLandResponse`, `UserLandError`.
- Service stubs registered in `internal/gitaly/service/setup/register.go`:
  `internal/gitaly/service/{object,changeset,derived}/server.go` (each has
  `server{deps *service.Dependencies}` embedding the Unimplemented server).
- Config: `internal/gitaly/config/config_graft.go` — `Cfg.Graft` (`[graft]`
  TOML section): `Standalone`, `Policy []GraftPolicyRule`, `Agents
  []GraftAgent` (TOML `[[graft.agent]]`), `ObjectService` (`MaxObjectsOrDefault`,
  `ChunkSizeOrDefault`), `Changelog.Disabled`, `Derived.Disabled`,
  `Derived.Eager`.
- `internal/graft/identity` — `identity.NewContext`, `FromContext`, `Actor`.
- `internal/graft/changelog/context.go` — `WithRewritten`, `IsRewritten`,
  `WithOperation`, `OperationFromContext` (gRPC method → `ff_branch`, `land`, ...).
- Praefect: `UserLand` is registered in `transactionRPCs` and
  `postReceiveHookRPCs` (`internal/praefect/coordinator.go`).
- `ee/` (GitLab EE licensed) was deleted; the fork is entirely MIT.

## Environment and commands (Windows host, code in WSL)

- Code: `\\wsl.localhost\Ubuntu\home\feder\scion\gitaly` (use this UNC path with
  Read/Edit/Write/Grep/Glob). Inside WSL it is `<repository>`.
- Run commands from Git Bash with the helpers (never `cd`, never pass quotes,
  pipes, `$`, `|`, `*` or parentheses as arguments — wsl re-parses them):
  - Tests: `MSYS_NO_PATHCONV=1 wsl -d Ubuntu -- bash <workspace>/gtest.sh ./internal/gitaly/service/object -r TestGetObjects,TestGetTree -v`
    (`-r` takes comma-separated top-level test names; omit to run the whole
    package). It serializes the shared build and is safe to run in parallel
    with other agents.
  - Anything else: `MSYS_NO_PATHCONV=1 wsl -d Ubuntu -- bash <workspace>/gx.sh go vet ./internal/gitaly/service/object/...`
    (`gx.sh go build ./...`, `gx.sh gofmt -l internal/graft`, `gx.sh git grep -n Foo -- internal`).
  - Files you write from Windows may get CRLF endings: after writing, run
    `gx.sh crlf <repo-relative paths...>`, then `gx.sh gofmt -l <dirs>`.
- PostgreSQL is not available: ~172 Praefect tests that need `PGHOST` fail on
  pristine upstream too. Ignore failures whose message is
  "PGHOST env var expected".
- Follow Gitaly conventions (`STYLE.md`, `doc/development.md`): `structerr`
  for gRPC errors, `testhelper.Context(t)`, `testcfg.Build`,
  `testserver.RunGitalyServer`, `gittest.WriteCommit` etc. Look at a sibling
  service's `testhelper_test.go` and copy its setup. Every RPC needs success,
  failure and validation tests. Never add new third-party dependencies.

## Work packages

### WP1 — Standalone mode (no GitLab)
Owns: `internal/gitlab/standalone_client.go` (+ `_test.go`), the client
selection in `internal/cli/gitaly/serve.go`, `validateGitlabSecret` (and any
GitLab URL validation) in `internal/gitaly/config/config.go`,
`internal/gitaly/config/config_graft_test.go`.
- `gitlab.NewStandaloneClient(cfg config.Graft, logger) gitlab.Client`:
  `Allowed` evaluates `cfg.Policy` (first rule whose `Actor` glob matches
  `params.GLID` and whose `Refs` globs match EVERY changed reference decides;
  a change set touching refs governed by different rules is denied unless all
  are allowed; empty Policy ⇒ allow; `**` suffix matches any depth), returning
  a clear message on denial; `PreReceive` ⇒ true; `PostReceive` ⇒ true with no
  messages; `ObjectPoolMembers` ⇒ empty map; `Check` ⇒ success with a
  descriptive `CheckInfo`.
- When `cfg.Graft.Standalone` is true: `serve.go` uses the standalone client
  (never contacts GitLab, ignores `gitlab.url`); config validation does not
  require a GitLab secret or URL. When false, upstream behaviour is unchanged
  (including `GITALY_TESTING_NO_GIT_HOOKS`).
- Tests: policy matching table (globs, `**`, multiple refs, deny precedence,
  empty policy), config validation in both modes, serve wiring if testable.

### WP2 — Agent identities and request defaults
Owns: `internal/gitaly/server/auth/**`, `internal/gitaly/server/server.go`
(interceptor wiring only), new `internal/graft/requestdefaults/**`.
- Per-agent tokens: when `cfg.Graft.Agents` is non-empty, the auth interceptor
  accepts a request if it carries a valid agent token (plain bearer token in
  the standard `authorization` metadata, compared in constant time; also
  accept the existing v2 HMAC token format computed with the agent token as
  secret) or the shared `auth.token` (if set); otherwise `Unauthenticated`.
  On success it stores `identity.Identity{Name: agent, Source: SourceAgent}`
  (or `SourceSharedToken` with name "shared") in the context. Agent `TokenFile`
  is read at startup. Health checks stay unauthenticated. When no agents are
  configured, upstream behaviour is unchanged.
- Request defaults interceptor (`requestdefaults`), active only when
  `cfg.Graft.Standalone` is true, for unary and streaming RPCs (for streams,
  wrap `RecvMsg`): using protoreflect on the request, fill the identity fields
  (`gl_id`/`gl_username` of every `gitaly.User` reachable from the request,
  including users nested in header messages and oneofs, and every string field
  named `gl_id`/`gl_username`) with the identity name (or "anonymous" without
  identity) when they are empty; for `SourceAgent` callers they are always
  overwritten with the agent name. `gl_repository` is filled with
  `"graft:" + storage_name + "/" + relative_path` only in the top-level
  `repository` field and is never overwritten. Absent messages are never
  created. With agents configured, agents get `PermissionDenied` on
  `gitaly.HookService`, on other administrative RPCs (see
  `internal/gitaly/server/auth/agent_methods.go`) and on any request carrying
  `git_config_options`.
- Tests: token matrix (agent ok, wrong token, shared token, none, health),
  identity in context, defaults filling for unary and stream requests.

### WP3 — UserLand RPC
Owns: `internal/gitaly/service/operations/land.go`, `land_test.go`.
Implements `UserLand` per the proto comment. Reuse existing building blocks:
quarantine directory like `UserRebaseConfirmable`/`UserFFBranch`,
`localrepo.IsAncestor`/merge-base helpers, `quarantineRepo.Rebase` (merge-tree
based; it drops merge commits — reject with `InvalidArgument` if the range
contains merges and strategy is rebase), `MergeTree` for STRATEGY_MERGE, and
`s.updateReferenceWithHooks(ctx, repo, user, quarantineDir, ref, new, old)`
which performs the compare-and-swap. Algorithm per attempt: resolve tip; if
missing → create when `create_branch` else NotFound; if tip == commit or tip is
ancestor → fast-forward; if commit is ancestor of tip → `already_landed`, no
update; else rebase/merge onto tip (FF_ONLY → `not_ancestor` error). Before the
update, if history was rewritten, use `changelog.WithRewritten(ctx)` for the
update call. On a CAS failure (`updateref.Error` / reference-state mismatch,
and the MVCC manifest conflict if applicable) retry from the top up to
`max_attempts` (default 20) with small jittered backoff; exhaustion →
`reference_update` detailed error. Merge conflicts → `merge_conflict` detailed
error (FailedPrecondition). Hook rejections → `custom_hook`/`access_check`
like `UserFFBranch`. Committer of rewritten commits: request user and
`timestamp` (or now).
- Tests: fast-forward, already landed, rebase onto diverged tip (linear result,
  content of both), merge strategy (two parents), FF_ONLY failure, conflict,
  create_branch, missing branch, validation errors, hook rejection, and a
  concurrency test: 10+ goroutines landing disjoint changes created from the
  same old tip on the same branch must all succeed, final tree contains all
  files, history linear.

### WP4 — ObjectService
Owns: `internal/gitaly/service/object/**`.
Implement `GetObjects` and `GetTree` per `proto/object.proto`, on the catfile
cache (`deps.GetCatfileCache()`, `ObjectReader`/`ObjectInfoReader` with
`Queue` for batching). `GetObjects`: validate repository, full-length OIDs of
the repo's object format, count ≤ `MaxObjectsOrDefault()`, resume fields;
stream chunks ≤ `ChunkSizeOrDefault()` bytes without loading huge objects
fully in memory if the reader allows streaming; dedupe OIDs preserving first
occurrence order; missing objects → `missing=true`. `GetTree`: by `tree_oid`
or `revision`+`path` (resolve `rev:path` via the object reader / `ls-tree`),
recursive or not, include sizes (batched info reads), stable depth-first
order, batch entries per message (e.g. 1000), paths relative to the listed
tree. Errors with `structerr` (InvalidArgument / NotFound).
- Tests: blobs/trees/commits, multi-chunk object with small chunk size,
  resume, missing objects, duplicates, limit, invalid OIDs, SHA256 object
  format (`gittest.ObjectHashIsSHA256` paths), GetTree by OID / revision /
  path / recursive / sizes / submodule entry / missing path.

### WP5 — Changeset log + ChangesetService
Owns: `internal/graft/changelog/**` (do not change the existing helpers'
semantics), `internal/gitaly/service/changeset/**`,
`internal/git/gitcmd/hooks_payload.go` (adding fields only),
`internal/gitaly/hook/referencetransaction.go`.
- Log storage: per repository, JSON Lines file `<repo git dir>/graft/changesets.jsonl`;
  each line one `Changeset` (sequence, reference, old/new OID, operation,
  actor, recorded_at RFC3339Nano, rewritten). Appends are serialized per repo
  path with an in-process mutex AND an `flock` on `<file>.lock`, fsync'd,
  sequences dense from 1 (derived from the last line; cache it). A torn last
  line (crash) is ignored and truncated on the next append.
- Recording: add `GraftOperation string`, `GraftActor string`,
  `GraftRewritten bool` (json omitempty) to `HooksPayload`, populated wherever
  the payload is built from a context (`changelog.OperationFromContext`,
  `identity.Actor(ctx, userDetails gl_id)`, `changelog.IsRewritten`). In
  `ReferenceTransactionHook`, when state is Committed and
  `!cfg.Graft.Changelog.Disabled`, parse the changes (old new ref per line;
  skip non-branch/tag internal refs such as `refs/keep-around/*`,
  `refs/tmp/*`, `refs/environments/*`, and symbolic HEAD updates) and append
  them. Recording errors are logged, never fail the update.
- `ChangesetService`: `ListChangesets` (cursor + limit, batches) and
  `WatchChangesets` (backlog then live; use an in-process broadcaster notified
  on append, plus a periodic re-read fallback every second; terminate on
  context cancellation).
- Tests: log append/read/torn-line/concurrency (50 goroutines ⇒ dense
  sequences), operation/actor/rewritten recorded through a real RPC
  (`UserCommitFiles` or `UserFFBranch` via testserver), List/Watch RPCs
  including live updates and cancellation.

### WP6 — Derived data + DerivedDataService
Owns: `internal/graft/derived/**`, `internal/gitaly/service/derived/**`,
`internal/gitaly/service/commit/last_commit_for_path.go` (integration only).
- Store: content-addressed, immutable entries under
  `<repo git dir>/graft/derived/<kind>/<first 2 hex of key>/<key>` written
  atomically (temp + rename), key = hex SHA-256 of (kind, commit OID, path).
- Kind `lastmod` (last commit that changed `path` as of `commit`), computed
  recursively from parents with memoization: if the path's tree entry OID at
  `commit` equals that at some parent (first such parent, like Git's history
  simplification) then `lastmod(commit) = lastmod(parent)`, else `commit`
  (also `commit` for a root commit where the path exists). Walk iteratively
  (explicit loop/stack, never recursion proportional to history), reading tree
  entries in batches via the catfile cache; store every computed value on the
  way so later queries are O(1).
- Kind `blame`: for (lastmod commit, path) store the parsed result of
  `git blame --porcelain` at that commit (keyed on the lastmod commit so all
  later revisions share it).
- RPCs: `Blame` (resolve revision → lastmod → cached blame; stream lines in
  batches); `PathHistory` (c1 = lastmod(rev, path); next = lastmod(first parent
  of c_i that is TREESAME... as defined above, or first parent) until limit or
  path absent; return `GitCommit`s via the existing catfile commit helpers).
- Integration: when `!cfg.Graft.Derived.Disabled`, `LastCommitForPath` uses
  `lastmod` instead of `git log -1` (identical output required).
- Tests: equivalence of lastmod with `git log -1 -- path` over linear, merge
  and rename/delete histories built with `gittest`; blame equivalence with
  `RawBlame`; cache hit (second call does not spawn git — count via a hook or
  check the store); PathHistory vs `git log --first-parent -- path`; a
  2,000-commit linear history without stack overflow.

### WP7 — Cache tier (`graft-cache`)
Owns: `cmd/graft-cache/**`, `internal/graft/cachetier/**`.
A gRPC server that sits between clients and Gitaly (per-node "daemon" tier or
regional "cache" tier; chain tiers by pointing one at another):
- Serves `ObjectService.GetObjects` from a byte-bounded in-memory LRU plus an
  optional on-disk store (atomic writes), keyed by
  (storage_name, relative_path, oid) — never shared across repositories.
  Misses are fetched from the upstream in ONE batched `GetObjects` call,
  verified (recompute the Git object ID from type+size+content for SHA1 and
  SHA256 and compare), stored, then streamed to the client in request order
  with the same chunking/resume semantics. Concurrent misses for the same key
  are coalesced.
- Serves `ObjectService.GetTree` from cache only when `tree_oid` is set
  (immutable); otherwise forwards.
- Every other RPC (all services) is proxied transparently to the single
  upstream using the existing `proxy` package (as Praefect does with
  `UnknownServiceHandler`), forwarding metadata (including authorization).
- Exposes gRPC health, Prometheus metrics (hits, misses, bytes, evictions) and
  a `--stats` JSON endpoint on an optional HTTP address.
- `cmd/graft-cache`: flags `--listen` (tcp or unix), `--upstream`,
  `--tier cache|daemon`, `--max-bytes`, `--disk-dir`, `--metrics-addr`,
  `--token` (sent upstream if the client sent none).
- Tests: against an in-process Gitaly (`testserver`) or a fake upstream:
  hits/misses, coalescing (upstream called once), verification failure,
  eviction, per-repo isolation, pass-through of another RPC (e.g.
  `RepositoryService.RepositoryExists` and `OperationService.UserCommitFiles`
  streaming), chained tiers.

### WP8 — Public Go SDK (`sdk/`)
Owns: `sdk/**` (new top-level package `github.com/feder-cr/open_scion/sdk`;
may import `client`, `auth`, `proto/go/gitalypb` but NOT `internal/` in
non-test code).
`sdk.Dial(ctx, address, opts...)` (`WithToken`, `WithAgentToken`,
`WithStorage`), `Client.Repo(relativePath) *Repo`, and on `Repo`:
`ReadFile(ctx, rev, path) ([]byte, error)`, `ListTree(ctx, rev, path,
recursive)`, `GetObjects(ctx, oids)` (with automatic resume),
`CommitFiles(ctx, CommitOptions{Branch, StartRev, Message, Author, Actions:
[]Action{Put/Delete/Move/Chmod}})` (builds the `UserCommitFiles` stream),
`Land(ctx, LandOptions)`, `Blame`, `PathHistory`, `Changesets(ctx, after,
limit)`, `WatchChangesets(ctx, after) (<-chan Changeset, <-chan error)`,
`ResolveRevision`. Errors expose the gRPC code and detailed errors
(`UserLandError` etc.) through typed helpers.
- Tests: integration tests that start a real Gitaly via `internal/testhelper`
  (allowed in `_test.go`) and exercise every method end to end.

### WP9 — Agent CLI (`graft`)
Owns: `cmd/graft/**`, `internal/graft/workspace/**`.
A sparse, server-centric working copy for agents, built on `sdk`:
`graft clone ADDR REPO DIR [--branch main] [--sparse a/,b/]` (materialize via
`GetTree` recursive + batched `GetObjects`, local object cache under
`.graft/objects`, state in `.graft/state.json`), `graft status`, `graft diff`,
`graft commit -m MSG [--land BRANCH]` (computes actions from status, calls
`CommitFiles` with start = base commit on a temporary branch
`refs/heads/graft/<user>/<random>` then `Land`), `graft land`, `graft update`
(refuses if local changes touch updated files), `graft log` (changeset log),
`graft watch`, `graft blame PATH`, `graft history PATH`, `graft cat REV:PATH`,
`graft sparse PREFIX...`. Config via flags/env `GRAFT_ADDRESS`, `GRAFT_TOKEN`,
`GRAFT_STORAGE`, `GRAFT_AUTHOR`. Path safety: validate every server path and
never write outside the workspace root.
- Tests: workspace tests against an in-process Gitaly; CLI `run(args, stdout,
  stderr) int` tests.

### WP10 — Product docs, packaging, end-to-end
Owns: `README.md` (add a Graft section at the top, keep upstream content
below), `doc/graft/**` except this file, `examples/graft/**`,
`internal/graft/e2e/**`.
- Docs: what Graft is/is not, architecture (ASCII), how each Scion idea maps
  to Graft, quickstart (standalone config, agents, `graft-cache` tiers, `graft`
  CLI), configuration reference for `[graft]`, limitations and roadmap.
- `examples/graft/config.toml` (standalone, two agents, policy) and
  `examples/graft/quickstart.sh`.
- `internal/graft/e2e`: an end-to-end test that starts standalone Gitaly with
  agents + policy, a daemon tier chained to a cache tier, then via the SDK:
  create repo, commit, 20 concurrent agents land disjoint changes, read
  through the daemon (cache hits > 0), blame/history, changeset log contains
  all 20 lands with actors, policy denial for a protected branch.

## Milestone acceptance
`gx.sh go build ./...`, `gx.sh go vet` on all touched packages, gofmt clean,
`gtest.sh` green for every touched package (except PGHOST-only failures),
`make lint-proto` clean, and the WP10 end-to-end test green.
