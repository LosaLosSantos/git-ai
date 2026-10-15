package verify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ErrEmptyRange is returned by CulpritFinder.Find when no commits lie between
// the green baseline and the failing tip, so there is nothing to blame.
var ErrEmptyRange = errors.New("verify: no commits between the green baseline and the tip")

// ErrNoBaseline is returned by AutoBisector.Bisect when no green tip is known,
// so the search has no lower bound.
var ErrNoBaseline = errors.New("verify: no green baseline to bisect from")

// commitTester runs the test command against one commit. It reports whether
// the test passed. An error means the test could not be run at all; it says
// nothing about the commit, and the bisection stops.
type commitTester interface {
	Test(ctx context.Context, oid string) (bool, error)
}

// verdictTester adapts a Tester, whose verdicts carry more than pass or fail,
// to commitTester. A timed out run is a failure, as the verifier reports it.
type verdictTester struct {
	tester Tester
}

// Test implements commitTester.
func (t verdictTester) Test(ctx context.Context, oid string) (bool, error) {
	res, err := t.tester.Test(ctx, oid)
	if err != nil {
		return false, err
	}
	return res.Passed && !res.TimedOut, nil
}

// CommitInfo is the commit metadata that bisection and auto-revert need.
type CommitInfo struct {
	ID      string
	Subject string
	// Message is the full commit message, which carries the trailers.
	Message string
	// CommitterEmail is the email of the committer. Auto-reverts are recognized
	// by it, so that a commit cannot pose as one by writing the trailer alone.
	CommitterEmail string
	// CommittedAt is the committer date. It is zero when the server did not
	// report it.
	CommittedAt time.Time
	// Truncated reports that the server cut the message, so that its trailers
	// are unknown. Such a commit is treated as a possible auto-revert.
	Truncated bool
}

// CommitSource lists the commits a bisection searches.
type CommitSource interface {
	// FirstParentRange returns the commits reachable from tip along first
	// parents and not reachable from base, oldest first. The last element is
	// tip itself. See CommitLister for the implementation over the server.
	FirstParentRange(ctx context.Context, base, tip string) ([]CommitInfo, error)
}

// ScratchReverter builds the commits that a bisection tests in place of the
// commits of its range that still contain a culprit which was reverted later in
// the range. See CulpritFinder.
type ScratchReverter interface {
	// RevertOnto creates a commit that is base with the changes of culprits
	// undone, in the given order, and returns it. A culprit whose changes are
	// already undone on base is skipped; when every culprit is, base itself is
	// returned. The commit must not be created on a branch that is verified.
	// release removes what keeps the commit reachable (a temporary branch); it
	// is never nil and must be called once the commit was tested.
	RevertOnto(ctx context.Context, base string, culprits []string) (oid string, release func() error, err error)
}

// BisectResult is the outcome of CulpritFinder.Find.
type BisectResult struct {
	// Base is the last green commit, the exclusive lower bound of the search.
	Base string
	// Tip is the failing commit the search started from.
	Tip string
	// Candidates is the number of commits between Base and Tip, inclusive of Tip.
	Candidates int
	// Tests is the number of test runs, including the flake check.
	Tests int
	// Flaky is true when the tip passed on the re-run. Nothing is blamed then.
	Flaky bool
	// Culprit is the first failing commit, or nil when Flaky is set.
	Culprit *CommitInfo
	// Undone lists the culprits of the range that an automatic revert later in
	// the range already undid. The commits between such a culprit and its revert
	// were tested with the culprit undone.
	Undone []string
}

// CulpritFinder finds the first commit of a first-parent range that fails the
// test command. It assumes the failure is monotone along the range (every
// commit after the culprit fails too); when that does not hold the result is
// still a commit whose parent passed, but not necessarily the only culprit.
//
// An automatic revert in the range breaks that assumption: once a culprit C is
// reverted by R, every commit from C up to R fails because of C, so a plain
// search returns C again and a second culprit D that landed between C and R is
// never found. When RevertOf recognizes R, each commit from C up to R is tested
// with C undone, through a scratch commit of Scratch, so that the range is
// monotone again and the search finds D.
type CulpritFinder struct {
	Tester  commitTester
	Commits CommitSource
	// RevertOf returns the culprit that c reverted automatically, and whether c
	// is such an automatic revert. When nil, no commit is taken as one.
	RevertOf func(c CommitInfo) (string, bool)
	// Scratch builds the commits tested with reverted culprits undone. When nil,
	// a range that needs one cannot be searched, and Find fails.
	Scratch ScratchReverter
}

