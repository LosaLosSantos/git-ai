package changedpaths

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"
	"sync/atomic"

	"github.com/feder-cr/git-ai/internal/git"
)

// ErrInvalidPath is returned for paths that PathHistory cannot answer, namely the
// absolute paths and the paths that reach above the root. Supports reports them.
var ErrInvalidPath = errors.New("changed paths: invalid path")

// maxStoredPerQuery bounds the summaries that one PathHistory query stores. It matches
// the bound that the Deriver puts on the writes of a walk. Without the bound a cold
// query on a long history would write one file for each commit it visits. A summary
// is computed only to be stored: once the bound is reached, the commits without a
// stored summary are answered by a diff restricted to the path, and a later query
// stores up to the bound more.
const maxStoredPerQuery = 1024

// Store persists the encoded summary of a commit, keyed by the commit ID. The
// store of the derived-data framework is adapted by NewIndexFor.
type Store interface {
	Get(commit string) ([]byte, bool, error)
	Put(commit string, data []byte) error
}

// Index answers first-parent path histories from the changed-paths summaries of
// commits. A summary is a pure function of its commit, so it is computed once and
// stored when a Store is given. It is safe for concurrent use if Objects and Store
// are.
type Index struct {
	objects  Objects
	store    Store // nil disables persistence
	maxPaths int
	computed atomic.Int64
}

// writeBudget bounds the summaries that a query stores. A nil budget is unlimited.
type writeBudget struct {
	left int
}

// take reports whether one more summary may be stored, and takes it from the budget.
func (b *writeBudget) take() bool {
	if b == nil {
		return true
	}
	if b.left <= 0 {
		return false
	}
	b.left--
	return true
}

// NewIndex returns an Index that reads objects and persists summaries in store. A
// nil store persists nothing, and every query diffs the path alone.
func NewIndex(objects Objects, store Store) *Index {
	return &Index{objects: objects, store: store, maxPaths: maxExactPaths}
}

// Computed returns how many summaries the index computed by diffing, rather than
// reading them from the store.
func (x *Index) Computed() int64 { return x.computed.Load() }

// CommitSummary returns the summary of the commit with the given ID, and stores it.
// Unlike the path queries, it reports a failure to store the summary, along with the
// summary itself, so that warming can report it.
func (x *Index) CommitSummary(ctx context.Context, commitID string) (Summary, error) {
	commit, err := x.objects.Commit(ctx, commitID)
	if err != nil {
		return Summary{}, fmt.Errorf("reading commit %s: %w", commitID, err)
	}

	s, found, err := x.storedSummary(commit.ID)
	if err != nil || found {
		return s, err
	}

	s, err = x.computeSummary(ctx, commit)
	if err != nil {
		return Summary{}, err
	}
	if x.store != nil {
		if err := x.store.Put(commit.ID, s.encode()); err != nil {
			return s, fmt.Errorf("storing changed paths of %s: %w", commit.ID, err)
		}
	}
	return s, nil
}

// KnownUnchanged reports whether the stored summary of commit proves that commit did
// not change path relative to its first parent. It reads the store only: it never
// reads objects or computes a summary, so it reports false for a commit without a
// valid stored summary, for an overflow and for a path that PathHistory does not
// answer (see Supports). It is a derived.UnchangedFunc, the pre-filter that
// DerivedDataService.PathHistory gives (*derived.Deriver).PathHistoryFiltered.
func (x *Index) KnownUnchanged(_ context.Context, commit git.ObjectID, path string) (bool, error) {
	filter, ok := normalize(path)
	if !ok || x.store == nil {
		return false, nil
	}

	s, found, err := x.storedSummary(commit.String())
	if err != nil || !found || s.Overflow {
		return false, err
	}
	return !s.MayContain(filter) || !s.Contains(filter), nil
}

// storedSummary returns the stored summary of the commit with the given ID. It
// reports false when there is no store, no entry or a corrupt one, which a computed
// summary replaces.
func (x *Index) storedSummary(commitID string) (Summary, bool, error) {
	if x.store == nil {
		return Summary{}, false, nil
	}

	data, found, err := x.store.Get(commitID)
	if err != nil {
		return Summary{}, false, fmt.Errorf("reading changed paths of %s: %w", commitID, err)
	}
	if !found {
		return Summary{}, false, nil
	}

	s, err := decodeSummary(data)
	if err != nil {
		return Summary{}, false, nil
	}
	return s, true, nil
}

