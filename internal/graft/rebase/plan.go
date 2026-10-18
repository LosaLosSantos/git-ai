package rebase

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/revrange"
	"github.com/feder-cr/git-ai/internal/graft/ancestry"
)

// LandKind is how a commit lands onto the tip of a branch.
type LandKind int

const (
	// LandAlreadyLanded means that the commit is reachable from the tip, the tip included.
	LandAlreadyLanded LandKind = iota + 1
	// LandFastForward means that the tip is reachable from the commit, so the commit becomes the
	// new tip.
	LandFastForward
	// LandDiverged means that neither is reachable from the other: the change must be rebased or
	// merged onto the tip.
	LandDiverged
)

var (
	// ErrMergesInRange is returned by LandPlan.Replay when the commits that the change adds to
	// the tip contain a merge commit, which a rebase cannot replay.
	ErrMergesInRange = errors.New("commit range contains merge commits")
	// ErrUnrelated is returned by LandPlan.Replay when the commit and the tip have no common
	// ancestor.
	ErrUnrelated = errors.New("commit shares no history with the tip")
)

// LandPlan is the decision of how a commit lands onto a tip, made from the range tip..commit. It
// answers what landing used to ask git one question at a time: whether the commit is already
// landed, whether it fast-forwards the tip, whether its range has merge commits, whether it shares
// history with the tip, and which commits a rebase replays.
type LandPlan struct {
	kind LandKind
	// commit is the commit that lands.
	commit git.ObjectID
	// parents are the parents of every commit of the range tip..commit. It is empty when
	// mergesInRange is set.
	parents map[git.ObjectID][]git.ObjectID
	// mergesInRange is set when the range is known to hold a merge commit without being listed
	// exactly (see Plan).
	mergesInRange bool
}

// Plan decides how commit lands onto tip, with the answers of checker, in at most four
// questions whatever the size of the history.
//
// The listing of the commits that commit adds to tip (checker.List) may hold commits that tip
// reaches when committer dates are skewed, never fewer than the range (see revrange). Two facts
// are exact from such a listing, so they cost nothing more:
//
//   - An empty listing means an empty range: commit is already landed. Not conversely: with skew
//     an already landed commit can have a non-empty listing, and the merge bases below settle it.
//   - tip is a parent of a listed commit exactly when tip is reachable from commit. A listed commit
//     is reachable from commit, so tip is then reachable from commit; and on a path from commit down
//     to tip, the commit right above tip is in the range, so it is listed. That is a fast-forward.
//
// Otherwise the change diverges, and one question for all the bottoms of the listing
// (checker.ReachedBottoms) tells whether the listing is exact. Without skew it is, and the plan is
// made from it. With skew:
//
//   - The merge bases of tip and commit are commit alone exactly when tip reaches commit: the
//     change is already landed.
//   - Otherwise only a rebase needs more than the kind, and a rebase needs a range without merge
//     commits. Such a range is a chain from commit down to the one commit p where it leaves the
//     range; p is a common ancestor, and every common ancestor is reachable from commit through
//     the chain, so through p: p is the only merge base. A listing of commit that excludes p then
//     lists the chain and nothing else, because every path from commit runs through p. So the
//     listing below the merge bases is either such a chain, and exact, or the range has a merge
//     commit, and a rebase refuses the change (ErrMergesInRange) without the exact range.
func Plan(ctx context.Context, checker ancestry.Checker, tip, commit git.ObjectID) (LandPlan, error) {
	listed, err := checker.List(ctx, commit, []git.ObjectID{tip})
	if err != nil {
		return LandPlan{}, fmt.Errorf("listing the commits of the change: %w", err)
	}

	plan, err := planLanding(tip, commit, listed)
	if err != nil || plan.Kind() != LandDiverged {
		return plan, err
	}

	bottoms, err := revrange.Bottoms(listed)
	if err != nil {
		return LandPlan{}, fmt.Errorf("listing the commits of the change: %w", err)
	}
	reached, err := checker.ReachedBottoms(ctx, tip, bottoms)
	if err != nil {
		return LandPlan{}, fmt.Errorf("checking the listing of the change: %w", err)
	}
	if len(reached) == 0 {
		return plan, nil
	}

	revrange.CountSkewedListing()

	bases, err := checker.MergeBases(ctx, tip, commit)
	if err != nil {
		return LandPlan{}, fmt.Errorf("finding the merge bases of the change: %w", err)
	}
	switch {
	case len(bases) == 0:
		// A bottom of the listing is reachable from tip, and every listed commit from commit.
		return LandPlan{}, fmt.Errorf("%s and %s have no merge base although they share history", tip, commit)
	case len(bases) == 1 && bases[0] == commit:
		return LandPlan{kind: LandAlreadyLanded, commit: commit}, nil
	case len(bases) > 1:
		// A range without merge commits leaves through one commit, the only merge base.
		return LandPlan{kind: LandDiverged, commit: commit, mergesInRange: true}, nil
	}

	relisted, err := checker.List(ctx, commit, bases)
	if err != nil {
		return LandPlan{}, fmt.Errorf("listing the commits of the change: %w", err)
	}
	if !isChainDownTo(relisted, commit, bases[0]) {
		return LandPlan{kind: LandDiverged, commit: commit, mergesInRange: true}, nil
	}

	return planLanding(tip, commit, relisted)
}

