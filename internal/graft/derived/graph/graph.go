// Package graph implements the graph derived kind: the generation number and the
// first-parent skip pointers of every commit, and the IsAncestor and MergeBases
// queries that use them. The answers equal those of git rev-list --ancestry and
// git merge-base --all for the same history.
//
// An Index reads the parents of commits through a Source. The entry of a commit is
// computed from the entries of its parents and stored in the derived-data store
// (see KindStore), so a commit whose entry is stored is never walked again. Entries
// computed in memory are kept in a bounded LRU cache. Walks never recurse, so the
// depth of a history is not limited by the goroutine stack. An Index is not safe
// for concurrent use.
package graph

import (
	"container/heap"
	"context"
	"fmt"
	"sort"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/graft/derived"
	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	// KindName and KindVersion identify the stored entries of the graph kind in the
	// derived-data store. The version must be bumped when the encoding or the computation
	// changes. Other packages that open the graph kind (for example
	// internal/graft/ancestry) use them so that both read the same entries.
	KindName    = "graph"
	KindVersion = 1

	kindName    = KindName
	kindVersion = KindVersion

	// maxCachedEntries bounds the number of entries an Index keeps in memory.
	maxCachedEntries = 100000
)

// Source provides the parents of commits.
type Source interface {
	// Parents returns the parents of the commit id in order. A root commit has none.
	Parents(ctx context.Context, id git.ObjectID) ([]git.ObjectID, error)
}

// Flags used by MergeBases to paint the commits reachable from the two inputs.
const (
	reachableFromA uint8 = 1 << iota
	reachableFromB
	// stale marks commits that are ancestors of a common ancestor that was already
	// found, so that they cannot be best common ancestors.
	stale
)

// entry is what an Index knows about one commit.
type entry struct {
	// parents are the parents of the commit in order. They are loaded from the Source
	// when parentsLoaded is set. A stored entry does not carry its parents, so they
	// are only read when a query needs to walk past the commit.
	parents       []git.ObjectID
	parentsLoaded bool
	// expanded is set when the parents of the commit were scheduled for computation
	// by the walk that is running.
	expanded bool
	// done is set when generation and skips are known. Done entries have done
	// ancestors, or are stored with their values, so they are never walked again.
	done bool
	// generation is 1 for a root and one more than the highest generation of the
	// parents otherwise. An ancestor always has a smaller generation than its
	// descendants.
	generation uint32
	// skips[k] is the commit 2^k first-parent steps before this commit. It exists
	// exactly when 2^k is at most the first-parent depth of the commit, so a commit at
	// depth d has bits.Len(d) pointers.
	skips []git.ObjectID
}

// Index answers the queries of the graph kind for the history reachable from the
// commits it is asked about. The entry of a commit covers its whole history, so the
// entries of all its ancestors are computed with it, and stored.
type Index struct {
	source Source
	store  derived.KindStore
	// cache holds the done entries of recent queries. An evicted entry is read back
	// from the store or computed again.
	cache *lru.Cache[git.ObjectID, *entry]
}

// NewIndex returns an Index that reads commits through source and keeps its
// entries in store. A store that is nil never finds a value, so the Index then only
// computes entries in memory.
func NewIndex(source Source, store derived.KindStore) *Index {
	cache, err := lru.New[git.ObjectID, *entry](maxCachedEntries)
	if err != nil {
		// lru.New only fails for a size that is not positive, which is a programming error.
		panic(fmt.Sprintf("creating graph cache: %v", err))
	}

	return &Index{source: source, store: store, cache: cache}
}

// Generation returns the generation number of the commit id.
func (ix *Index) Generation(ctx context.Context, id git.ObjectID) (uint32, error) {
	e, err := newQuery(ix).lookup(ctx, id)
	if err != nil {
		return 0, err
	}
	return e.generation, nil
}

// IsAncestor reports whether ancestor is reachable from descendant, including when
// both are the same commit. Both commits must exist.
func (ix *Index) IsAncestor(ctx context.Context, ancestor, descendant git.ObjectID) (bool, error) {
	return newQuery(ix).isAncestor(ctx, ancestor, descendant)
}

// MergeBases returns the best common ancestors of a and b, sorted by object ID. A
// best common ancestor is a common ancestor that is not an ancestor of another common
// ancestor, as printed by git merge-base --all. It returns no commits when a and b
// have no common ancestor.
func (ix *Index) MergeBases(ctx context.Context, a, b git.ObjectID) ([]git.ObjectID, error) {
	return newQuery(ix).mergeBases(ctx, a, b)
}

// MergeBase returns one best common ancestor of a and b, the first of MergeBases.
// It returns the empty object ID when a and b have no common ancestor.
func (ix *Index) MergeBase(ctx context.Context, a, b git.ObjectID) (git.ObjectID, error) {
	bases, err := ix.MergeBases(ctx, a, b)
	if err != nil || len(bases) == 0 {
		return "", err
	}
	return bases[0], nil
}

