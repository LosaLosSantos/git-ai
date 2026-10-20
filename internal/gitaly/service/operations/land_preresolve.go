package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	// current are the keys of the conflicting files at the latest tip the land looked at: of the
	// verdicts kept, only these are the land's now. A verdict of a file that has since changed (its
	// key is another) is stale, and decides nothing about the land.
	current map[landFileKey]bool
	// verifyAll says that a verification is configured: every resolution then needs a verified tree
	// to land, a rule's as well as a model's (15.9).
	verifyAll bool
}

func newLandPreResolution() *landPreResolution {
	return &landPreResolution{verdicts: landVerdicts{}, verified: map[git.ObjectID]bool{}}
}

// setCurrent records which files the land has now: the verdicts of other files, or of the same files
// as they were, are stale.
func (p *landPreResolution) setCurrent(files []*landFileState) {
	p.current = make(map[landFileKey]bool, len(files))
	for _, f := range files {
		p.current[landKeyOf(f.conflict)] = true
	}
}

// hasModel reports whether a verdict for a current file of the land is a model's: the land is a
// model's resolution that only a verified tree lands. A stale verdict does not count.
func (p *landPreResolution) hasModel() bool {
	if p == nil {
		return false
	}
	for k, v := range p.verdicts {
		if p.current[k] && v.file.FromModel() {
			return true
		}
	}
	return false
}

// mustVerify reports whether the land has a resolution whose landing needs a verified tree: a
// model's, or any when a verification is configured. A stale verdict does not count.
func (p *landPreResolution) mustVerify() bool {
	if p == nil {
		return false
	}
	if !p.verifyAll {
		return p.hasModel()
	}
	for k := range p.verdicts {
		if p.current[k] {
			return true
		}
	}
	return false
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
	// moved says that the branch moved under the batch that was landing a model's resolution until
	// the batch ran out of attempts: the tree it was about to land is another one, and the land goes
	// out again as it does for an unverified tree.
	moved bool
}

