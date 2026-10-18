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
// Refine makes such a listing exact with a fixed number of git processes, whatever the size of the
// history. It relies on three facts about a listing L of head that excludes base (or commits that
// base reaches), whose extra commits are X:
//
//   - X is closed downwards in L: the parents of an extra commit are ancestors of base too, so
//     those of them that are in L are extra as well.
//   - Hence X is not empty exactly when some bottom of L, a commit of L none of whose parents is in
//     L, is an ancestor of base: going down from an extra commit through parents that are in L
//     ends at a bottom, which is extra.
//   - git merge-base --independent base b1 ... bn drops exactly the bottoms that base reaches. It
//     drops a commit that base or another of the commits reaches. If a bottom bi is reached by
//     another bottom bj, the path from bj to bi runs through a parent of bj that L does not list,
//     which is therefore an ancestor of base, so base reaches bi too.
//
// So one process tells whether the listing is exact. When it is not, one git rev-list base, which
// excludes nothing and is therefore exact, lists every ancestor of base, and the commits of the
// listing among them are removed: work linear in the history of base, in one process.
package revrange

import (
	"bufio"
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
			"committer dates are skewed.",
	})

	skewExtraCommitsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "graft_revrange_skew_extra_commits_total",
		Help: "Commits outside of the range that skewed listings held and that were removed.",
	})
)

// CountSkewedListing counts a listing that held commits outside of its range, for callers that
// handle such a listing themselves (see rebase.Plan).
func CountSkewedListing() {
	skewedListingsTotal.Inc()
}

// Commit is a commit of a range with all of its parents in order, as git rev-list --parents prints
// it.
type Commit struct {
	ID      git.ObjectID
	Parents []git.ObjectID
}

// Walker answers the questions that Refine builds a range from.
type Walker interface {
	// List returns every commit that is reachable from head and from none of the commits of
	// exclude, each with all of its parents, in no particular order. The listing may also hold
	// commits that are reachable from a commit of exclude (see the package documentation), never
	// fewer commits. Only Refine makes it exact.
	List(ctx context.Context, head git.ObjectID, exclude []git.ObjectID) ([]Commit, error)
	// MergeBases returns the best common ancestors of a and b, as git merge-base --all prints them,
	// and none when a and b share no history. The answer is exact.
	MergeBases(ctx context.Context, a, b git.ObjectID) ([]git.ObjectID, error)
	// ReachedBottoms returns the commits of bottoms that base reaches. bottoms must be the bottoms
	// of a listing that excludes base or commits that base reaches (see Bottoms), which lets one
	// question answer for all of them. The answer is exact.
	ReachedBottoms(ctx context.Context, base git.ObjectID, bottoms []git.ObjectID) (map[git.ObjectID]bool, error)
	// Reachable returns the commits of commits that base reaches, for any commits. It may walk the
	// whole history of base. The answer is exact.
	Reachable(ctx context.Context, base git.ObjectID, commits []git.ObjectID) (map[git.ObjectID]bool, error)
}

// Exact returns the commits reachable from head and not from base, exactly, in no particular order.
// It is empty when head is reachable from base.
func Exact(ctx context.Context, w Walker, base, head git.ObjectID) ([]Commit, error) {
	listed, err := w.List(ctx, head, []git.ObjectID{base})
	if err != nil {
		return nil, err
	}

	return Refine(ctx, w, base, listed)
}

