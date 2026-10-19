package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// A land with resolve_conflicts is resolved out of the branch's lane (PLAN10 15.8): the resolver
// is called before the lane, and a file's verdict is kept by the file's sides, so that the lane,
// whatever its tip, applies every verdict whose file it finds unchanged and never calls the
// resolver itself.

// landFileKey identifies a conflicting file by what its verdict depends on: its path and its
// three sides, each by object ID and mode (empty for an absent side).
type landFileKey struct {
	path                           string
	baseOID, oursOID, theirsOID    git.ObjectID
	baseMode, oursMode, theirsMode string
}

// landKeyOf returns the key of a conflicting file.
func landKeyOf(c landFileConflict) landFileKey {
	k := landFileKey{path: c.path}
	if c.base != nil {
		k.baseOID, k.baseMode = c.base.oid, c.base.mode
	}
	if c.ours != nil {
		k.oursOID, k.oursMode = c.ours.oid, c.ours.mode
	}
	if c.theirs != nil {
		k.theirsOID, k.theirsMode = c.theirs.oid, c.theirs.mode
	}
	return k
}

// landVerdict is a landable verdict on a file: the resolution the resolver gave it, and the name
// of that resolver.
type landVerdict struct {
	file     resolver.ResponseFile
	resolver string
}

// landVerdicts are the landable verdicts of a land, by file key.
type landVerdicts map[landFileKey]landVerdict

// landPreResolution is what the pre-resolution of a land hands to its lane.
type landPreResolution struct {
	verdicts landVerdicts
	// verified are the trees that verify.command passed on, out of the lane (15.9): a tree, by its
	// object ID, so that a tip that moved to the same tree keeps the verification.
	verified map[git.ObjectID]bool
	// unverifiable, when set, is why the land has run out of time to verify a model's resolution:
	// the pre-resolution then verifies nothing and calls no resolver, and the files of the model
	// become proposals with this reason.
	unverifiable string
}

func newLandPreResolution() *landPreResolution {
	return &landPreResolution{verdicts: landVerdicts{}, verified: map[git.ObjectID]bool{}}
}

// markVerified records that verify.command passed on tree.
func (p *landPreResolution) markVerified(tree git.ObjectID) {
	if p.verified == nil {
		p.verified = map[git.ObjectID]bool{}
	}
	p.verified[tree] = true
}

// isVerified reports whether verify.command passed on tree, out of the lane.
func (p *landPreResolution) isVerified(tree git.ObjectID) bool {
	return p != nil && tree != "" && p.verified[tree]
}

// landNeedsResolution is the answer of a lane that cannot land the land as it stands: it met
// conflicting files without a verdict (new files, or files whose sides changed since they were
// resolved), or the tree it would land is not one that verify.command passed on (unverified: the
// tip moved). UserLand resolves and verifies again out of the lane, and lands again.
type landNeedsResolution struct {
	tip, commit git.ObjectID
	paths       []string
	unverified  git.ObjectID
}

func (e *landNeedsResolution) Error() string {
	if e.unverified != "" {
		return fmt.Sprintf("the tree %s of the land at tip %s was not verified", e.unverified, e.tip)
	}
	return fmt.Sprintf("conflicting files to resolve at tip %s: %s", e.tip, strings.Join(e.paths, ", "))
}

// keptChanging is the merge conflict of a land whose files kept changing until it gave up, when
// says when.
func (e *landNeedsResolution) keptChanging(when string) error {
	paths := make([][]byte, 0, len(e.paths))
	for _, p := range e.paths {
		paths = append(paths, []byte(p))
	}
	return landMergeConflictError(e.tip, e.commit, paths,
		fmt.Errorf("files kept changing while the land was resolved, %s: %s", when, strings.Join(e.paths, ", ")))
}

