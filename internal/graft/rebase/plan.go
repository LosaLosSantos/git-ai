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
	// parents are the parents of every commit of the range tip..commit.
	parents map[git.ObjectID][]git.ObjectID
}

// Plan decides how commit lands onto tip, with the answers of checker.
//
// The listing of the commits that commit adds to tip (checker.List) may hold commits that tip
// reaches when committer dates are skewed, never fewer than the range (see revrange). Two facts
// are exact from such a listing, so they cost nothing more:
//
//   - The listing is empty exactly when the range is: commit is already landed.
//   - tip is a parent of a listed commit exactly when tip is reachable from commit. A listed commit
//     is reachable from commit, so tip is then reachable from commit; and on a path from commit down
//     to tip, the commit right above tip is in the range, so it is listed. That is a fast-forward.
//
// Whether the change has merge commits, whether it shares history with tip and which commits a
// rebase replays need the exact range, so a diverged change has its listing made exact by
// revrange.Refine first, which checks the bottoms of the listing once without skew.
func Plan(ctx context.Context, checker ancestry.Checker, tip, commit git.ObjectID) (LandPlan, error) {
	listed, err := checker.List(ctx, commit, []git.ObjectID{tip})
	if err != nil {
		return LandPlan{}, fmt.Errorf("listing the commits of the change: %w", err)
	}

	plan, err := planLanding(tip, commit, listed)
	if err != nil || plan.Kind() != LandDiverged {
		return plan, err
	}

	rng, err := revrange.Refine(ctx, checker, tip, commit, listed)
	if err != nil {
		return LandPlan{}, fmt.Errorf("making the range of the change exact: %w", err)
	}

	// The exact range is empty when every listed commit was reachable from tip: the commit is
	// already landed. It cannot be a fast-forward, which the listing would have shown.
	return planLanding(tip, commit, rng)
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