func (e *landNeedsResolution) Error() string {
	if e.moved {
		return fmt.Sprintf("the branch moved while the resolution of the land at tip %s was being landed", e.tip)
	}
	if e.unverified != "" {
		return fmt.Sprintf("the tree %s of the land at tip %s was not verified", e.unverified, e.tip)
	}
	return fmt.Sprintf("conflicting files to resolve at tip %s: %s", e.tip, strings.Join(e.paths, ", "))
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
	var moved *landTipMovedError
	switch {
	case err == nil, errors.Is(err, errLandNoConflicts):
		return nil
	case errors.As(err, &moved):
		// The branch moved while the verification waited for its turn: the verdicts are kept, and the
		// lane hands the land back with the tree of the new tip to be verified (nothing stale was).
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
//
// A land that had a conflict never ends on a bare deadline: whatever stopped it, the deadline, the
// cap on the rounds or a lane that kept handing it back, it ends with the merge conflict and its
// package (landFinalPass): the proposals for what is known, a model's files PROPOSED with the
// reason, and NOT_ATTEMPTED with the reason for the files that changed under it. Only a caller that
// gives up gets its cancellation.
func (s *Server) landResolving(ctx context.Context, in *gitalypb.UserLandRequest, committer git.Signature) (*gitalypb.UserLandResponse, error) {
	timeout, err := s.resolveCfg.TimeoutOrDefault()
	if err != nil {
		return nil, structerr.NewInternal("resolver timeout: %w", err)
	}
	if s.resolveCfg.Verify.Enabled {
		verifyTimeout, err := s.resolveCfg.Verify.TimeoutOrDefault()
		if err != nil {
			return nil, structerr.NewInternal("verify timeout: %w", err)
		}
		timeout += verifyTimeout
	}
	resolveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pre := newLandPreResolution()
	pre.verifyAll = s.resolveCfg.Verify.Enabled
	var need *landNeedsResolution
	// deadline reports whether the land ran out of its own timeout, and not its caller.
	deadline := func() bool {
		return ctx.Err() == nil && errors.Is(resolveCtx.Err(), context.DeadlineExceeded)
	}
	rounds := s.resolveCfg.ResolveRoundsOrDefault()
	for round := 1; round <= rounds; round++ {
		if err := s.landPreResolve(resolveCtx, in, committer, pre); err != nil {
			// The deadline ended a step of the round: the listing, the tree, the resolution or the
			// verification. A refusal of the policy is not a deadline.
			if deadline() && status.Code(err) != codes.PermissionDenied {
				var unfinished *landVerifyUnfinishedError
				reason := landStoppedReason(need, timeout)
				if errors.As(err, &unfinished) {
					reason = fmt.Sprintf("could not verify before the deadline of %s", timeout)
				}
				return s.landFinalPass(ctx, in, committer, pre, reason)
			}
			var unfinished *landVerifyUnfinishedError
			if errors.As(err, &unfinished) {
				// Not the deadline of the land: the caller gave up (or its own deadline passed).
				return nil, landCallerGaveUp(ctx, err)
			}
			return nil, err
		}
		resp, err := s.landOnce(ctx, in, committer, nil, pre)
		if !errors.As(err, &need) {
			return resp, err
		}
		if deadline() {
			return s.landFinalPass(ctx, in, committer, pre, landStoppedReason(need, timeout))
		}
	}
	return s.landFinalPass(ctx, in, committer, pre, landStoppedReason(need, timeout))
}

// landCallerGaveUp is the status of a land whose caller gave up, or whose own deadline passed, while
// it waited for a slot or verified: the caller's cancellation, not an error of the server.
func landCallerGaveUp(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return structerr.NewDeadlineExceeded("verifying the resolution: %w", err)
	case ctx.Err() != nil:
		return structerr.NewCanceled("verifying the resolution: %w", err)
	}
	return err
}

// landStoppedReason is why the land could not go on, as the files it leaves say: the tree it was
// about to land was not verified before the branch moved again, the files changed again before they
// could be resolved, or the time ran out before the conflicts were resolved at all.
func landStoppedReason(need *landNeedsResolution, timeout time.Duration) string {
	switch {
	case need != nil && (need.unverified != "" || need.moved):
		return "could not verify before the branch moved again"
	case need != nil:
		return "the files changed again before they could be resolved"
	}
	return fmt.Sprintf("the time ran out before the conflicts were resolved (deadline of %s)", timeout)
}

// landFinalPasses bounds the passes of landFinalPass: each is cheap, as it calls no resolver and runs
// no verification, so that losing the branch to another land four times in a row is a branch that
// moves by the millisecond. Nothing is verified in the lane either (15.9): a land that reaches it
// without a verified tree is handed back, and the pass ends it with the package.
const landFinalPasses = 4

// landFinalPass ends a land that could not go on (landResolving): the agent gets the package, not
// a deadline. A verified AI land is opportunistic, and losing the race is harmless. It pre-resolves
// once more with the request's context, in a new quarantine, verifying nothing and calling no
// resolver: the verdicts that are kept give the proposals (a model's files PROPOSED with the reason,
// the rules' files RESOLVED, or NOT_ATTEMPTED with the reason when a verification is configured and
// their tree could not be verified), the files without a verdict are NOT_ATTEMPTED with the reason, and
// the land fails with the package. That is every ending of a land that had a conflict: the package is
// what landPreResolve returns, and nothing else comes out of it but the refusal of the policy or an
// error of the server. When there is nothing to report as a conflict after all (the change landed,
// the merge is clean, or every file has a verdict a rule made), the lane answers, and lands.
//
// Between the pre-resolution and the lane the branch can move to a tip with other conflicts, and
// the lane hands the land back: the pass is made again, at most landFinalPasses times. A land that
// loses all of them ends as any land that lost the race, with the retryable "kept moving" of
// landKeptMovingError; the package of a conflict is not what it is about.
func (s *Server) landFinalPass(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	committer git.Signature,
	pre *landPreResolution,
	reason string,
) (*gitalypb.UserLandResponse, error) {
	pre.unverifiable = reason
	for pass := 1; ; pass++ {
		if err := s.landPreResolve(ctx, in, committer, pre); err != nil {
			return nil, err
		}
		if s.landFinalPassHook != nil {
			s.landFinalPassHook(pass)
		}
		resp, err := s.landOnce(ctx, in, committer, nil, pre)
		var needs *landNeedsResolution
		if !errors.As(err, &needs) {
			return resp, err
		}
		if pass >= landFinalPasses {
			return nil, landFinalPassGaveUp(in, needs, pass)
		}
	}
}

// landFinalPassGaveUp is the error of a land that lost every pass of the final pass: as any land
// that lost the race, the retryable "kept moving" FailedPrecondition with the ReferenceUpdate detail
// (landKeptMovingError). needs is the lane's last handing back, whose tip is the one the branch is
// at; the package of a conflict is not what it is about.
func landFinalPassGaveUp(in *gitalypb.UserLandRequest, needs *landNeedsResolution, passes int) error {
	return landKeptMovingError(in, landConflict{
		reference: git.NewReferenceNameFromBranchName(string(in.GetBranch())),
		// The tip the last pass resolved at is the one the branch moved away from; where it moved to is
		// not known here (the lane handed the land back without a compare-and-swap), so newOID is empty.
		oldOID: needs.tip,
		newOID: "",
		cause:  errors.New("the branch kept moving while the land was being ended"),
	}, passes)
}