// landPreResolve resolves, out of the lane and in a quarantine it discards, the conflicting files
// of the land at the branch's current tip that have no verdict in pre yet. It returns nil when the
// lane can take the land: the verdicts are in pre, or there is nothing to resolve (no branch, the
// change already landed, a clean merge: the lane answers those). Otherwise it returns the error of
// the land: the merge conflict with the package (the proposals kept), or the policy's refusal.
func (s *Server) landPreResolve(ctx context.Context, in *gitalypb.UserLandRequest, committer git.Signature, pre *landPreResolution) error {
	// The lane resolves only the merge conflict of a rebase or a merge: a fast-forward-only land
	// that cannot fast-forward is refused without resolution.
	if in.GetStrategy() == gitalypb.UserLandRequest_STRATEGY_FF_ONLY {
		return nil
	}
	_, repo, cleanup, err := s.quarantinedRepo(ctx, in.GetRepository())
	if err != nil {
		return err
	}
	defer cleanup()

	tip, err := repo.ResolveRevision(ctx, git.NewReferenceNameFromBranchName(string(in.GetBranch())).Revision())
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return nil
		}
		return fmt.Errorf("reading the branch: %w", err)
	}
	commitID, err := repo.ResolveRevision(ctx, git.Revision(in.GetCommitId()+"^{commit}"))
	if err != nil {
		return nil // the lane refuses an invalid change
	}
	if landed, err := repo.IsAncestor(ctx, commitID.Revision(), tip.Revision()); err != nil {
		return fmt.Errorf("checking whether the change already landed: %w", err)
	} else if landed {
		return nil
	}

	l, _, err := s.landResolveAt(ctx, in, repo, tip, commitID, committer, pre)
	var unfinished *landVerifyUnfinishedError
	var needs *landNeedsResolution
	switch {
	case err == nil, errors.Is(err, errLandNoConflicts):
		return nil
	case errors.As(err, &unfinished), errors.As(err, &needs):
		// Not the land's failure: UserLand decides what a verification that ran out of time, or a
		// land that still needs files resolved, comes to.
		return err
	}
	// Only a conflict that the resolution left unresolved is a merge conflict, and only the policy's
	// refusal is a refusal. Any other error (a git command that failed, a caller that gave up) means
	// the conflicts could not be listed or resolved, and goes out with its cause.
	var unresolved *landUnresolvedError
	var denied *landPolicyDeniedError
	if !errors.As(err, &unresolved) && !errors.As(err, &denied) {
		switch {
		case errors.Is(err, context.Canceled):
			return structerr.NewCanceled("resolving the conflicts of the land: %w", err)
		case errors.Is(err, context.DeadlineExceeded):
			return structerr.NewDeadlineExceeded("resolving the conflicts of the land: %w", err)
		}
		s.logger.WithError(err).WithField("branch", string(in.GetBranch())).Error("graft: the conflicts of the land could not be resolved")
		return structerr.NewInternal("resolving the conflicts of the land: %w", err)
	}
	files := l.files[:min(len(l.files), landMaxPackageFiles)]
	paths := make([][]byte, 0, len(files))
	for _, f := range files {
		paths = append(paths, []byte(f.conflict.path))
	}
	s.logger.WithError(err).WithField("branch", string(in.GetBranch())).Info("graft: conflict resolution of land failed")
	return landWithPackage(ctx, landMergeConflictError(tip, commitID, paths, err), err)
}