// Refine returns the exact range of a listing: the commits of listed that base does not reach.
// listed must be a listing of a commit that excludes base or commits that base reaches (Walker.List,
// or one side of a symmetric difference with base on the other side). Without skew that costs one
// question for all the bottoms of the listing; otherwise one more, which may walk the history of
// base.
func Refine(ctx context.Context, w Walker, base git.ObjectID, listed []Commit) ([]Commit, error) {
	if len(listed) == 0 {
		return nil, nil
	}

	bottoms, err := Bottoms(listed)
	if err != nil {
		return nil, err
	}

	reached, err := w.ReachedBottoms(ctx, base, bottoms)
	if err != nil {
		return nil, err
	}
	if len(reached) == 0 {
		return listed, nil
	}

	skewedListingsTotal.Inc()

	ids := make([]git.ObjectID, 0, len(listed))
	for _, c := range listed {
		ids = append(ids, c.ID)
	}
	extra, err := w.Reachable(ctx, base, ids)
	if err != nil {
		return nil, err
	}
	skewExtraCommitsTotal.Add(float64(len(extra)))

	rng := make([]Commit, 0, len(listed)-len(extra))
	for _, c := range listed {
		if !extra[c.ID] {
			rng = append(rng, c)
		}
	}
	return rng, nil
}

