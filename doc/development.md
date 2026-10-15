# Development

This document provides an overview of how to work in the Gitaly codebase. This includes a high-level overview of the repository layout, important Make targets, and various coding conventions we should use.

More specific guidance is available in the documents linked to from here.

[STYLE.md](contributing/STYLE.md) defines general Go style and conventions.

## Repository map

- `proto/` — Protobuf service/RPC definitions (`*.proto`) and generated Go in `proto/go/gitalypb/`. Refer to [protobuf.md](protobuf.md) for more detailed proto design rules.
- `internal/gitaly/service/<svc>/` — RPC handlers, one method per file with a companion `_test.go`.
- `internal/grpc/middleware/` — gRPC interceptors (logging, concurrency limits, panic recovery, request info, observability, …); individual packages may also define their own `middleware.go`.
- `internal/git/gitcmd/` — the Git command factory; spawn Git through this, never `exec` directly.
- `internal/structerr/` — structured error framework (gRPC codes, metadata, rich error details).
- `internal/testhelper/` — test setup (context, servers, repos, leak detection); `gittest/` builds test data.
- `internal/praefect/` — Praefect cluster coordination, routing, transactions.
- `cmd/` — entrypoints (`gitaly`, `praefect`, `gitaly-backup`, helper binaries).
- `client/` — Go client library for talking to Gitaly.
- `_support/` — dev/CI scripts (migrations, schema generation, custom test runners).
- `tools/` — Go tooling, including the custom `golangci-lint` plugin in `tools/golangci-lint/gitaly/`.

## Make targets

Everything runs through `make`, which sets up `gotestsum`, packed binaries, the linter plugin,
and an isolated test environment.

### Build

- `make build` — build binaries (also builds bundled Git; first run is slow).
- `make proto` — regenerate Go code after editing any `.proto`; commit the generated code with it.

### Test

- `make test` — run the Go test suite (alias for `test-go`).
- Targeted tests:
  `TEST_PACKAGES=./internal/gitaly/service/foo TEST_OPTIONS="-count=1 -run=TestName" make test`
- `make race-go` — race detector
- `make debug-test-go` — delve-based debugging.

### Verify

- `make format` — format code (runs gci/gofmt/gofumpt/goimports via golangci-lint).
- `make lint` / `make lint-fix` — lint, and lint with autofixes.
- `make verify` — pre-push gate: mod-tidy, NOTICE, proto check, and lint.
- `make lint-docs` / `make lint-docs-fix` — markdownlint + vale on `README.md`, `REVIEWING.md`,
  `STYLE.md`, and `doc/`.
