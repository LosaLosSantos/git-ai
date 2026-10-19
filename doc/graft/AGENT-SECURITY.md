# Agent security model

Agents are automated writers. They are useful only if they can land changes
without a human, so Graft gives them the least authority that still lets them
work. This document lists what an agent can and cannot do, how it is
identified, and which settings control it.

## Identities

- Each agent has its own token, configured as a `[[graft.agent]]` entry with a
  `token_file`. The token is compared in constant time. The v2 HMAC token
  format, computed with the agent token as the secret, is accepted too.
- A request with a valid agent token runs as that agent
  (`identity.SourceAgent`). The agent name is the actor recorded in the
  changelog, in policy decisions and in trailers.
- The shared token (`auth.token`) is an operator credential. It is not an
  agent. Setup calls of tools and the verifier use it.
- Health checks need no token.
- With no agents configured, the server behaves as upstream Gitaly.

Agent identity is taken from the token, never from the request. The request
defaults interceptor overwrites `gl_id` and `gl_username` fields with the
agent's name for agent callers. An agent cannot impersonate another actor by
putting a different name in the request.

## What an agent may call

Agents may call only the RPCs on the built-in allowlist, plus any RPCs listed
in `graft.agent_rpcs`. The allowlist is what the SDK, the `graft` CLI and the
cache pass-through use for agent work:

- object and tree reads (`ObjectService`);
- changesets (`ChangesetService`, read only);
- derived data (`DerivedDataService`);
- code search (`SearchService`, read only, used by `graft search`);
- reviews (`ReviewService`, used by `graft review`). Its mutators write only
  the review store of the repository, never references, and record the
  token's identity as the actor, so an agent cannot approve as another agent;
- landing and commits: `UserLand`, `UserCommitFiles`, `UserCreateBranch`,
  `UserDeleteBranch` (through policy);
- read accessors of the commit, ref, blob, tree and diff services
  (`Find*`, `List*`, `Get*`), plus `CommitService.TreeEntry` and
  `DiffService.DiffStats` (used by the SDK and `graft show`);
- `ServerService.ServerInfo`, `RepositoryService.RepositoryExists` (which
  `graft-cache` uses to authorize a caller for a repository) and the gRPC
  health service.

Explicitly not allowed for agents, even though upstream Gitaly offers them.
These can never be enabled, not even through `graft.agent_rpcs`:

- `RepositoryService` mutators: `CreateRepository`, `CreateFork`,
  `CreateRepositoryFrom*`, `BackupRepository`, `RestoreRepository`,
  `RemoveRepository`, `ReplicateRepository`, `WriteRef`, `FetchRemote`,
  `FetchSourceBranch`, `FetchBundle`, `SetCustomHooks`, `RestoreCustomHooks`,
  `OptimizeRepository`, `PruneUnreachableObjects`.
- `RefService` `UpdateReferences` and `DeleteRefs`.
- `RemoteService` `UpdateRemoteMirror`, `FindRemoteRepository` and
  `FindRemoteRootRef`, which push to or query a URL the caller chooses.
- `CleanupService`, `ObjectPoolService`, `HookService`, `InternalGitaly`,
  `PartitionService`.

An agent does not see `refs/graft/` at all (`internal/graft/agentrefs`): the
agent guard refuses a request whose revision, reference or pattern names a
reference under it (including the short forms `graft/proposals/...` and
`graft/green/...`, which git completes to the server's namespaces; other
`graft/...` short names, such as the workspace's temporary branches, are
branches), and removes such references from every response; git upload-pack and
receive-pack hide the prefix from agents. A test classifies every string or
bytes field of every RPC agents may call by default, so a new field cannot
escape the filter; RPCs added with `graft.agent_rpcs` are checked by field
name. Other callers still see graft-verify's green references, which are
documented for consumers.

Whatever the RPC and whatever the policy says, an agent never creates,
updates or deletes a reference under `refs/graft/`: the reference-transaction
hook refuses it, so a push, `UpdateReferences`, `DeleteRefs`, `WriteRef` or an
operation RPC that would do so fails. The namespace is the server's: the
proposals of a land (`refs/graft/proposals/`, hidden from advertisement and
push, written by the server) and graft-verify's green references (written with
the shared token).

Every other RPC that is not on the allowlist (for example the other
`OperationService` RPCs such as `UserMergeBranch`, or `SSHService`) is denied
by default and can be enabled with `graft.agent_rpcs`.

A call outside the allowlist fails with `PermissionDenied`. The error does
not reveal more than that the method is not allowed for agents.

### Extending the allowlist (`agent_rpcs`)

```toml
[graft]
agent_rpcs = ["/gitaly.RepositoryService/RepositorySize"]
```

Each entry is a full gRPC method name. The server checks the list at startup
and refuses to start when an entry is not a registered method or is one of the
methods above that agents may never call. The list applies to every agent;
there are no per-agent allowlists.

`SSHService` is not on the built-in allowlist, so agents cannot fetch or push
over the Git protocol through Gitaly or through the `graft-cache`
pass-through. To allow it, list
`/gitaly.SSHService/SSHUploadPackWithSidechannel` (fetch) and
`/gitaly.SSHService/SSHReceivePack` (push). Pushes are still checked by the
branch policy, but they bypass the `UserLand` path, its retries and its
conflict resolution.

The test `TestAgentCannotCreateRepositoryFromSnapshot` and the phase-2
allowlist test check this from the outside. An exhaustive table test checks
that every registered RPC is either allowlisted or denied for agents, so a new
RPC is denied until someone decides otherwise.

## Requests that are always refused for agents

- `gitaly.HookService` calls.
- Administrative RPCs (see `internal/gitaly/server/auth/agent_methods.go`).
- Any request that carries `git_config_options`. Such options could change how
  Git runs on the server.
- Any request that carries `gitaly-servers` metadata. It names the address
  and token of other Gitaly servers, which RPCs such as `UserCommitFiles` with
  a `start_repository` on another storage dial and fetch from. An agent could
  otherwise make the server connect to any address.

## Branch policy

Policy rules (`[[graft.policy]]`) decide which actors may update which
references. Each rule has an actor glob and reference globs. The first rule
whose actor matches and whose refs match every changed reference decides.
Changes that touch references governed by different rules are denied unless
all are allowed. `**` as a suffix matches any depth.

Policy covers the `HEAD` symbolic ref as well, so an agent cannot repoint
`HEAD` to bypass a protected branch.

A denial by policy is a `PermissionDenied` error. Standalone denial messages
do not start with `GitLab:`, so they are not mistaken for a GitLab hook
rejection.

## Resolution and verification are opt-in

- Automatic conflict resolution is off by default and can be limited to
  specific actors and paths (see [RESOLVE](RESOLVE.md)).
- The verifier writes the green ref and reverts with the operator credential,
  not an agent credential (see [VERIFY](VERIFY.md)).
- No Graft server component calls a language model. AI resolvers and
  summarizers run in `tools/graft-agents` behind a vendor-neutral interface.

## Operator checklist

1. Give every agent its own token file, readable only by the agent's account.
2. Keep `auth.token` for operators and tooling, and do not give it to agents.
3. Protect default and release branches with policy rules.
4. Enable resolution only for actors and paths you trust.
5. Keep `graft-resolve` and the verifier on a separate account when possible.
6. Review changelog entries with `graft log`, which shows the actor of every
   change.
