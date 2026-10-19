package operations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// landResolutionSyntaxUncheckedTotal counts the files of agents' resolutions that landed without a
// syntax verdict: the resolver's grammar cannot read a side of them.
var landResolutionSyntaxUncheckedTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_land_resolution_syntax_unchecked_total",
	Help: "Files of agents' resolutions that landed without a syntax verdict, because a side does not parse with the resolver's grammar.",
})

// landAgentResolver is the resolver name of an agent's resolution in the Graft-Resolved-By trailer.
func landAgentResolver(actor string) string {
	if actor == "" {
		return "agent"
	}
	return "agent/" + actor
}

// landReview is an agent's resolution held against the merge it resolves, as landReviewResolution
// leaves it: the conflicting files, what resolves each, and whether a rule of an answer rejected a
// file (its outcome says which).
type landReview struct {
	listing  landListing
	resolved map[string]resolver.ResponseFile
	actor    string
	rejected bool
}

// landReviewResolution holds the resolution of the request against the merge of tip and commitID:
// it is accepted only when it was made against this tip and this commit and resolves exactly the
// paths that conflict, each once, and every file gets the acceptance rules of a resolver answer.
// A refusal is the error (a *landUnresolvedError with its package, or a gRPC error); a file that
// breaks a rule of an answer is REJECTED in the review.
func (s *Server) landReviewResolution(ctx context.Context, in *gitalypb.UserLandRequest, repo *localrepo.Repo, tip, commitID git.ObjectID) (landReview, error) {
	r := in.GetResolution()
	if r.GetCommit() != commitID.String() {
		return landReview{}, structerr.NewInvalidArgument("resolution is for commit %q, not commit_id %q", r.GetCommit(), commitID)
	}

	l, err := landListConflicts(ctx, repo, tip, commitID, s.resolveCfg.MaxFileBytesOrDefault())
	if errors.Is(err, errLandNoConflicts) {
		return landReview{}, structerr.NewFailedPrecondition("the merge of tip %s and commit %s has no conflicts, so there is nothing to resolve: land without a resolution", tip, commitID)
	}
	if err != nil {
		return landReview{}, err
	}
	if len(l.files) == 0 {
		return landReview{}, l.notAttempted(landNoConflictingFiles, nil)
	}
	// A merge with more conflicting files than a package lists cannot be resolved through a land,
	// whatever the resolution: its package says so, and the land refuses it before reading anything.
	if len(l.files) > landMaxPackageFiles {
		return landReview{}, l.notAttempted(fmt.Sprintf("the merge has %d conflicting files, more than the %d that a conflict package lists: it cannot be resolved through a land", len(l.files), landMaxPackageFiles), nil)
	}
	if r.GetTip() != tip.String() {
		return landReview{}, l.notAttempted(fmt.Sprintf("the branch tip moved from %s to %s since the resolution was made: resolve the conflicts of this package", r.GetTip(), tip), nil)
	}
	if l.bases > 1 {
		return landReview{}, l.notAttempted(fmt.Sprintf("the change has %d merge bases (a criss-cross history), which are not resolved", l.bases), nil)
	}

	// The merge commit names the actor and every resolved path in its trailers: a value that cannot
	// be one trailer line is refused before anything else, and resolving the path again cannot help.
	actor := identity.Actor(ctx, "")
	if !landTrailerValueValid(landAgentResolver(actor)) {
		return landReview{}, structerr.NewFailedPrecondition("the actor %q cannot be recorded in the %s trailer: it must be one line of printable UTF-8", actor, landTrailerResolvedBy)
	}
	for _, f := range l.files {
		if !landTrailerValueValid(f.conflict.path) {
			return landReview{}, structerr.NewFailedPrecondition("the conflicting path %q cannot be recorded in the %s trailer: it must be one line of printable UTF-8, so its conflict cannot be resolved through a land", f.conflict.path, landTrailerResolvedPath)
		}
	}

	resolved, reason := landResolutionFiles(r, l.files)
	if reason != "" {
		return landReview{}, l.notAttempted(reason, nil)
	}

	review := landReview{listing: l, resolved: resolved, actor: actor}
	maxBytes := s.resolveCfg.MaxFileBytesOrDefault()
	for _, f := range l.files {
		rf := resolved[f.conflict.path]
		err := resolver.ValidateFile(rf, maxBytes)
		if err == nil {
			err = landModeOfSides(f.conflict, rf)
		}
		if err != nil {
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_REJECTED, err.Error()
			review.rejected = true
		}
	}
	return review, nil
}

