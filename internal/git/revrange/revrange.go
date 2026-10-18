// Package revrange computes revision ranges exactly: the commits reachable from one commit and not
// from another.
//
// git rev-list base..head is not exact. Its walk stops once every commit it still has to visit is
// reachable from base and older, by committer date, than the last commit it listed (revision.c,
// limit_list and still_interesting, after a few more steps). A commit that the walk reached from
// head before the walk from base reached it is then listed although base reaches it. That happens
// when committer dates are skewed: commits dated before the history they build on, for example
// because the clock of their author is behind or because their dates were set explicitly. The
// listing never misses a commit of the range, because only the ancestors of base are ever marked
// as excluded, but it may hold extra commits, and every one of them is an ancestor of base.
//
// Exact builds the exact range from such a listing with exact primitives (git merge-base
// --is-ancestor and git merge-base --all, which walk until the answer is certain). It relies on
// two facts about the extra commits X of a listing L of head excluding base:
//
//   - X is closed downwards in L: the parents of an extra commit are ancestors of base too, so
//     those of them that are in L are extra as well.
//   - Hence X is not empty exactly when some bottom of L, a commit of L none of whose parents is in
//     L, is an ancestor of base: going down from an extra commit through parents that are in L
//     ends at a bottom, which is extra.
//
// Without skew a listing has one bottom per point where the range joins the history of base,
// usually one, and checking the bottoms costs one git process each. Under skew Exact lists again
// below the exact merge bases, which bound the walk on every path that runs through them, and then
// removes the remaining extra commits bottom by bottom with one check each.
package revrange

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	skewedListingsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "graft_revrange_skewed_listings_total",
		Help: "Listings of a revision range that held commits outside of the range, which happens when " +
			"committer dates are skewed, and that were made exact.",
	})

	skewChecksTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "graft_revrange_skew_checks_total",
		Help: "Ancestry checks spent removing the commits outside of the range from skewed listings.",
	})
)

// Commit is a commit of a range with all of its parents in order, as git rev-list --parents prints
// it.
type Commit struct {
	ID      git.ObjectID
	Parents []git.ObjectID
}

// Walker answers the questions that Exact builds a range from.
type Walker interface {
	// List returns every commit that is reachable from head and from none of the commits of
	// exclude, each with all of its parents, in no particular order. The listing may also hold
	// commits that are reachable from a commit of exclude (see the package documentation), never
	// fewer commits. Only Exact and Refine make it exact.
	List(ctx context.Context, head git.ObjectID, exclude []git.ObjectID) ([]Commit, error)
	// IsAncestor reports whether ancestor is reachable from descendant, the same commit included.
	// The answer is exact.
	IsAncestor(ctx context.Context, ancestor, descendant git.ObjectID) (bool, error)
	// MergeBases returns the best common ancestors of a and b, as git merge-base --all prints them,
	// and none when a and b share no history. The answer is exact.
	MergeBases(ctx context.Context, a, b git.ObjectID) ([]git.ObjectID, error)
}

// Exact returns the commits reachable from head and not from base, exactly, in no particular order.
// It is empty when head is reachable from base.
func Exact(ctx context.Context, w Walker, base, head git.ObjectID) ([]Commit, error) {
	listed, err := w.List(ctx, head, []git.ObjectID{base})
	if err != nil {
		return nil, err
	}

	return Refine(ctx, w, base, head, listed)
}

// Refine returns the exact range of the commits reachable from head and not from base, given
// listed, which w.List returned for head excluding base.
//
// When the listing is exact, which it is unless committer dates are skewed, Refine checks each of
// its bottoms once. Otherwise it lists again below the merge bases of base and head and removes the
// commits of that listing that base reaches, checking one bottom at a time: at most one check for
// each such commit plus one for each bottom of the range.
func Refine(ctx context.Context, w Walker, base, head git.ObjectID, listed []Commit) ([]Commit, error) {
	if len(listed) == 0 {
		return nil, nil
	}

	rng, err := newRangeSet(listed)
	if err != nil {
		return nil, fmt.Errorf("listing of %s excluding %s: %w", head, base, err)
	}

	extra := false
	for _, bottom := range rng.bottoms() {
		reachable, err := w.IsAncestor(ctx, bottom, base)
		if err != nil {
			return nil, err
		}
		if reachable {
			extra = true
			break
		}
	}
	if !extra {
		return listed, nil
	}

	skewedListingsTotal.Inc()

	// Every common ancestor of base and head is an ancestor of one of their merge bases, so the
	// range is also what head reaches without reaching a merge base, and a walk that excludes the
	// merge bases stops at them.
	bases, err := w.MergeBases(ctx, base, head)
	if err != nil {
		return nil, err
	}
	if len(bases) == 0 {
		// A bottom of the listing is an ancestor of base, and every commit of the listing is an
		// ancestor of head, so base and head share history.
		return nil, fmt.Errorf("%s and %s have no merge base although they share history", base, head)
	}

	relisted, err := w.List(ctx, head, bases)
	if err != nil {
		return nil, err
	}

	rng, err = newRangeSet(relisted)
	if err != nil {
		return nil, fmt.Errorf("listing of %s excluding the merge bases: %w", head, err)
	}

	// A bottom that base does not reach is in the range for good: the commits of the range are
	// exactly those that base does not reach, whatever else the walk removes.
	inRange := make(map[git.ObjectID]bool)
	checks := 0
	for removed := true; removed; {
		removed = false
		for _, bottom := range rng.bottoms() {
			if inRange[bottom] {
				continue
			}

			checks++
			reachable, err := w.IsAncestor(ctx, bottom, base)
			if err != nil {
				skewChecksTotal.Add(float64(checks))
				return nil, err
			}

			if reachable {
				rng.remove(bottom)
				removed = true
			} else {
				inRange[bottom] = true
			}
		}
	}
	skewChecksTotal.Add(float64(checks))

	return rng.commits(), nil
}