// Bottoms returns the commits of listed none of whose parents is in listed, in listing order. It
// returns an error when a commit is listed twice.
func Bottoms(listed []Commit) ([]git.ObjectID, error) {
	in := make(map[git.ObjectID]bool, len(listed))
	for _, c := range listed {
		if in[c.ID] {
			return nil, fmt.Errorf("commit %s listed twice", c.ID)
		}
		in[c.ID] = true
	}

	var bottoms []git.ObjectID
	for _, c := range listed {
		bottom := true
		for _, parent := range c.Parents {
			if in[parent] {
				bottom = false
				break
			}
		}
		if bottom {
			bottoms = append(bottoms, c.ID)
		}
	}
	return bottoms, nil
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

// Sides lists the symmetric difference left...right with one git rev-list --left-right --parents:
// the commits that git lists on the side of left and those it lists on the side of right. Each side
// is a listing that excludes commits the other tip reaches (the merge bases), so Refine with the
// other tip as base makes it exact.
func Sides(ctx context.Context, repo gitcmd.RepositoryExecutor, left, right git.ObjectID) (leftSide, rightSide []Commit, _ error) {
	w := gitWalker{repo: repo}
	err := w.revListLines(ctx,
		[]gitcmd.Option{gitcmd.Flag{Name: "--left-right"}, gitcmd.Flag{Name: "--parents"}},
		[]string{left.String() + "..." + right.String()},
		func(line string, ids []git.ObjectID) error {
			c := Commit{ID: ids[0], Parents: ids[1:]}
			switch line[0] {
			case '<':
				leftSide = append(leftSide, c)
			case '>':
				rightSide = append(rightSide, c)
			default:
				return fmt.Errorf("rev-list --left-right printed %q", line)
			}
			return nil
		})
	return leftSide, rightSide, err
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
	args := make([]string, 0, len(exclude)+1)
	args = append(args, head.String())
	for _, id := range exclude {
		args = append(args, "^"+id.String())
	}

	var commits []Commit
	err := w.revList(ctx, []gitcmd.Option{gitcmd.Flag{Name: "--parents"}}, args, func(fields []git.ObjectID) {
		commits = append(commits, Commit{ID: fields[0], Parents: fields[1:]})
	})
	return commits, err
}

// MergeBases runs git merge-base --all, which exits with 1 and prints nothing when a and b share no
// history.
func (w gitWalker) MergeBases(ctx context.Context, a, b git.ObjectID) ([]git.ObjectID, error) {
	stdout, empty, err := w.mergeBase(ctx, "--all", []string{a.String(), b.String()})
	if err != nil || empty {
		return nil, err
	}
	return stdout, nil
}

// ReachedBottoms runs git merge-base --independent base bottoms...: it prints the commits that no
// other of its arguments reaches, so the bottoms it does not print are those that base reaches (see
// the package documentation).
func (w gitWalker) ReachedBottoms(ctx context.Context, base git.ObjectID, bottoms []git.ObjectID) (map[git.ObjectID]bool, error) {
	if len(bottoms) == 0 {
		return nil, nil
	}

	args := make([]string, 0, len(bottoms)+1)
	args = append(args, base.String())
	for _, id := range bottoms {
		args = append(args, id.String())
	}

	independent, _, err := w.mergeBase(ctx, "--independent", args)
	if err != nil {
		return nil, err
	}

	printed := make(map[git.ObjectID]bool, len(independent))
	for _, id := range independent {
		printed[id] = true
	}

	reached := map[git.ObjectID]bool{}
	for _, id := range bottoms {
		if !printed[id] {
			reached[id] = true
		}
	}
	return reached, nil
}

// Reachable runs git rev-list base, which excludes nothing and therefore lists every ancestor of
// base, and keeps the commits of commits among them.
func (w gitWalker) Reachable(ctx context.Context, base git.ObjectID, commits []git.ObjectID) (map[git.ObjectID]bool, error) {
	wanted := make(map[git.ObjectID]bool, len(commits))
	for _, id := range commits {
		wanted[id] = true
	}

	reached := map[git.ObjectID]bool{}
	err := w.revList(ctx, nil, []string{base.String()}, func(fields []git.ObjectID) {
		if wanted[fields[0]] {
			reached[fields[0]] = true
		}
	})
	return reached, err
}

// revList runs git rev-list with flags and args, and calls fn with the object IDs of each line of
// its output, as it reads them.
func (w gitWalker) revList(ctx context.Context, flags []gitcmd.Option, args []string, fn func([]git.ObjectID)) error {
	return w.revListLines(ctx, flags, args, func(_ string, ids []git.ObjectID) error {
		fn(ids)
		return nil
	})
}

// revListLines runs git rev-list with flags and args, and calls fn with each line of its output and
// the object IDs of the line, without the side mark of --left-right, as it reads them.
func (w gitWalker) revListLines(ctx context.Context, flags []gitcmd.Option, args []string, fn func(string, []git.ObjectID) error) error {
	objectHash, err := w.repo.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("detecting object hash: %w", err)
	}

	var stderr bytes.Buffer
	cmd, err := w.repo.Exec(ctx, gitcmd.Command{Name: "rev-list", Flags: flags, Args: args},
		gitcmd.WithStderr(&stderr), gitcmd.WithSetupStdout())
	if err != nil {
		return fmt.Errorf("rev-list: %w", err)
	}

	scanner := bufio.NewScanner(cmd)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var parseErr error
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(strings.TrimLeft(line, "<>"))
		if len(fields) == 0 || parseErr != nil {
			continue
		}
		ids, err := parseIDs(objectHash, fields)
		if err != nil {
			parseErr = fmt.Errorf("parsing rev-list output %q: %w", line, err)
			continue
		}
		if err := fn(line, ids); err != nil {
			parseErr = err
		}
	}
	scanErr := scanner.Err()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("rev-list: %w, stderr: %q", err, stderr.String())
	}
	if scanErr != nil {
		return fmt.Errorf("reading rev-list output: %w", scanErr)
	}
	return parseErr
}

// mergeBase runs git merge-base with the mode flag and args and returns the commits it prints.
// empty reports that it exited with 1 without printing anything, which --all does when the commits
// share no history.
func (w gitWalker) mergeBase(ctx context.Context, flag string, args []string) (_ []git.ObjectID, empty bool, _ error) {
	objectHash, err := w.repo.ObjectHash(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("detecting object hash: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := w.repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "merge-base",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: flag}},
			Args:  args,
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		if status, ok := command.ExitStatus(err); ok && status == 1 && stdout.Len() == 0 && stderr.Len() == 0 {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("merge-base %s: %w, stderr: %q", flag, err, stderr.String())
	}

	ids, err := parseIDs(objectHash, strings.Fields(stdout.String()))
	if err != nil {
		return nil, false, fmt.Errorf("parsing merge-base output: %w", err)
	}
	return ids, false, nil
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