// query is one call of an Index. It keeps every entry it loads, so that the
// evictions of the cache cannot change the answer of a call while it runs. Entries
// that a failed call left half computed are dropped with the query.
type query struct {
	ix      *Index
	entries map[git.ObjectID]*entry
}

func newQuery(ix *Index) *query {
	return &query{ix: ix, entries: make(map[git.ObjectID]*entry)}
}

// load returns the entry of id without computing it. The entry is read from the
// query, the cache or the store, or created empty when none of them has it.
func (q *query) load(id git.ObjectID) *entry {
	if e, ok := q.entries[id]; ok {
		return e
	}

	if e, ok := q.ix.cache.Get(id); ok {
		q.entries[id] = e
		return e
	}

	if e, ok := q.ix.readStored(id); ok {
		q.ix.cache.Add(id, e)
		q.entries[id] = e
		return e
	}

	e := &entry{}
	q.entries[id] = e
	return e
}

// lookup returns the done entry of id, computing the entries of its history that
// are missing.
func (q *query) lookup(ctx context.Context, id git.ObjectID) (*entry, error) {
	if e := q.load(id); e.done {
		return e, nil
	}
	return q.get(ctx, id)
}

// parents returns the parents of the commit id, which has the entry e. They are read
// from the source the first time.
func (q *query) parents(ctx context.Context, id git.ObjectID, e *entry) ([]git.ObjectID, error) {
	if !e.parentsLoaded {
		parents, err := q.ix.source.Parents(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("reading parents of %s: %w", id, err)
		}
		e.parents = parents
		e.parentsLoaded = true
	}
	return e.parents, nil
}

// get computes the entries of id and of the commits of its history that are missing.
// The walk uses an explicit stack. Each commit is expanded, which schedules its
// parents, and computed once all of them are done.
func (q *query) get(ctx context.Context, id git.ObjectID) (*entry, error) {
	stack := []git.ObjectID{id}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		current := stack[len(stack)-1]
		e := q.load(current)

		switch {
		case e.done:
			stack = stack[:len(stack)-1]
		case !e.expanded:
			parents, err := q.parents(ctx, current, e)
			if err != nil {
				return nil, err
			}

			e.expanded = true
			for _, parent := range parents {
				pe := q.load(parent)
				if pe.done {
					continue
				}
				if pe.expanded {
					// The parent is an ancestor of itself: it was expanded and has not been
					// computed, so it is still waiting for commits that lead back to it.
					return nil, fmt.Errorf("commit graph has a cycle through %s", parent)
				}
				stack = append(stack, parent)
			}
		default:
			// Every parent was scheduled above this commit and is done.
			if err := q.compute(ctx, current, e); err != nil {
				return nil, err
			}
			stack = stack[:len(stack)-1]
		}
	}

	return q.entries[id], nil
}

// compute sets the generation and the skip pointers of the commit id with the entry
// e. The parents of e must be done. The entry is cached and stored.
func (q *query) compute(ctx context.Context, id git.ObjectID, e *entry) error {
	var highest uint32
	for _, parent := range e.parents {
		pe, err := q.lookup(ctx, parent)
		if err != nil {
			return err
		}
		if pe.generation > highest {
			highest = pe.generation
		}
	}
	e.generation = highest + 1

	if len(e.parents) > 0 {
		e.skips = []git.ObjectID{e.parents[0]}
		// The commit 2^k steps back is 2^(k-1) steps before the commit 2^(k-1) steps
		// back. That commit has a pointer of length k-1 exactly when 2^k steps exist.
		for k := 1; ; k++ {
			middle, err := q.lookup(ctx, e.skips[k-1])
			if err != nil {
				return err
			}
			if len(middle.skips) < k {
				break
			}
			e.skips = append(e.skips, middle.skips[k-1])
		}
	}

	e.done = true
	q.ix.cache.Add(id, e)
	q.ix.writeStored(id, e)

	return nil
}

// isAncestor reports whether ancestor is reachable from descendant.
func (q *query) isAncestor(ctx context.Context, ancestor, descendant git.ObjectID) (bool, error) {
	a, err := q.lookup(ctx, ancestor)
	if err != nil {
		return false, err
	}
	d, err := q.lookup(ctx, descendant)
	if err != nil {
		return false, err
	}

	if ancestor == descendant {
		return true, nil
	}
	if a.generation >= d.generation {
		// Every ancestor has a smaller generation than the commits it reaches.
		return false, nil
	}

	onChain, err := q.onFirstParentChain(ctx, ancestor, a.generation, descendant)
	if err != nil {
		return false, err
	}
	if onChain {
		return true, nil
	}

	return q.search(ctx, ancestor, descendant, a.generation)
}

