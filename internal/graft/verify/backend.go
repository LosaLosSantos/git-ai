package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/open_scion/internal/graft/workspace"
	"github.com/feder-cr/open_scion/sdk"
)

// errReadOnly is returned by the write methods of the verifier's backend. The
// verifier only reads the repository and moves its green refs, which it does
// through greenRefs.
var errReadOnly = errors.New("graft-verify does not write branches")

// sdkBackend implements workspace.Backend on top of the public sdk for one
// repository. Only the read methods and the changeset log are supported.
type sdkBackend struct {
	repo *sdk.Repo
	// refs reads references by their exact name. When set, a full reference
	// name such as refs/heads/main resolves only to that reference: a revision
	// lookup applies git's DWIM rules, so that once main is deleted a branch
	// named refs/heads/main (refs/heads/refs/heads/main) would be verified, and
	// its tip moved to green, in its place.
	refs *refUpdater
}

var _ workspace.Backend = (*sdkBackend)(nil)

// ResolveRevision implements workspace.Backend.
func (b *sdkBackend) ResolveRevision(ctx context.Context, rev string) (string, error) {
	if b.refs != nil && isFullRefName(rev) {
		target, err := b.refs.lookupRef(ctx, rev)
		if err != nil {
			return "", err
		}
		if target == "" {
			return "", fmt.Errorf("%w: reference %s", workspace.ErrNotFound, rev)
		}
		// The object ID is resolved to the commit it names, which checks that it
		// is one.
		rev = target
	}

	oid, err := b.repo.ResolveRevision(ctx, rev)
	if err != nil {
		return "", mapError(err)
	}
	return oid, nil
}

// ListTree implements workspace.Backend.
func (b *sdkBackend) ListTree(ctx context.Context, rev, path string, recursive bool) ([]workspace.Entry, error) {
	entries, err := b.repo.ListTree(ctx, rev, path, recursive)
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]workspace.Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, workspace.Entry{
			Path: e.Path,
			OID:  e.OID,
			Type: objectType(e.Type),
			Mode: uint32(e.Mode),
			Size: e.Size,
		})
	}
	return out, nil
}

// GetObjects implements workspace.Backend. Missing objects are left out.
func (b *sdkBackend) GetObjects(ctx context.Context, oids []string) (map[string][]byte, error) {
	objects, err := b.repo.GetObjects(ctx, oids)
	if err != nil {
		return nil, err
	}

	out := make(map[string][]byte, len(objects))
	for _, o := range objects {
		if !o.Missing {
			out[o.OID] = o.Data
		}
	}
	return out, nil
}

// ReadFile implements workspace.Backend.
func (b *sdkBackend) ReadFile(ctx context.Context, rev, path string) ([]byte, error) {
	content, err := b.repo.ReadFile(ctx, rev, path)
	if err != nil {
		return nil, mapError(err)
	}
	return content, nil
}

// CommitFiles implements workspace.Backend. It is not supported.
func (b *sdkBackend) CommitFiles(context.Context, workspace.CommitOptions) (string, error) {
	return "", errReadOnly
}

// Land implements workspace.Backend. It is not supported.
func (b *sdkBackend) Land(context.Context, workspace.LandOptions) (workspace.LandResult, error) {
	return workspace.LandResult{}, errReadOnly
}

// DeleteBranch implements workspace.Backend. It is not supported.
func (b *sdkBackend) DeleteBranch(context.Context, string) error {
	return errReadOnly
}

// Changesets implements workspace.Backend.
func (b *sdkBackend) Changesets(ctx context.Context, after uint64, limit int) ([]workspace.Changeset, error) {
	changesets, err := b.repo.Changesets(ctx, after, clampUint32(limit))
	if err != nil {
		return nil, err
	}

	out := make([]workspace.Changeset, 0, len(changesets))
	for _, c := range changesets {
		out = append(out, toChangeset(c))
	}
	return out, nil
}

// WatchChangesets implements workspace.Backend. The channels are closed when
// ctx is done or the watch ends. A watch failure is sent on the error channel
// before the change channel closes.
func (b *sdkBackend) WatchChangesets(ctx context.Context, after uint64) (<-chan workspace.Changeset, <-chan error) {
	changes, watchErrs := b.repo.WatchChangesets(ctx, after)
	out := make(chan workspace.Changeset)
	errs := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errs)

		for c := range changes {
			select {
			case out <- toChangeset(c):
			case <-ctx.Done():
				return
			}
		}
		if err := <-watchErrs; err != nil {
			errs <- err
		}
	}()

	return out, errs
}