// landResolving lands a request with resolve_conflicts (PLAN10 15.8, 15.9): it pre-resolves out of
// the lane and lands; when the lane meets files without a verdict, or a tree that verify.command
// did not pass on, it resolves and verifies only what changed, out of the lane, and lands again.
// The rounds are bounded by the deadline of the land, the resolver's timeout plus the verification's
// when one is configured, and by resolve_rounds, a safety cap. A round is lost only when another
// land changed the files or the tip, so the lands that contend for a file all land, one per round.
// A land that runs out of its deadline fails with the merge conflict naming the files that kept
// changing; but a model's resolution that could not be verified in time does not fail the agent: its
// files become proposals, with the reason, in the package (landUnverified).
func (s *Server) landResolving(ctx context.Context, in *gitalypb.UserLandRequest, committer git.Signature) (*gitalypb.UserLandResponse, error) {
	timeout, err := s.resolveCfg.TimeoutOrDefault()
	if err != nil {
		return nil, fmt.Errorf("resolver timeout: %w", err)
	}
	if s.resolveCfg.Verify.Enabled {
		verifyTimeout, err := s.resolveCfg.Verify.TimeoutOrDefault()
		if err != nil {
			return nil, fmt.Errorf("verify timeout: %w", err)
		}
		timeout += verifyTimeout
	}
	resolveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pre := newLandPreResolution()
	var need *landNeedsResolution
	// deadline reports whether the land ran out of its own timeout, and not its caller.
	deadline := func() bool {
		return ctx.Err() == nil && errors.Is(resolveCtx.Err(), context.DeadlineExceeded)
	}
	until := fmt.Sprintf("until the deadline of %s", timeout)
	// giveUp ends a land that cannot go on: a model's resolution that was not verified in time goes
	// to the agent as proposals; anything else is the merge conflict of files that kept changing.
	giveUp := func(when, unverifiedReason string) (*gitalypb.UserLandResponse, error) {
		if unverifiedReason != "" {
			return s.landUnverified(ctx, in, committer, pre, unverifiedReason, need, when)
		}
		return nil, need.keptChanging(when)
	}
	rounds := s.resolveCfg.ResolveRoundsOrDefault()
	for round := 1; round <= rounds; round++ {
		if err := s.landPreResolve(resolveCtx, in, committer, pre); err != nil {
			var unfinished *landVerifyUnfinishedError
			switch {
			case errors.As(err, &unfinished) && deadline():
				return s.landUnverified(ctx, in, committer, pre, fmt.Sprintf("could not verify before the deadline of %s", timeout), need, until)
			case need != nil && deadline():
				// A land that lost a round to its files or its tip changing, and then ran out of its
				// deadline while it resolved or verified again, failed because they kept changing.
				return giveUp(until, landNotVerifiedReason(need))
			}
			return nil, err
		}
		resp, err := s.landOnce(ctx, in, committer, nil, pre)
		if !errors.As(err, &need) {
			return resp, err
		}
		if deadline() {
			return giveUp(until, landNotVerifiedReason(need))
		}
	}
	return giveUp(fmt.Sprintf("after %d rounds", rounds), landNotVerifiedReason(need))
}

// landNotVerifiedReason is why a model's files become proposals when the lane handed the land back
// for a tree that was not verified: the branch moved on after the verification. It is empty when the
// lane asked for files to be resolved.
func landNotVerifiedReason(need *landNeedsResolution) string {
	if need != nil && need.unverified != "" {
		return "could not verify before the branch moved again"
	}
	return ""
}

// landUnverified ends a land whose model resolution could not be verified in time. A verified AI
// land is opportunistic, and losing the race is harmless: the agent gets the proposals. It
// pre-resolves once more, with the request's context, in a new quarantine, verifying nothing and
// calling no resolver (the verdicts are kept): the files of the model are PROPOSED with the reason,
// the proposals are kept, and the land fails with the package. need, when set, is the state the
// lane handed back, for the merge conflict if the land cannot be reported as proposals after all.
func (s *Server) landUnverified(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	committer git.Signature,
	pre *landPreResolution,
	reason string,
	need *landNeedsResolution,
	when string,
) (*gitalypb.UserLandResponse, error) {
	pre.unverifiable = reason
	err := s.landPreResolve(ctx, in, committer, pre)
	var needs *landNeedsResolution
	switch {
	case errors.As(err, &needs):
		if need == nil {
			need = needs
		}
		return nil, need.keptChanging(when)
	case err != nil:
		return nil, err
	}
	// The land is no longer a conflict to resolve (the change landed, or the merge is clean): the
	// lane answers it.
	resp, err := s.landOnce(ctx, in, committer, nil, pre)
	if errors.As(err, &needs) {
		return nil, needs.keptChanging(when)
	}
	return resp, err
}