// onFirstParentChain reports whether ancestor is on the first-parent chain of
// descendant. The generations along the chain decrease, so the commits of the chain
// with a generation of at least the generation of ancestor form a prefix of it. The
// skip pointers reach the end of that prefix in logarithmically many steps, and
// ancestor is on the chain exactly when the end of the prefix is ancestor.
func (q *query) onFirstParentChain(ctx context.Context, ancestor git.ObjectID, generation uint32, descendant git.ObjectID) (bool, error) {
	current := descendant
	cur, err := q.lookup(ctx, current)
	if err != nil {
		return false, err
	}

	for current != ancestor {
		moved := false
		// Jumps of decreasing length. The longest jump that stays at or above the
		// generation of ancestor is taken, and then the walk continues from there.
		for k := len(cur.skips) - 1; k >= 0; k-- {
			next, err := q.lookup(ctx, cur.skips[k])
			if err != nil {
				return false, err
			}
			if next.generation >= generation {
				current = cur.skips[k]
				cur = next
				moved = true
				break
			}
		}
		if !moved {
			return false, nil
		}
	}

	return true, nil
}

// search looks for ancestor among the ancestors of descendant. It does not expand
// a commit whose generation is not above the generation of ancestor, since such a
// commit cannot reach ancestor.
func (q *query) search(ctx context.Context, ancestor, descendant git.ObjectID, generation uint32) (bool, error) {
	visited := map[git.ObjectID]bool{descendant: true}
	stack := []git.ObjectID{descendant}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		e, err := q.lookup(ctx, current)
		if err != nil {
			return false, err
		}
		parents, err := q.parents(ctx, current, e)
		if err != nil {
			return false, err
		}

		for _, parent := range parents {
			if parent == ancestor {
				return true, nil
			}
			if visited[parent] {
				continue
			}
			visited[parent] = true

			pe, err := q.lookup(ctx, parent)
			if err != nil {
				return false, err
			}
			if pe.generation <= generation {
				continue
			}
			stack = append(stack, parent)
		}
	}

	return false, nil
}

// queuedCommit is a commit waiting in the queue of the paint-down walk.
type queuedCommit struct {
	id         git.ObjectID
	generation uint32
}

// commitHeap is a max-heap of commits ordered by generation, so that commits are
// popped before their ancestors.
type commitHeap struct {
	items []queuedCommit
}

func (h *commitHeap) Len() int { return len(h.items) }

func (h *commitHeap) Less(i, j int) bool { return h.items[i].generation > h.items[j].generation }

func (h *commitHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *commitHeap) Push(x any) { h.items = append(h.items, x.(queuedCommit)) }

func (h *commitHeap) Pop() any {
	last := h.items[len(h.items)-1]
	h.items = h.items[:len(h.items)-1]
	return last
}

// hasNonStale reports whether a queued commit is not marked stale. The walk of
// mergeBases stops once every queued commit is stale.
func (h *commitHeap) hasNonStale(flags map[git.ObjectID]uint8) bool {
	for _, item := range h.items {
		if flags[item.id]&stale == 0 {
			return true
		}
	}
	return false
}

// mergeBases returns the best common ancestors of a and b.
func (q *query) mergeBases(ctx context.Context, a, b git.ObjectID) ([]git.ObjectID, error) {
	if _, err := q.lookup(ctx, a); err != nil {
		return nil, err
	}
	if _, err := q.lookup(ctx, b); err != nil {
		return nil, err
	}
	if a == b {
		return []git.ObjectID{a}, nil
	}

	// The paint-down walk of git merge-base: commits are visited newest generation
	// first, and a commit reachable from both inputs is a candidate. The candidates
	// are then filtered for ancestors of other candidates.
	flags := map[git.ObjectID]uint8{a: reachableFromA, b: reachableFromB}
	queue := &commitHeap{}
	for _, id := range []git.ObjectID{a, b} {
		e, err := q.lookup(ctx, id)
		if err != nil {
			return nil, err
		}
		queue.items = append(queue.items, queuedCommit{id: id, generation: e.generation})
	}
	heap.Init(queue)

	var candidates []git.ObjectID
	for queue.hasNonStale(flags) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		current := heap.Pop(queue).(queuedCommit)
		f := flags[current.id]
		if f&(reachableFromA|reachableFromB) == reachableFromA|reachableFromB && f&stale == 0 {
			candidates = append(candidates, current.id)
			f |= stale
			flags[current.id] = f
		}

		e, err := q.lookup(ctx, current.id)
		if err != nil {
			return nil, err
		}
		parents, err := q.parents(ctx, current.id, e)
		if err != nil {
			return nil, err
		}

		for _, parent := range parents {
			if flags[parent]&f == f {
				continue
			}
			flags[parent] |= f

			pe, err := q.lookup(ctx, parent)
			if err != nil {
				return nil, err
			}
			heap.Push(queue, queuedCommit{id: parent, generation: pe.generation})
		}
	}

	var best []git.ObjectID
	for i, candidate := range candidates {
		redundant := false
		for j, other := range candidates {
			if i == j {
				continue
			}
			isAncestor, err := q.isAncestor(ctx, candidate, other)
			if err != nil {
				return nil, err
			}
			if isAncestor {
				redundant = true
				break
			}
		}
		if !redundant {
			best = append(best, candidate)
		}
	}

	sort.Slice(best, func(i, j int) bool { return best[i] < best[j] })
	return best, nil
}