// Blame implements workspace.Backend.
func (b *sdkBackend) Blame(ctx context.Context, rev, path string) ([]workspace.BlameLine, error) {
	result, err := b.repo.Blame(ctx, rev, path)
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]workspace.BlameLine, 0, len(result.Lines))
	for _, l := range result.Lines {
		out = append(out, workspace.BlameLine{
			LineNumber:         int(l.LineNumber),
			CommitID:           l.CommitID,
			OriginalLineNumber: int(l.OriginalLineNumber),
			Content:            l.Content,
		})
	}
	return out, nil
}

// PathHistory implements workspace.Backend.
func (b *sdkBackend) PathHistory(ctx context.Context, rev, path string, limit int) ([]workspace.Commit, error) {
	commits, err := b.repo.PathHistory(ctx, rev, path, clampUint32(limit))
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]workspace.Commit, 0, len(commits))
	for _, c := range commits {
		out = append(out, workspace.Commit{
			ID:         c.GetId(),
			AuthorName: string(c.GetAuthor().GetName()),
			Subject:    string(c.GetSubject()),
		})
	}
	return out, nil
}

// pinRevision is the revision name under which pinnedBackend answers with the
// commit it is pinned to. It is not a reference of the repository.
const pinRevision = "graft-verify-pin"

// pinnedBackend resolves pinRevision to a commit that the verifier chooses, so
// that the workspace package materializes exactly that commit: the verifier
// tests a tip or an older commit (bisect) with the same code path. Every other
// call goes to the wrapped backend.
type pinnedBackend struct {
	workspace.Backend

	mu  sync.Mutex
	oid string
}

// pin sets the commit that pinRevision resolves to.
func (p *pinnedBackend) pin(oid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.oid = oid
}

// ResolveRevision implements workspace.Backend.
func (p *pinnedBackend) ResolveRevision(ctx context.Context, rev string) (string, error) {
	if rev != pinRevision {
		return p.Backend.ResolveRevision(ctx, rev)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.oid == "" {
		return "", errors.New("graft-verify: no commit is pinned")
	}
	return p.oid, nil
}

// greenRefs reads and moves the green refs of the verifier.
type greenRefs interface {
	// Current returns the object ID of ref, or "" when ref does not exist.
	Current(ctx context.Context, ref string) (string, error)
	// Move points ref from old to new. An empty old means that ref must not
	// exist yet. The move fails, changing nothing, when ref does not point to
	// old.
	Move(ctx context.Context, ref, old, new string) error
}

// zeroOIDLike returns the all-zero object ID of the same length as oid, which
// is the "no object" value of its object format.
func zeroOIDLike(oid string) string {
	return strings.Repeat("0", len(oid))
}

// mapError translates sdk errors that the workspace reacts to into the
// workspace sentinel errors. Other errors are returned unchanged.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if sdk.Code(err) == codes.NotFound {
		return fmt.Errorf("%w: %s", workspace.ErrNotFound, status.Convert(err).Message())
	}
	return err
}

// objectType maps an sdk object type to the workspace object type.
func objectType(t sdk.ObjectType) workspace.ObjectType {
	switch t {
	case sdk.ObjectBlob:
		return workspace.ObjectBlob
	case sdk.ObjectTree:
		return workspace.ObjectTree
	case sdk.ObjectCommit:
		return workspace.ObjectCommit
	default:
		return workspace.ObjectType(strings.ToLower(t.String()))
	}
}

// toChangeset converts a changeset of the server's log.
func toChangeset(c *sdk.Changeset) workspace.Changeset {
	return workspace.Changeset{
		Sequence:   c.GetSequence(),
		Reference:  string(c.GetReference()),
		OldOID:     c.GetOldOid(),
		NewOID:     c.GetNewOid(),
		Operation:  c.GetOperation(),
		Actor:      c.GetActor(),
		RecordedAt: c.GetRecordedAt().AsTime(),
		Rewritten:  c.GetRewritten(),
	}
}

// clampUint32 converts a non-negative count to uint32. Negative values become
// zero, which selects the server default.
func clampUint32(n int) uint32 {
	if n <= 0 {
		return 0
	}
	if n > int(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(n)
}