// isChainDownTo reports whether listed is a chain of commits with one parent each, from commit
// down to a commit whose parent is base, and nothing else.
func isChainDownTo(listed []ancestry.RangeCommit, commit, base git.ObjectID) bool {
	parents := make(map[git.ObjectID][]git.ObjectID, len(listed))
	for _, c := range listed {
		parents[c.ID] = c.Parents
	}

	visited := 0
	for id := commit; ; {
		ps, ok := parents[id]
		if !ok || len(ps) != 1 || visited == len(listed) {
			return false
		}
		visited++

		if ps[0] == base {
			return visited == len(listed)
		}
		id = ps[0]
	}
}

// planLanding decides how commit lands onto tip from rng, the exact range tip..commit:
//
//   - The range is empty exactly when commit is reachable from tip: LandAlreadyLanded.
//   - tip is a parent of a commit of the range exactly when tip is reachable from commit: on a path
//     from commit down to tip, the commit right above tip is not reachable from tip, so it is in the
//     range. That is LandFastForward, whatever merge commits the range has.
//   - Otherwise LandDiverged.
//
// A non-empty range must contain commit and list every commit once; anything else is an error,
// because the range does not come from tip..commit. Only Plan calls it, with a listing for the two
// facts that a listing gives exactly and with the exact range otherwise.
func planLanding(tip, commit git.ObjectID, rng []ancestry.RangeCommit) (LandPlan, error) {
	if len(rng) == 0 {
		return LandPlan{kind: LandAlreadyLanded, commit: commit}, nil
	}

	parents := make(map[git.ObjectID][]git.ObjectID, len(rng))
	fastForward := false
	for _, c := range rng {
		if _, ok := parents[c.ID]; ok {
			return LandPlan{}, fmt.Errorf("range %s..%s lists commit %s twice", tip, commit, c.ID)
		}
		parents[c.ID] = c.Parents

		for _, parent := range c.Parents {
			if parent == tip {
				fastForward = true
			}
		}
	}

	if _, ok := parents[commit]; !ok {
		return LandPlan{}, fmt.Errorf("range %s..%s does not contain the commit", tip, commit)
	}

	if fastForward {
		return LandPlan{kind: LandFastForward, commit: commit}, nil
	}

	return LandPlan{kind: LandDiverged, commit: commit, parents: parents}, nil
}

// Kind returns how the commit lands.
func (p LandPlan) Kind() LandKind {
	return p.kind
}

// Replay returns the commits that a rebase of a LandDiverged plan replays onto the tip, oldest
// first. These are all the commits of the range: git rev-list --right-only --no-merges --topo-order
// --reverse tip...commit without --cherry-pick lists the same commits in the same order.
//
// It returns ErrMergesInRange when a commit of the range has more than one parent. Otherwise every
// commit of the range has at most one parent, and the range is the chain from the commit down its
// parents. The bottom of the chain either has a parent, which is outside of the range and therefore
// reachable from the tip, so the two share history, or it is a root: then no ancestor of the commit
// is reachable from the tip, and Replay returns ErrUnrelated.
func (p LandPlan) Replay() ([]git.ObjectID, error) {
	if p.kind != LandDiverged {
		return nil, fmt.Errorf("replay of a landing that is not diverged (kind %d)", p.kind)
	}

	if p.mergesInRange {
		return nil, ErrMergesInRange
	}

	for _, parents := range p.parents {
		if len(parents) > 1 {
			return nil, ErrMergesInRange
		}
	}

	chain := make([]git.ObjectID, 0, len(p.parents))
	for id := p.commit; ; {
		if len(chain) == len(p.parents) {
			// A history is acyclic, so a chain can only be longer than the range when the range
			// does not come from git.
			return nil, fmt.Errorf("range of %s has a cycle", p.commit)
		}
		chain = append(chain, id)

		parents := p.parents[id]
		if len(parents) == 0 {
			return nil, ErrUnrelated
		}

		if _, inRange := p.parents[parents[0]]; !inRange {
			break
		}
		id = parents[0]
	}

	// Every commit of the range is reachable from the commit through commits of the range, so
	// without merge commits the chain is the whole range.
	if len(chain) != len(p.parents) {
		return nil, fmt.Errorf("range of %s is not a chain: %d of %d commits reached", p.commit, len(chain), len(p.parents))
	}

	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	return chain, nil
}
