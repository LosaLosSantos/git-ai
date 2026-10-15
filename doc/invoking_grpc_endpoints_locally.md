# Invoking gRPC endpoints locally

How to call the gRPC API of a Gitaly or Praefect running on your machine, using
[`grpcurl`](https://github.com/fullstorydev/grpcurl) and [`grpcui`](https://github.com/fullstorydev/grpcui).

## 1. Find the socket

Gitaly listens on a UNIX socket when run from the GDK. Read `socket_path` from
`gitaly/gitaly.config.toml`, or from one of the `gitaly/gitaly-*.praefect.toml` files when going
through Praefect.

Both tools take the socket as a `unix://` address with an absolute path.

## 2. Mint a token, if the server wants one

Authentication is enforced only when `token` is set in the `[auth]` section of that same file. When it
is unset, drop the `-H` flag from every command below.

When it is set, the request needs a V2 bearer token in the `authorization` metadata field. A V2 token
is `v2.<signature>.<timestamp>`, where `timestamp` is the current Unix time and `signature` is its
HMAC-SHA256 digest keyed with the shared secret, hex-encoded. It is valid for 30 seconds either side
of the timestamp, so mint it immediately before each call:

```shell
timestamp=$(date +%s)
signature=$(printf '%s' "${timestamp}" | openssl dgst -sha256 -hmac "<shared secret>" -hex | awk '{print $NF}')
GITALY_TOKEN="v2.${signature}.${timestamp}"
```

V2 is the only scheme the server accepts. For the authoritative implementation, see
[`auth/token.go`](../auth/token.go) and [`auth/README.md`](../auth/README.md).

## 3. Discover the method you need

Gitaly and Praefect both register the gRPC server reflection service, so a protoset is optional.
Against a running server, discovery works without one:

```shell
grpcurl -plaintext -H "authorization: Bearer ${GITALY_TOKEN}" unix://<socket> list
grpcurl -plaintext -H "authorization: Bearer ${GITALY_TOKEN}" unix://<socket> list gitaly.RefService
grpcurl -plaintext -H "authorization: Bearer ${GITALY_TOKEN}" unix://<socket> describe gitaly.FindDefaultBranchNameRequest
```

When you know roughly what a method is called but not which service holds it, list the methods of
every service and search them. There are only a couple of dozen services, so this is quick:

```shell
for service in $(grpcurl -plaintext -H "authorization: Bearer ${GITALY_TOKEN}" unix://<socket> list); do
  grpcurl -plaintext -H "authorization: Bearer ${GITALY_TOKEN}" unix://<socket> list "${service}" 2>/dev/null
done | grep -i <term>
```

Build a protoset when you want to work without reflection: to list and describe the API with no
server running, or to test against a server whose reflection service you cannot reach. Pass
`-protoset` to any command in place of, or alongside, the address:

```shell
make build-protoset   # writes _build/protoset/gitaly.protoset
grpcurl -protoset _build/protoset/gitaly.protoset describe gitaly.RepositoryService.RepositoryInfo
```

## 4. Call it

`gitaly.ServerService.ServerInfo` takes an empty request, which makes it the quickest way to confirm
that the socket and token work before moving on to a real RPC:

```shell
grpcurl -plaintext -H "authorization: Bearer ${GITALY_TOKEN}" \
  -d '{}' unix://<socket> gitaly.ServerService.ServerInfo
```

Repository-scoped RPCs need `storage_name` plus `relative_path`, the repository's path relative to the
storage root:

```shell
grpcurl -plaintext -H "authorization: Bearer ${GITALY_TOKEN}" \
  -d '{"repository":{"storage_name":"default","relative_path":"@hashed/ab/cd/abcdef.git"}}' \
  unix://<socket> gitaly.RefService.FindDefaultBranchName
```

For a browsable form of the same API, open the web GUI:

```shell
grpcui -plaintext -H "authorization: Bearer ${GITALY_TOKEN}" -protoset _build/protoset/gitaly.protoset unix://<socket>
```

## Notes

- `bytes` fields (revisions, ref names, paths inside a repository) are base64 in JSON, in requests and
  responses alike — a returned `"name": "cmVmcy9oZWFkcy9tYXN0ZXI="` is `refs/heads/master`. `string`
  fields such as `storage_name` and `relative_path` are passed as is.
- For client-streaming RPCs, pass `-d @` and write the request messages as a sequence of JSON
  documents on stdin.
- `describe` gives you a field's name and type, not its meaning. Units and semantics live in the
  comments in `proto/<service>.proto` — `RepositorySizeResponse.size`, for instance, is in kilobytes.
