# MVCC RPC flow (superseded)

The request flow described in earlier revisions of this document (the POC `BeginMVCCWrite`
session, the S3 pointer and the change proposals) no longer describes the code. The MVCC cache
still exists, but how repository state is stored and kept in sync across Gitaly nodes is now
documented in the storage core docs, which are the reference:

- `doc/graft/STORAGE.md`: architecture, modes, configuration and operations of the storage core.
- `internal/git/mvcc`: the manifest format and the local MVCC cache.