// landPrecheck is the check of an agent's resolution, run before the land enters its lane
// (landPrecheckResolution): for the tip and the commit it was run against, the result.
type landPrecheck struct {
	tip, commit git.ObjectID
	result      landCheckResult
}

// landPrecheckResolution runs the resolver's checks of the request's resolution before the land
// enters its lane, so that no branch is held while an external process runs: the lane only applies
// the verdicts (landWithResolution). The check is run against the tip the resolution was made
// against, which is the only tip the land accepts it at; if the branch has moved by the time the
// lane runs the land, the land refuses the resolution with a fresh package, as it would anyway.
// The whole check has one deadline, the resolver's timeout: every check request uses what remains
// of it, and its expiry fails the check closed.
//
// It returns nil when there is nothing to check: no resolver, or a resolution the land refuses
// before checking (the lane reports why).
func (s *Server) landPrecheckResolution(ctx context.Context, in *gitalypb.UserLandRequest, repo *localrepo.Repo) (*landPrecheck, error) {
	if s.conflictResolver == nil {
		return nil, nil
	}
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, structerr.NewInternal("detecting object hash: %w", err)
	}
	tip, err := objectHash.FromHex(in.GetResolution().GetTip())
	if err != nil {
		return nil, nil
	}
	commitID, err := repo.ResolveRevision(ctx, git.Revision(in.GetCommitId()+"^{commit}"))
	if err != nil {
		return nil, nil
	}
	review, err := s.landReviewResolution(ctx, in, repo, tip, commitID)
	if err != nil || review.rejected {
		// The lane refuses it the same way, before any check.
		return nil, nil
	}

	timeout, err := s.resolveCfg.TimeoutOrDefault()
	if err != nil {
		return nil, structerr.NewInternal("resolver timeout: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := s.landCheckResolution(ctx, in, repo, review.listing, review.actor, review.resolved)
	if err != nil {
		return nil, err
	}
	return &landPrecheck{tip: tip, commit: commitID, result: result}, nil
}

// landWithResolution lands commitID onto tip with the caller's resolution of the conflicts
// (UserLandRequest.resolution, PLAN10 3.3 and 12), held against the merge by
// landReviewResolution. When a resolver is configured, the files that may be sent to it must pass
// its checks (the check operation: the syntax check and the configured checks), which ran before
// the lane (precheck, for this tip and commit): a failing or failed check fails the land, and a
// resolution without a check for this tip and commit fails closed. Nothing lands partially: an
// unaccepted resolution fails with a fresh conflict package (*landUnresolvedError).
//
// The merge commit carries Graft-Resolved-By: agent/<actor> and, when the resolver checked the
// resolution, Graft-Checked-By: <resolver>.
func (s *Server) landWithResolution(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	tip, commitID git.ObjectID,
	committer git.Signature,
	precheck *landPrecheck,
) (landResolution, error) {
	review, err := s.landReviewResolution(ctx, in, repo, tip, commitID)
	if err != nil {
		return landResolution{}, err
	}
	l, rejected := review.listing, review.rejected

	checkedBy, syntaxUnchecked := "", 0
	if !rejected && s.conflictResolver != nil {
		if precheck == nil || precheck.tip != tip || precheck.commit != commitID {
			return landResolution{}, l.notAttempted(fmt.Sprintf("the resolution could not be checked: it was not checked against tip %s and commit %s before the land", tip, commitID), nil)
		}
		res := precheck.result
		for _, f := range l.files {
			v, ok := res.verdicts[f.conflict.path]
			if !ok {
				continue
			}
			if v.rejected != "" {
				f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_REJECTED, v.rejected
			}
			f.syntaxUnchecked = v.syntaxUnchecked
		}
		if res.failure != "" {
			// The files checked before the check failed keep their verdict; the others were not
			// attempted.
			for _, f := range l.files {
				if v, ok := res.verdicts[f.conflict.path]; !ok {
					f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, ""
				} else if v.rejected == "" {
					f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_RESOLVED, ""
				}
			}
			reason := "the resolution could not be checked: " + res.failure
			return landResolution{}, l.unresolved(reason, reason, res.failureErr)
		}
		checkedBy = res.checkedBy
		for _, v := range res.verdicts {
			if v.syntaxUnchecked {
				syntaxUnchecked++
			}
		}
	}
	for _, f := range l.files {
		if f.outcome == gitalypb.LandConflictFile_OUTCOME_REJECTED {
			rejected = true
		}
	}
	if rejected {
		for _, f := range l.files {
			if f.outcome != gitalypb.LandConflictFile_OUTCOME_REJECTED {
				f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_RESOLVED, ""
			}
		}
		return landResolution{}, l.unresolved("", "the resolution was rejected: "+landUnresolvedCause(l.files), nil)
	}

	resp := &resolver.Response{Version: resolver.ProtocolVersion, Resolver: landAgentResolver(review.actor)}
	for _, f := range l.files {
		resp.Files = append(resp.Files, review.resolved[f.conflict.path])
	}
	res, err := s.landWriteResolution(ctx, in, repo, tip, commitID, l.mergedTree, committer, review.actor, resp, checkedBy)
	res.syntaxUnchecked = syntaxUnchecked
	return res, l.verifyRejected(err)
}

// landResolutionFiles maps the files of the resolution to the conflicting paths. It returns a
// reason when the resolution does not resolve exactly the conflicting paths, each once.
func landResolutionFiles(r *gitalypb.LandResolution, files []*landFileState) (map[string]resolver.ResponseFile, string) {
	conflicting := make(map[string]bool, len(files))
	var want []string
	for _, f := range files {
		conflicting[f.conflict.path] = true
		want = append(want, f.conflict.path)
	}
	resolved := make(map[string]resolver.ResponseFile, len(r.GetFiles()))
	for _, rf := range r.GetFiles() {
		p := string(rf.GetPath())
		if !conflicting[p] {
			return nil, fmt.Sprintf("the resolution names %q, which does not conflict at this tip", p)
		}
		if _, dup := resolved[p]; dup {
			return nil, fmt.Sprintf("the resolution names %q more than once", p)
		}
		out := resolver.ResponseFile{Path: p, Mode: rf.GetMode(), Content: rf.GetContent()}
		switch rf.GetAction() {
		case gitalypb.LandResolvedFile_ACTION_WRITE:
			out.Action = resolver.ActionWrite
		case gitalypb.LandResolvedFile_ACTION_DELETE:
			out.Action = resolver.ActionDelete
		default:
			out.Action = rf.GetAction().String()
		}
		resolved[p] = out
	}
	if len(resolved) != len(conflicting) {
		var missing []string
		for _, p := range want {
			if _, ok := resolved[p]; !ok {
				missing = append(missing, p)
			}
		}
		slices.Sort(missing)
		return nil, fmt.Sprintf("the resolution does not resolve %s", strings.Join(missing, ", "))
	}
	return resolved, ""
}

// landCheckVerdict is the verdict of the resolver's check on one file.
type landCheckVerdict struct {
	// rejected is the reason of a check the file failed, empty when it passed.
	rejected        string
	syntaxUnchecked bool
}

// landCheckResult is what the checks of an agent's resolution found: the checker's name (for the
// Graft-Checked-By trailer, "" when no file could be sent), the verdict of every file checked, and,
// when a check could not run, why (the check fails closed: the land does not proceed).
type landCheckResult struct {
	checkedBy  string
	verdicts   map[string]landCheckVerdict
	failure    string
	failureErr error
}

// landCheckBatchBytes bounds the file data (the sides and the resolution) of one check request,
// resolver.CheckBatchBytes. A test may set a smaller bound on the server (checkBatchBytes).
func (s *Server) landCheckBatchBytes() int64 {
	if s.checkBatchBytes > 0 {
		return s.checkBatchBytes
	}
	return resolver.CheckBatchBytes
}

// landCheckResolution runs the resolver's checks on the files of the resolution that may be sent
// to the resolver (landClassify). The files go in check requests of at most landCheckBatchBytes of
// file data each, so that neither the server nor graft-resolve holds more than one bounded
// request at a time: each batch is read, checked and released before the next, and every batch must
// name the same checker. The listing and the attributes of the trees are read once for all batches.
// The requests carry no commit message: the checks judge the files. A check that cannot run (ctx
// expired included) stops the checks: the result says why, with the verdicts found before.
func (s *Server) landCheckResolution(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	l landListing,
	actor string,
	resolved map[string]resolver.ResponseFile,
) (landCheckResult, error) {
	result := landCheckResult{verdicts: map[string]landCheckVerdict{}}
	batches, err := landCheckBatches(ctx, repo, l.files, resolved, s.landCheckBatchBytes())
	if err != nil {
		return result, err
	}
	cache := newLandClassifyCache(l.files)

	var unchecked []string
	for _, batch := range batches {
		if err := s.landClassify(ctx, repo, batch, l.base, l.tip, l.commitID, cache); err != nil {
			return result, err
		}
		req := resolver.Request{
			Repository: resolver.Repository{
				Storage:      in.GetRepository().GetStorageName(),
				RelativePath: in.GetRepository().GetRelativePath(),
			},
			Branch: string(in.GetBranch()),
			Tip:    l.tip.String(),
			Commit: l.commitID.String(),
			Actor:  actor,
		}
		if l.hasBase {
			req.MergeBase = l.base.String()
		}
		var checked []*landFileState
		for _, f := range batch {
			if !f.sent() {
				// The file may not go to the resolver: it gets the server's rules only.
				unchecked = append(unchecked, fmt.Sprintf("%s (%s)", f.conflict.path, f.reason))
				f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_UNSPECIFIED, ""
				continue
			}
			req.Files = append(req.Files, landRequestFile(f))
			req.Resolution = append(req.Resolution, resolved[f.conflict.path])
			checked = append(checked, f)
		}
		// The sides of the batch are released once it is checked: the next batch reads its own, and
		// a package reads again, within its own bounds, what its hunks need.
		release := func() {
			for _, f := range batch {
				f.sides, f.read = [3][]byte{}, false
			}
		}
		if len(checked) == 0 {
			release()
			continue
		}

		resp, err := s.conflictResolver.Check(ctx, req)
		release()
		switch {
		case err != nil:
			result.failure, result.failureErr = err.Error(), err
		case !landResolverNameValid(resp.Resolver):
			result.failure = fmt.Sprintf("the check names an invalid resolver %q", resp.Resolver)
		case result.checkedBy != "" && resp.Resolver != result.checkedBy:
			result.failure = fmt.Sprintf("its checks named the resolvers %q and %q", result.checkedBy, resp.Resolver)
		}
		if result.failure != "" {
			return result, nil
		}
		result.checkedBy = resp.Resolver
		for i, v := range resp.Files {
			verdict := landCheckVerdict{syntaxUnchecked: v.OK && v.Syntax == resolver.SyntaxUnchecked}
			if !v.OK {
				verdict.rejected = v.Reason
			}
			result.verdicts[checked[i].conflict.path] = verdict
		}
	}
	if len(result.verdicts) == 0 {
		s.logger.WithField("unchecked", unchecked).InfoContext(ctx, "graft: no file of the agent's resolution may be sent to the resolver's checks")
		return result, nil
	}
	s.logger.WithFields(log.Fields{
		"checked_by":     result.checkedBy,
		"checked":        len(result.verdicts),
		"check_requests": len(batches),
		"not_checked":    unchecked,
	}).InfoContext(ctx, "graft: agent's resolution checked")
	return result, nil
}

// landCheckBatches splits the files into batches of at most budget bytes of file data: the sizes
// of their sides (from the object info, before reading) and of their resolution, plus what a file
// adds to the request around them (resolver.CheckFileOverheadBytes for its path). A file larger
// than the budget is a batch of its own, which fits a check request since max_file_bytes is at
// most resolver.MaxCheckFileBytes.
func landCheckBatches(ctx context.Context, repo *localrepo.Repo, files []*landFileState, resolved map[string]resolver.ResponseFile, budget int64) ([][]*landFileState, error) {
	var batches [][]*landFileState
	var cur []*landFileState
	var size int64
	for _, f := range files {
		n := int64(len(resolved[f.conflict.path].Content)) + resolver.CheckFileOverheadBytes(f.conflict.path)
		for _, b := range []*landBlob{f.conflict.base, f.conflict.ours, f.conflict.theirs} {
			if b == nil || (b.mode != resolver.ModeRegular && b.mode != resolver.ModeExecutable && b.mode != resolver.ModeSymlink) {
				continue
			}
			info, err := repo.ReadObjectInfo(ctx, b.oid.Revision())
			if err != nil {
				return nil, fmt.Errorf("reading size of %q: %w", f.conflict.path, err)
			}
			n += info.Size
		}
		if len(cur) > 0 && size+n > budget {
			batches = append(batches, cur)
			cur, size = nil, 0
		}
		cur = append(cur, f)
		size += n
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches, nil
}
