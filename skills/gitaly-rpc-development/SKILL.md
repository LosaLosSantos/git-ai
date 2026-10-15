---
name: gitaly-rpc-development
description: Use when adding or changing a Gitaly gRPC RPC — editing .proto files, setting RPC annotations, regenerating code with make proto, implementing the handler, and writing tests. Covers the full end-to-end flow and the non-obvious rules.
---

# Adding or changing a gRPC RPC

The end-to-end workflow for adding or changing a Gitaly RPC.

Gitaly RPCs span three layers: the `.proto` definition, generated Go, and a handler in
`internal/gitaly/service/`. Getting the proto annotations or regeneration step wrong breaks
Praefect routing or CI. Follow this order.

## 1. Define or change the RPC in proto

Service definitions live in `proto/<service>.proto`. Each RPC needs a request and response
message named after it (`FooBar` → `FooBarRequest`, `FooBarResponse`).

```proto
// FetchRemote fetches references from a remote repository into the local repository.
rpc FetchRemote(FetchRemoteRequest) returns (FetchRemoteResponse) {
  option (op_type) = {
    op: MUTATOR
  };
}
```

Rules:

- **Document every service, RPC, message, and field** in comments — enough that a caller need
  not read the implementation. Document non-obvious error cases.
- **Name RPCs verb-first:** `GetCommit`, `ListRefs`, `IsRepositoryEmpty`, `CreateRepositoryFromBundle`.
  Operation RPCs match the Git verb (`FetchRemote`).
- **Keep messages flat.** Abstract only for real, repeated benefit.
- **Never make backwards-incompatible changes** to an existing RPC or message. Add a new RPC and
  deprecate the old one (see [doc/PROCESS.md](../../doc/PROCESS.md)).
- **Pick `string` vs `bytes` by encoding.** A Protobuf `string` must be valid UTF-8, but Git does
  not enforce encodings, so most Git data is arbitrary bytes. Use `bytes` for anything Git controls
  (`revision`, `ref`, `path` inside a repo). Use `string` only for values Gitaly produces that are
  guaranteed UTF-8/ASCII, such as `relative_path` (a path on Gitaly's disk) or `commit_id` (a hex
  object ID).
- **Stream unbounded or oversized payloads.** Use a `stream` when the result is an unbounded
  sequence (a `repeated` collection that can grow without limit) or a payload that could exceed
  gRPC's message-size limit, sending it in batches/chunks. For high-volume byte payloads (e.g. Git
  fetch/pack data), Gitaly avoids gRPC messages entirely and uses a **sidechannel**
  ([doc/sidechannel.md](../../doc/sidechannel.md)); the full streaming patterns are in
  [doc/protobuf.md](../../doc/protobuf.md).

## 2. Set the RPC annotations (required)

Gitaly's middleware reads these annotations (via the proto registry) to locate the target
repository/storage, invalidate caches, apply concurrency limits, and coordinate transactions;
Praefect additionally uses them to route and replicate requests across nodes. Missing or wrong
annotations is a common failure.

- **Operation type** — every RPC: `op: ACCESSOR | MUTATOR | MAINTENANCE`.
  - `ACCESSOR`: read-only, no side effects.
  - `MUTATOR`: modifies repository state.
  - `MAINTENANCE`: best-effort upkeep, non-critical.
- **Scope** (mutators): `option (op_type).scope` of `SERVER | STORAGE | REPOSITORY`.
  `REPOSITORY` is the default and needs a repository field tagged `[(target_repository)=true]`:

  ```proto
  message FetchRemoteRequest {
    Repository repository = 1 [(target_repository)=true];
    // ...
  }
  ```

  `STORAGE` scope needs a storage field tagged `[(storage)=true]`.
- **Mutators must vote transactionally**: compute a deterministic vote over all written data,
  with at least a preparatory and a committing vote, so replicas stay consistent. Keep errors
  deterministic for the same input.

See [doc/protobuf.md](../../doc/protobuf.md) for the full annotation and streaming reference.

## 3. Regenerate code

```shell
make proto
```

This regenerates Go (`proto/go/gitalypb/`) and Ruby. **Commit the generated code together with the
`.proto` change** — CI's `check-proto` fails otherwise.

## 4. Implement the handler

Handlers live at `internal/gitaly/service/<service>/<rpc>.go` as methods on the service's
`*server`, with a companion `<rpc>_test.go`.

- Validate the request first; return `structerr.NewInvalidArgument(...)` for bad input.
- Spawn Git through `internal/git/gitcmd` (the command factory) with long flags and `flag=value` syntax.
- Use context-based logging: `log.DebugContext(ctx, ...)`.
- Return errors from `internal/structerr` (`NewNotFound`, `NewFailedPrecondition`, `NewInternal`, …),
  wrapping causes with `%w`. Avoid `Unavailable` except for retriable network failures.
- For rich, client-parseable errors, attach a proto error-details message (RPC `Frobnicate` → `FrobnicateError`
  with a `oneof`).

## 5. Test end to end

Write table-driven tests covering success, failure, and validation. Generate all test data at
runtime with `gittest` helpers (`gittest.CreateRepository`, `WriteCommit`, `WriteBlob`, `WriteRef`,
`WriteTag`, …) — never hardcode object IDs. See [doc/development.md](../../doc/development.md) for
test conventions and how tests run. Run the package you touched:

```shell
TEST_PACKAGES=./internal/gitaly/service/<service> TEST_OPTIONS="-count=1 -run=TestFooBar" make test
```

## Before you finish

```shell
make proto     # if any .proto changed (and commit generated code)
make format
make verify    # pre-push gate: mod-tidy, NOTICE, proto check, lint
make test
```
