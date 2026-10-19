package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
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
}

// landNeedsResolution is the answer of a lane that met conflicting files without a verdict: new
// files, or files whose sides changed since they were resolved. UserLand resolves them out of the
// lane and lands again.
type landNeedsResolution struct {
	tip, commit git.ObjectID
	paths       []string
}

func (e *landNeedsResolution) Error() string {
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

	l, _, err := s.landResolveAt(ctx, in, repo, tip, commitID, committer, pre.verdicts)
	switch {
	case err == nil, errors.Is(err, errLandNoConflicts):
		return nil
	}
	files := l.files[:min(len(l.files), landMaxPackageFiles)]
	paths := make([][]byte, 0, len(files))
	for _, f := range files {
		paths = append(paths, []byte(f.conflict.path))
	}
	s.logger.WithError(err).WithField("branch", string(in.GetBranch())).Info("graft: conflict resolution of land failed")
	return landWithPackage(ctx, landMergeConflictError(tip, commitID, paths, err), err)
}

// landResolving lands a request with resolve_conflicts (PLAN10 15.8): it pre-resolves out of the
// lane and lands; when the lane meets files without a verdict, it resolves only those, out of the
// lane, and lands again. The rounds are bounded by the resolver's timeout for the whole land, and
// by resolve_rounds, a safety cap. A round is lost only when another land changed one of the
// files, so the lands that contend for a file all land, one per round; a land that keeps losing
// to new arrivals fails at its deadline with the merge conflict, naming the files that kept
// changing.
func (s *Server) landResolving(ctx context.Context, in *gitalypb.UserLandRequest, committer git.Signature) (*gitalypb.UserLandResponse, error) {
	timeout, err := s.resolveCfg.TimeoutOrDefault()
	if err != nil {
		return nil, fmt.Errorf("resolver timeout: %w", err)
	}
	resolveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pre := &landPreResolution{verdicts: landVerdicts{}}
	var need *landNeedsResolution
	// deadline reports whether the land ran out of the resolver's timeout, and not its caller.
	deadline := func() bool {
		return ctx.Err() == nil && errors.Is(resolveCtx.Err(), context.DeadlineExceeded)
	}
	until := fmt.Sprintf("until the deadline of %s", timeout)
	rounds := s.resolveCfg.ResolveRoundsOrDefault()
	for round := 1; round <= rounds; round++ {
		if err := s.landPreResolve(resolveCtx, in, committer, pre); err != nil {
			// A land that lost a round to its files changing, and then ran out of its deadline
			// while it resolved them again, failed because they kept changing.
			if need != nil && deadline() {
				return nil, need.keptChanging(until)
			}
			return nil, err
		}
		resp, err := s.landOnce(ctx, in, committer, nil, pre)
		if !errors.As(err, &need) {
			return resp, err
		}
		if deadline() {
			return nil, need.keptChanging(until)
		}
	}
	return nil, need.keptChanging(fmt.Sprintf("after %d rounds", rounds))
}
