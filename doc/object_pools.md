# Object Pools

When creating forks of a repository, most of the objects for forked repository
and the repository it forked from are shared. Storing those shared objects
multiple times is a waste of disk space and also of CPU time, given that those
shared objects would have to be repacked for both repositories. To fix this
waste of resources, we use object pools, which are essentially a repository
which holds the shared objects of both repositories.

The sharing of objects for a given repository and its object pool is done via
alternate object directories which Gitaly sets up when linking a repository to
an object pool by writing the `objects/info/alternates` file.

## Lifetime of Object Pools

The lifetime of object pools is maintained via the
[ObjectPoolService](../proto/objectpool.proto), which provides various RPCs to
create and delete object pools as well as to add members to or remove members
from the pool.

An object pool is typically created from an existing repository by doing a
[`--local`](https://git-scm.com/docs/git-clone#Documentation/git-clone.txt---local)
clone of the repository, which bypasses the normal transport mechanisms and
instead simply performs a copy of the references and objects.

Afterwards, any repositories which shall be a member of the pool needs to be
linked to it. Linking most importantly involves setting up the "alternates" file
of the pool member, but it also includes deleting all bitmaps for packs of the
member. This is required by Git because it can only ever use a single bitmap.
While it's not an error to have multiple bitmaps, Git will print a
[user-visible warning](https://gitlab.com/gitlab-org/gitaly/-/issues/1728) on clone or fetch
if there are. See [`git-multi-pack-index(1)`](https://git-scm.com/docs/multi-pack-index#_future_work)
for an explanation of this limitation.

Removing a member from an object pool is slightly more involved, as members of
an object pool members will miss objects which are only part of the object pool.
It is thus not as simple as removing `objects/info/alternates`, as that would
leave behind a corrupt repository. Instead, Gitaly hard-links all objects which
are part of the object pool into the dissociating member first and removes the
alternate afterwards. In order to check whether the operation succeeded, Gitaly
now runs `git-fsck(1)` to check for missing objects. If there are none, the
dissociation has succeeded. Otherwise, it will fail and re-add the alternates
file.

## Housekeeping

Housekeeping for object pools is handled differently from normal repositories as
it not only involves repacking the pool, but also updating it. The housekeeping
task is thus hosted by the `FetchIntoObjectPool` RPC. This task is typically
only executed with the original object pool member from which the pool has been
seeded and updates the pool by fetching from that member.

It performs the following tasks:

1. Common housekeeping tasks are performed. These are common cleanups which are
   shared between object pools and normal repositories. Most importantly, it
   removes stale lockfiles and deletes known-broken stale references.

1. A fetch is performed from the object pool member into the object pool with a
   `+refs/*:refs/remotes/origin/*` refspec. This fetch is most notably not a
   pruning fetch, that is any reference which gets deleted in the member will
   stay around in the pool.

1. The fetch may create new dangling objects which are not referenced anymore in
   the pool repository. These dangling objects will be kept alive by creating
   dangling references such that they do not get deleted in the pool. See
   [Dangling Objects](#dangling-objects) for more information.

1. Loose references are packed via `git-pack-refs(1)`.
1. The pool is repacked via `git-repack(1)`. The repack produces a single packfile
   including all objects with a bitmap index. In order to improve reuse of
   packfiles where Git will read data from the packfile directly instead of
   generating it on the fly, the packfile uses a delta island including
   `refs/heads` and `refs/tags`. This restricts Git to only generate deltas for
   objects which are directly reachable via either a branch or a tag. Most
   notably, this causes us to not generate deltas against dangling references.

## Dangling Objects

When fetching from pool members into the object pool, then any force-updated
references may cause objects in the pool to not be referenced anymore. For
normal repositories, it is perfectly fine to delete those references after a
certain time. In the context of object pools, any other member of the pool may
still use any of those unreferenced objects. Deleting them would thus
potentially cause corrupt repositories.

This issue is kind of unsolvable: there is no point in time where it's safe to
delete objects from the object pool, as we do not know which repositories may be
linked to it. And even if we knew, we cannot determine all references of all
repositories at once in a race-free manner. We thus must consider each object to
still be referenced somewhere.

As a safeguard to not lose any objects by accident, we thus create dangling
references in the object pool after the fetch in `FetchIntoObjectPool`. For each
dangling object, a reference `refs/dangling/$OID` is created which points into
the object. This assures that each object is still referenced.

Having unreachable objects kept alive in this fashion does have its problems:

- For busy repositories, we generate loads of dangling references. While these
  references [cannot be seen by clients](#references), they are seen when
  performing housekeeping tasks on the object pool itself. Fetches into the
  object pool and repacking of references can thus become quite expensive.

- Keeping dangling references alive makes Git consider them as reachable. While
  this is the exact effect we want to achieve, it will also cause Git to
  generate packfiles which may use such objects as delta bases which would under
  normal circumstances be considered as unreachable. The resulting packfile is
  thus potentially suboptimal. Gitaly works around this issue by using a delta
  island for `refs/heads/` and `refs/tags/`. This can only be considered a
  best-effort strategy, as it only considers a single object pool member's
  reachability while ignoring potential reachability by any other pool member.

## References

When Git repositories have alternates set up, then they by default advertise any
references of the alternate itself. A client would thus typically also see both
dangling references as well as any other reference which was potentially already
deleted in the pool member which the client is fetching from. Besides being
inefficient, the resulting references would also be wrong.

To avoid advertising of such references, Gitaly uses a workaround of setting the
config entry `core.alternateRefsCommand=exit 0 #`. This causes Git to use the
given command instead of executing `git-for-each-ref(1)` in the alternate and thus
stops it from advertising alternate references.

## Placement and co-location across storages

Because a member shares objects with its pool through the `objects/info/alternates`
file (a filesystem link resolved relative to the member's `objects/` directory),
a member and its object pool **must live on the same storage**. If they end up on
different storages, the alternate dangles and the member is left with missing
objects. Anything that relocates a repository between storages therefore has to
keep the pool and its members together.

### During a repository move

When a repository is moved between storages (the project / wiki / snippet
repository storage-move flow), the pool is handled before the member. The pool is
established or located on the destination first, and only then is the member
placed alongside it. The member's move is a `ReplicateRepository` that carries the
pool's location so the destination places the two together. This is the same
requirement described under [Lifetime of Object Pools](#lifetime-of-object-pools).
A member must not exist without a reachable pool.

On a single Gitaly node (sharded, no Praefect), this is straightforward. The
destination is one storage, so the pool and the member both land there.

### Co-location on Gitaly Cluster (Praefect)

On a Praefect cluster, a repository is not on a single node but replicated
across a subset of nodes, so "same storage" has to be resolved to "the same set
of Gitaly nodes the pool is on." The member's `ReplicateRepository` has no
explicit additional-repository field. The member is not linked to its pool until
later (the pool and the member are replicated as two independent calls), so at
replication time the request has no pool operand to carry, and only the caller
knows the member is destined for the pool. The pool's location is therefore
supplied out-of-band as a partitioning hint (the pool's relative path) instead of
a request field; see commits `b2b776ec8` and `574f2747d` for the rationale.
Praefect's
repository-creation routing resolves that hint against its metadata store (a
database lookup, with no disk access) and assigns the new member to the pool's
own nodes:

- The member's primary becomes the pool's primary.
- The member is assigned to **all of the pool's assigned replica storages**.
  Those that are currently healthy and on the pool's latest generation are
  created synchronously as secondaries; any that are unhealthy or on an older
  generation are still assigned to the member, but recorded as outdated and
  populated asynchronously by replication.

Health and generation therefore decide only whether a given replica is filled
synchronously or asynchronously, not whether the member is assigned there. The
member always ends up assigned to the same node set as the pool.

A pool member will live on the same node as its pool regardless of the replication factor,
even when the replication factor is smaller than the number of nodes in the virtual storage.
While a freshly created repository could otherwise be assigned to an arbitrary subset,
a pooled member is instead pinned to the pool's node set.

- The replication factor decides how many replica nodes.
- The partitioning hint decides the which nodes to use.

### Safe failure

Routing fails the move (rather than placing the member uncolocated) in two
cases:

- The pool is not present on the destination when the member is being placed.
  Routing cannot resolve the hint and creation fails with `additional
  repository not found`.
- The pool's primary node is currently unhealthy. Because the member's primary
  must be the pool's primary, routing fails with a "primary not healthy" error.
  (An unhealthy or outdated *secondary* does not fail the move; it is assigned
  as an outdated replica and filled asynchronously, as described above.)

In both cases the member stays on its source storage. This is a safe abort: the
move does not fall back to placing the member without its pool, so it never
produces an uncolocated (and therefore corrupt) member. Re-running the move once
the pool is present on the destination, and its primary is healthy, succeeds.

## Further Reading

- [How Git object deduplication works in GitLab](https://docs.gitlab.com/development/git_object_deduplication/)
