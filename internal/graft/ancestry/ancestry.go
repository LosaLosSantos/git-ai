// Package ancestry answers the history questions of a landing (revrange.Walker): which commits a
// change adds to a branch, whether a commit is reachable from another and the merge bases of two
// commits. The git checker runs git for each question, and the graph checker answers from the
// stored graph index (see internal/graft/derived/graph). The listing of the git checker may hold
// commits outside of the range when committer dates are skewed; revrange.Exact, which
// rebase.Plan uses, makes it exact. The listing of the graph checker is exact. A landing gives the
// same outcome with either.
package ancestry

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/revrange"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/feder-cr/git-ai/internal/graft/derived/graph"
	"github.com/feder-cr/git-ai/internal/log"
)

// graphKindName and graphKindVersion identify the graph entries in the derived-data store, as
// defined by internal/graft/derived/graph.
const (
	graphKindName    = graph.KindName
	graphKindVersion = graph.KindVersion
)

// Checker answers the history questions of one landing attempt. A Checker is used by one goroutine
// at a time. Its List is a listing that only revrange.Exact or revrange.Refine (through
// rebase.Plan) make exact: no caller may take it for the exact range.
type Checker = revrange.Walker

// RangeCommit is a commit of a range with all of its parents in order, as git rev-list --parents
// prints it.
type RangeCommit = revrange.Commit

// Git returns a Checker that runs git for each question. It is the behaviour without the graph
// index.
func Git(repo *localrepo.Repo) Checker {
	return revrange.Git(repo)
}

// Graph returns a Checker that answers from the graph index of repo. The index reads the parents
// of commits through reader and stores its entries in store, so that they are not computed again.
// A nil store computes the entries in memory only.
//
// Graph returns an error for repositories whose history git does not walk as the object database
// describes it: shallow repositories, repositories with grafts and repositories with replace refs.
// The answers of the index would differ from git for them.
func Graph(ctx context.Context, repo *localrepo.Repo, reader catfile.ObjectContentReader, store *derived.Store) (Checker, error) {
	if repo == nil || reader == nil {
		return nil, errors.New("graph ancestry needs a repository and an object reader")
	}

	repoPath, err := repo.Path(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting repository path: %w", err)
	}
	if err := checkHistoryWalkedAsStored(repoPath); err != nil {
		return nil, err
	}

	source := graph.NewCatfileSource(reader)
	index := graph.NewIndex(source, store.Kind(graphKindName, graphKindVersion))
	return graphChecker{index: index, source: source}, nil
}

// New returns the Graph checker when enabled, and the Git checker otherwise. When the graph
// checker cannot be constructed, New logs the reason and returns the Git checker.
func New(ctx context.Context, enabled bool, repo *localrepo.Repo, reader catfile.ObjectContentReader, store *derived.Store, logger log.Logger) Checker {
	if !enabled {
		return Git(repo)
	}

	checker, err := Graph(ctx, repo, reader, store)
	if err != nil {
		logger.WithError(err).Warn("graft: graph ancestry is unavailable, using git for ancestry")
		return Git(repo)
	}

	return checker
}

