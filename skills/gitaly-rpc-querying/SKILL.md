---
name: gitaly-rpc-querying
description: Use when calling or inspecting the gRPC API of a running Gitaly or Praefect locally — listing and describing services with grpcurl, minting the V2 auth token, invoking an RPC or exploring it in grpcui, and optionally building a protoset with make build-protoset.
---

# Querying Gitaly RPCs locally

Read [doc/invoking_grpc_endpoints_locally.md](../../doc/invoking_grpc_endpoints_locally.md) first: it
carries every command, and the socket address form, the token scheme, and the JSON encoding of `bytes`
fields are each easy to get wrong by guessing.

The workflow it documents:

1. **Find the socket** — `socket_path` in the GDK's `gitaly/gitaly.config.toml`, or in one of the
   `gitaly/gitaly-*.praefect.toml` files when going through Praefect.
1. **Mint a V2 token**, but only if `token` is set in the `[auth]` section of that same file.
1. **Discover the method** by listing and describing services. This works through reflection
   against a running server, so a protoset is optional.
1. **Call it**, starting with `gitaly.ServerService.ServerInfo` to prove out the socket and token.