// Find runs the flake check and then binary-searches the first-parent range
// base..tip. The tip is expected to fail already; Find re-runs it once and
// reports Flaky when it passes.
func (f *CulpritFinder) Find(ctx context.Context, base, tip string) (*BisectResult, error) {
	if base == "" || tip == "" {
		return nil, errors.New("verify: base and tip are required")
	}
	if base == tip {
		return nil, ErrEmptyRange
	}

	commits, err := f.Commits.FirstParentRange(ctx, base, tip)
	if err != nil {
		return nil, fmt.Errorf("list commits %s..%s: %w", base, tip, err)
	}
	if len(commits) == 0 {
		return nil, ErrEmptyRange
	}
	if commits[len(commits)-1].ID != tip {
		return nil, fmt.Errorf("verify: commit range does not end at tip %s", tip)
	}

	res := &BisectResult{Base: base, Tip: tip, Candidates: len(commits)}
	undo := f.undoneCulprits(commits)
	for _, culprits := range undo {
		for _, c := range culprits {
			if !slices.Contains(res.Undone, c) {
				res.Undone = append(res.Undone, c)
			}
		}
	}

	// Flake check: a failure that does not reproduce on the same commit is not
	// attributable to the commit. Every revert of the range is at or before the
	// tip, so the tip is tested as it is.
	passed, err := f.test(ctx, res, tip, undo[len(commits)-1])
	if err != nil {
		return res, fmt.Errorf("flake check of %s: %w", tip, err)
	}
	if passed {
		res.Flaky = true
		return res, nil
	}

	idx, err := f.search(ctx, res, commits, undo)
	if err != nil {
		return res, err
	}
	culprit := commits[idx]
	res.Culprit = &culprit
	return res, nil
}

// undoneCulprits returns, for every commit of the range, the culprits that an
// automatic revert later in the range undoes and that the commit still
// contains: commits[k] contains culprit commits[i] up to its revert commits[j],
// for i <= k < j. The culprits of a commit are listed newest first, the order
// in which they are reverted. A revert of a commit outside the range is
// ignored: the green baseline, which contains it, passed.
func (f *CulpritFinder) undoneCulprits(commits []CommitInfo) [][]string {
	undo := make([][]string, len(commits))
	if f.RevertOf == nil {
		return undo
	}

	index := make(map[string]int, len(commits))
	for i, c := range commits {
		index[c.ID] = i
	}
	for j, c := range commits {
		target, ok := f.RevertOf(c)
		if !ok {
			continue
		}
		i, ok := index[target]
		if !ok || i >= j {
			continue
		}
		for k := i; k < j; k++ {
			if !slices.Contains(undo[k], target) {
				undo[k] = append(undo[k], target)
			}
		}
	}
	for _, culprits := range undo {
		slices.SortFunc(culprits, func(a, b string) int { return index[b] - index[a] })
	}
	return undo
}

// search returns the index of the first failing commit. Invariant: commits[hi]
// fails and the commit before lo passes (lo == -1 stands for base, which is
// green by definition). The loop ends when lo and hi are adjacent, so hi is the
// first failing commit. Each commit is tested with the culprits of undo[i]
// undone.
func (f *CulpritFinder) search(ctx context.Context, res *BisectResult, commits []CommitInfo, undo [][]string) (int, error) {
	lo, hi := -1, len(commits)-1
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		passed, err := f.test(ctx, res, commits[mid].ID, undo[mid])
		if err != nil {
			return 0, fmt.Errorf("bisect test of %s: %w", commits[mid].ID, err)
		}
		if passed {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi, nil
}

// test runs the test command on oid with the culprits of undo undone, and
// counts the run. Without culprits to undo, oid itself is tested; otherwise a
// scratch commit of Scratch is, and it is released after the run.
func (f *CulpritFinder) test(ctx context.Context, res *BisectResult, oid string, undo []string) (_ bool, returnedErr error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if len(undo) == 0 {
		res.Tests++
		return f.Tester.Test(ctx, oid)
	}

	if f.Scratch == nil {
		return false, fmt.Errorf("verify: %s must be tested with the reverted culprits %s undone, and no scratch reverter is configured",
			oid, strings.Join(undo, ", "))
	}
	scratch, release, err := f.Scratch.RevertOnto(ctx, oid, undo)
	if err != nil {
		return false, fmt.Errorf("undo the reverted culprits %s on %s: %w", strings.Join(undo, ", "), oid, err)
	}
	defer func() {
		if err := release(); err != nil {
			returnedErr = errors.Join(returnedErr, fmt.Errorf("release the scratch commit of %s: %w", oid, err))
		}
	}()

	res.Tests++
	return f.Tester.Test(ctx, scratch)
}