// checkHistoryWalkedAsStored returns an error when the repository at repoPath has shallow
// commits, grafts or replace refs, which change the history git walks.
func checkHistoryWalkedAsStored(repoPath string) error {
	for _, name := range []string{"shallow", filepath.Join("info", "grafts")} {
		if err := requireAbsent(filepath.Join(repoPath, name)); err != nil {
			return err
		}
	}

	replaced, err := os.ReadDir(filepath.Join(repoPath, "refs", "replace"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading replace refs: %w", err)
	}
	if len(replaced) > 0 {
		return errors.New("repository has replace refs")
	}

	packed, err := os.ReadFile(filepath.Join(repoPath, "packed-refs"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading packed refs: %w", err)
	}
	if bytes.Contains(packed, []byte(" refs/replace/")) {
		return errors.New("repository has replace refs")
	}

	return nil
}

// requireAbsent returns an error when path exists.
func requireAbsent(path string) error {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return fmt.Errorf("repository has %s", filepath.Base(path))
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("checking %s: %w", filepath.Base(path), err)
	}
}

// graphChecker answers with the graph index.
type graphChecker struct {
	index *graph.Index
	// source reads the parents of the commits of a range.
	source graph.Source
}

// The flags of the walk of List.
const (
	// fromHead marks the commits reachable from head.
	fromHead uint8 = 1 << iota
	// fromExcluded marks the commits reachable from a commit of exclude.
	fromExcluded
)

// List returns exactly the commits reachable from head and from none of exclude. It walks down from
// all of them at once in the order of their generation numbers, highest first. Every descendant of
// a commit has a higher generation, so when the walk takes a commit, all of its descendants that the
// walk reaches were taken already and its flags are final: it is in the range when head reaches it
// and no excluded commit does. The walk ends when no commit that only head reaches is left to take,
// as the ancestors of the others are not in the range.
func (c graphChecker) List(ctx context.Context, head git.ObjectID, exclude []git.ObjectID) ([]RangeCommit, error) {
	w := graphWalk{checker: c, flags: map[git.ObjectID]uint8{}}

	if err := w.add(ctx, head, fromHead); err != nil {
		return nil, err
	}
	for _, id := range exclude {
		if err := w.add(ctx, id, fromExcluded); err != nil {
			return nil, err
		}
	}

	var commits []RangeCommit
	for w.headOnly > 0 {
		next := heap.Pop(&w.queue).(generationItem)
		flags := w.flags[next.id]
		if flags == fromHead {
			w.headOnly--
		}

		parents, err := c.source.Parents(ctx, next.id)
		if err != nil {
			return nil, err
		}

		// A commit that an excluded commit reaches passes only that on: its ancestors are not in
		// the range, whether head reaches them or not.
		propagate := fromExcluded
		if flags == fromHead {
			commits = append(commits, RangeCommit{ID: next.id, Parents: parents})
			propagate = fromHead
		}

		for _, parent := range parents {
			if err := w.add(ctx, parent, propagate); err != nil {
				return nil, err
			}
		}
	}

	return commits, nil
}

// IsAncestor answers from the graph index.
func (c graphChecker) IsAncestor(ctx context.Context, ancestor, descendant git.ObjectID) (bool, error) {
	return c.index.IsAncestor(ctx, ancestor, descendant)
}

// MergeBases answers from the graph index.
func (c graphChecker) MergeBases(ctx context.Context, a, b git.ObjectID) ([]git.ObjectID, error) {
	return c.index.MergeBases(ctx, a, b)
}

// graphWalk is the state of one List of the graph checker.
type graphWalk struct {
	checker graphChecker
	// flags holds the flags of every commit the walk reached.
	flags map[git.ObjectID]uint8
	// queue holds the commits that the walk reached and did not take yet.
	queue generationQueue
	// queued holds the commits of queue.
	queued map[git.ObjectID]bool
	// headOnly counts the commits of queue whose flags are fromHead alone.
	headOnly int
}

// add gives id the flags, and queues it when the walk did not reach it before.
func (w *graphWalk) add(ctx context.Context, id git.ObjectID, flags uint8) error {
	old, reached := w.flags[id]
	if reached {
		if !w.queued[id] {
			// A commit is taken after all of its descendants, so a commit that was taken gets no
			// flags from a descendant afterwards.
			if old|flags != old {
				return fmt.Errorf("graph walk reached commit %s again after taking it", id)
			}
			return nil
		}

		if old == fromHead && old|flags != fromHead {
			w.headOnly--
		}
		w.flags[id] = old | flags
		return nil
	}

	generation, err := w.checker.index.Generation(ctx, id)
	if err != nil {
		return err
	}

	if w.queued == nil {
		w.queued = map[git.ObjectID]bool{}
	}
	w.flags[id] = flags
	w.queued[id] = true
	if flags == fromHead {
		w.headOnly++
	}
	heap.Push(&w.queue, generationItem{id: id, generation: generation})
	return nil
}

// generationItem is a commit of the queue of a graph walk.
type generationItem struct {
	id         git.ObjectID
	generation uint32
}

// generationQueue is a heap of commits, highest generation first.
type generationQueue []generationItem

func (q generationQueue) Len() int           { return len(q) }
func (q generationQueue) Less(i, j int) bool { return q[i].generation > q[j].generation }
func (q generationQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *generationQueue) Push(x any)        { *q = append(*q, x.(generationItem)) }
func (q *generationQueue) Pop() any {
	old := *q
	item := old[len(old)-1]
	*q = old[:len(old)-1]
	return item
}