// computeSummary returns the summary of commit, computed by diffing its whole tree
// against the tree of its first parent.
func (x *Index) computeSummary(ctx context.Context, commit Commit) (Summary, error) {
	parentTree, err := x.firstParentTree(ctx, commit)
	if err != nil {
		return Summary{}, err
	}

	x.computed.Add(1)
	paths, overflow, err := changedFiles(ctx, x.objects, parentTree, commit.Tree, "", x.maxPaths)
	if err != nil {
		return Summary{}, fmt.Errorf("diffing commit %s: %w", commit.ID, err)
	}

	if overflow {
		return Summary{Overflow: true}, nil
	}
	return newSummary(paths), nil
}

// firstParentTree returns the tree of the first parent of commit. A root commit
// compares with the empty tree, so that every file it holds counts as changed, as
// git log does.
func (x *Index) firstParentTree(ctx context.Context, commit Commit) (string, error) {
	if len(commit.Parents) == 0 {
		return "", nil
	}

	parent, err := x.objects.Commit(ctx, commit.Parents[0])
	if err != nil {
		return "", fmt.Errorf("reading parent %s: %w", commit.Parents[0], err)
	}
	return parent.Tree, nil
}

// changed reports whether commit changed path relative to its first parent. It
// is the condition under which git log --first-parent -- path shows the commit.
func (x *Index) changed(ctx context.Context, commit Commit, path string, budget *writeBudget) (bool, error) {
	if x.store == nil {
		// Without a store a summary would be computed for this one query only, so the
		// path is diffed alone, which reads fewer trees.
		return x.diffPath(ctx, commit, path)
	}

	s, found, err := x.storedSummary(commit.ID)
	if err != nil {
		return false, err
	}
	if !found {
		if !budget.take() {
			// The summary could not be stored, and computing it would diff the whole
			// tree for this one query, so the path is diffed alone.
			return x.diffPath(ctx, commit, path)
		}

		if s, err = x.computeSummary(ctx, commit); err != nil {
			return false, err
		}
		// The summary is a cache of the commit, so the answer does not depend on
		// whether it could be stored, and a failed write is ignored.
		_ = x.store.Put(commit.ID, s.encode())
	}

	if !s.Overflow {
		if !s.MayContain(path) {
			return false, nil
		}
		return s.Contains(path), nil
	}

	// The commit changed too many files to store them, so the path is diffed alone.
	return x.diffPath(ctx, commit, path)
}

// diffPath reports whether commit changed path, or a file below it, relative to its
// first parent. It diffs the trees restricted to path.
func (x *Index) diffPath(ctx context.Context, commit Commit, path string) (bool, error) {
	parentTree, err := x.firstParentTree(ctx, commit)
	if err != nil {
		return false, err
	}

	paths, _, err := changedFiles(ctx, x.objects, parentTree, commit.Tree, path, 0)
	if err != nil {
		return false, fmt.Errorf("diffing commit %s: %w", commit.ID, err)
	}
	return len(paths) > 0, nil
}

// PathHistory calls fn for each commit that changed path, newest first, starting at
// start and following first parents. It reports exactly the commits, in the order,
// that (*derived.Deriver).PathHistory reports for the same arguments: it stops after
// limit commits, and like the Deriver it reports none when limit is not positive.
// The path is normalized as the Deriver normalizes it (see Supports). The walk reads
// only the commits it visits and their summaries, and it never recurses.
func (x *Index) PathHistory(ctx context.Context, start, path string, limit int, fn func(Commit) error) error {
	filter, ok := normalize(path)
	if !ok {
		return fmt.Errorf("%w: %q", ErrInvalidPath, path)
	}

	budget := &writeBudget{left: maxStoredPerQuery}
	shown := 0
	for id := start; id != ""; {
		if shown >= limit {
			break
		}

		commit, err := x.objects.Commit(ctx, id)
		if err != nil {
			return fmt.Errorf("reading commit %s: %w", id, err)
		}

		changed, err := x.changed(ctx, commit, filter, budget)
		if err != nil {
			return err
		}
		if changed {
			if err := fn(commit); err != nil {
				return err
			}
			shown++
		}

		if len(commit.Parents) == 0 {
			break
		}
		id = commit.Parents[0]
	}

	return nil
}

// Supports reports whether PathHistory answers path. It holds for every path that
// the Deriver answers from its own normalization (the empty path, "/" and "." name
// the root, and the other paths are cleaned), except the absolute paths and the
// paths that reach above the root, which the callers answer with the Deriver.
func Supports(path string) bool {
	_, ok := normalize(path)
	return ok
}

// normalize returns the path that the Deriver compares for path: "" for the root,
// and otherwise the cleaned path. It reports false for the paths that the Deriver
// would compare as absolute or above the root.
func normalize(path string) (string, bool) {
	switch path {
	case "", "/", ".":
		return "", true
	}

	clean := pathpkg.Clean(path)
	switch {
	case clean == ".":
		return "", true
	case strings.HasPrefix(clean, "/"), clean == "..", strings.HasPrefix(clean, "../"):
		return "", false
	}
	return clean, true
}