// Boundary returns the parents of the commits of rng that are not themselves in rng, sorted. For
// the exact range of head excluding base, they are the commits where the range joins the history
// of base: every path from head into that history runs through one of them, so a walk of head that
// also excludes them cannot list a commit outside of the range.
func Boundary(rng []Commit) []git.ObjectID {
	in := make(map[git.ObjectID]bool, len(rng))
	for _, c := range rng {
		in[c.ID] = true
	}

	seen := make(map[git.ObjectID]bool)
	var boundary []git.ObjectID
	for _, c := range rng {
		for _, parent := range c.Parents {
			if !in[parent] && !seen[parent] {
				seen[parent] = true
				boundary = append(boundary, parent)
			}
		}
	}

	sort.Slice(boundary, func(i, j int) bool { return boundary[i] < boundary[j] })
	return boundary
}

// rangeSet is a listing of commits that Refine removes commits from.
type rangeSet struct {
	// order is the order of the listing.
	order []git.ObjectID
	// byID holds the commits that are still in the set.
	byID map[git.ObjectID]Commit
}

func newRangeSet(listed []Commit) (*rangeSet, error) {
	s := &rangeSet{byID: make(map[git.ObjectID]Commit, len(listed))}
	for _, c := range listed {
		if _, ok := s.byID[c.ID]; ok {
			return nil, fmt.Errorf("commit %s listed twice", c.ID)
		}
		s.byID[c.ID] = c
		s.order = append(s.order, c.ID)
	}
	return s, nil
}

// bottoms returns the commits of the set none of whose parents is in the set, in listing order.
func (s *rangeSet) bottoms() []git.ObjectID {
	var bottoms []git.ObjectID
	for _, id := range s.order {
		c, ok := s.byID[id]
		if !ok {
			continue
		}

		bottom := true
		for _, parent := range c.Parents {
			if _, ok := s.byID[parent]; ok {
				bottom = false
				break
			}
		}
		if bottom {
			bottoms = append(bottoms, id)
		}
	}
	return bottoms
}

func (s *rangeSet) remove(id git.ObjectID) {
	delete(s.byID, id)
}

// commits returns the commits of the set in listing order.
func (s *rangeSet) commits() []Commit {
	commits := make([]Commit, 0, len(s.byID))
	for _, id := range s.order {
		if c, ok := s.byID[id]; ok {
			commits = append(commits, c)
		}
	}
	return commits
}

// Git returns a Walker that runs git in repo for each question.
func Git(repo gitcmd.RepositoryExecutor) Walker {
	return gitWalker{repo: repo}
}

type gitWalker struct {
	repo gitcmd.RepositoryExecutor
}

// List runs git rev-list --parents.
func (w gitWalker) List(ctx context.Context, head git.ObjectID, exclude []git.ObjectID) ([]Commit, error) {
	objectHash, err := w.repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	args := make([]string, 0, len(exclude)+1)
	args = append(args, head.String())
	for _, id := range exclude {
		args = append(args, "^"+id.String())
	}

	var stdout, stderr bytes.Buffer
	if err := w.repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "rev-list",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--parents"}},
			Args:  args,
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return nil, fmt.Errorf("rev-list: %w, stderr: %q", err, stderr.String())
	}

	var commits []Commit
	for _, line := range strings.Split(stdout.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		ids, err := parseIDs(objectHash, fields)
		if err != nil {
			return nil, fmt.Errorf("parsing rev-list output %q: %w", line, err)
		}
		commits = append(commits, Commit{ID: ids[0], Parents: ids[1:]})
	}

	return commits, nil
}

// IsAncestor runs git merge-base --is-ancestor, which exits with 1 when ancestor is not reachable
// from descendant.
func (w gitWalker) IsAncestor(ctx context.Context, ancestor, descendant git.ObjectID) (bool, error) {
	var stderr bytes.Buffer
	if err := w.repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "merge-base",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--is-ancestor"}},
			Args:  []string{ancestor.String(), descendant.String()},
		},
		gitcmd.WithStderr(&stderr),
	); err != nil {
		if status, ok := command.ExitStatus(err); ok && status == 1 && stderr.Len() == 0 {
			return false, nil
		}
		return false, fmt.Errorf("merge-base --is-ancestor: %w, stderr: %q", err, stderr.String())
	}

	return true, nil
}

// MergeBases runs git merge-base --all, which exits with 1 and prints nothing when a and b share no
// history.
func (w gitWalker) MergeBases(ctx context.Context, a, b git.ObjectID) ([]git.ObjectID, error) {
	objectHash, err := w.repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := w.repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "merge-base",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--all"}},
			Args:  []string{a.String(), b.String()},
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		if status, ok := command.ExitStatus(err); ok && status == 1 && stdout.Len() == 0 && stderr.Len() == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("merge-base --all: %w, stderr: %q", err, stderr.String())
	}

	bases, err := parseIDs(objectHash, strings.Fields(stdout.String()))
	if err != nil {
		return nil, fmt.Errorf("parsing merge-base output: %w", err)
	}
	return bases, nil
}

func parseIDs(objectHash git.ObjectHash, fields []string) ([]git.ObjectID, error) {
	ids := make([]git.ObjectID, 0, len(fields))
	for _, field := range fields {
		id, err := objectHash.FromHex(field)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, errors.New("no object ID")
	}
	return ids, nil
}
