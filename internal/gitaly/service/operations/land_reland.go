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

// landWithResolution lands commitID onto tip with the caller's resolution of the conflicts
// (UserLandRequest.resolution, PLAN10 3.3 and 12). It is accepted only when it was made against
// this tip and this commit and resolves exactly the paths that conflict, each once. Every file
// gets the acceptance rules of a resolver answer; when a resolver is configured, the files that
// may be sent to it also get its checks (the check operation: the syntax check and the configured
// checks), and a failing or failed check fails the land. Nothing lands partially: an unaccepted
// resolution fails with a fresh conflict package (*landUnresolvedError).
//
// The merge commit carries Graft-Resolved-By: agent/<actor> and, when the resolver checked the
// resolution, Graft-Checked-By: <resolver>.
func (s *Server) landWithResolution(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	tip, commitID git.ObjectID,
	committer git.Signature,
) (landResolution, error) {
	r := in.GetResolution()
	if r.GetCommit() != commitID.String() {
		return landResolution{}, structerr.NewInvalidArgument("resolution is for commit %q, not commit_id %q", r.GetCommit(), commitID)
	}

	l, err := landListConflicts(ctx, repo, tip, commitID, s.resolveCfg.MaxFileBytesOrDefault())
	if errors.Is(err, errLandNoConflicts) {
		return landResolution{}, structerr.NewFailedPrecondition("the merge of tip %s and commit %s has no conflicts, so there is nothing to resolve: land without a resolution", tip, commitID)
	}
	if err != nil {
		return landResolution{}, err
	}
	if len(l.files) == 0 {
		return landResolution{}, l.notAttempted(landNoConflictingFiles, nil)
	}
	if r.GetTip() != tip.String() {
		return landResolution{}, l.notAttempted(fmt.Sprintf("the branch tip moved from %s to %s since the resolution was made: resolve the conflicts of this package", r.GetTip(), tip), nil)
	}
	if l.bases > 1 {
		return landResolution{}, l.notAttempted(fmt.Sprintf("the change has %d merge bases (a criss-cross history), which are not resolved", l.bases), nil)
	}

	// The merge commit names the actor and every resolved path in its trailers: a value that cannot
	// be one trailer line is refused before anything else, and resolving the path again cannot help.
	actor := identity.Actor(ctx, "")
	if !landTrailerValueValid(landAgentResolver(actor)) {
		return landResolution{}, structerr.NewFailedPrecondition("the actor %q cannot be recorded in the %s trailer: it must be one line of printable UTF-8", actor, landTrailerResolvedBy)
	}
	for _, f := range l.files {
		if !landTrailerValueValid(f.conflict.path) {
			return landResolution{}, structerr.NewFailedPrecondition("the conflicting path %q cannot be recorded in the %s trailer: it must be one line of printable UTF-8, so its conflict cannot be resolved through a land", f.conflict.path, landTrailerResolvedPath)
		}
	}

	resolved, reason := landResolutionFiles(r, l.files)
	if reason != "" {
		return landResolution{}, l.notAttempted(reason, nil)
	}

	maxBytes := s.resolveCfg.MaxFileBytesOrDefault()
	rejected := false
	for _, f := range l.files {
		rf := resolved[f.conflict.path]
		err := resolver.ValidateFile(rf, maxBytes)
		if err == nil {
			err = landModeOfSides(f.conflict, rf)
		}
		if err != nil {
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_REJECTED, err.Error()
			rejected = true
		}
	}

	checkedBy, syntaxUnchecked := "", 0
	if !rejected && s.conflictResolver != nil {
		if checkedBy, syntaxUnchecked, err = s.landCheckResolution(ctx, in, repo, l, actor, resolved); err != nil {
			return landResolution{}, err
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

	resp := &resolver.Response{Version: resolver.ProtocolVersion, Resolver: landAgentResolver(actor)}
	for _, f := range l.files {
		resp.Files = append(resp.Files, resolved[f.conflict.path])
	}
	res, err := s.landWriteResolution(ctx, in, repo, tip, commitID, l.mergedTree, committer, actor, resp, checkedBy)
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

// landCheckResolution runs the resolver's checks on the files of the resolution that may be sent
// to the resolver (landClassify), and sets the outcome of each checked file that fails them to
// REJECTED. It returns the checker's name for the Graft-Checked-By trailer, or "" when no file
// could be sent, and the number of files accepted without a syntax verdict. The check fails
// closed: a check that cannot run is a *landUnresolvedError with a fresh package.
func (s *Server) landCheckResolution(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	l landListing,
	actor string,
	resolved map[string]resolver.ResponseFile,
) (string, int, error) {
	if err := s.landClassify(ctx, repo, l.files, l.base, l.tip, l.commitID); err != nil {
		return "", 0, err
	}

	req := resolver.Request{
		Repository: resolver.Repository{
			Storage:      in.GetRepository().GetStorageName(),
			RelativePath: in.GetRepository().GetRelativePath(),
		},
		Branch:   string(in.GetBranch()),
		Tip:      l.tip.String(),
		Commit:   l.commitID.String(),
		Messages: resolver.Messages{Tip: l.tipMessage, Commit: l.commitMessage},
		Actor:    actor,
	}
	if l.hasBase {
		req.MergeBase = l.base.String()
	}
	var checked []*landFileState
	var unchecked []string
	for _, f := range l.files {
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
	if len(checked) == 0 {
		s.logger.WithField("unchecked", unchecked).InfoContext(ctx, "graft: no file of the agent's resolution may be sent to the resolver's checks")
		return "", 0, nil
	}

	// Fail closed: a check that cannot run (an error, an answer that is no valid check response, a
	// resolver that does not know the check operation) fails the land with a fresh package.
	resp, err := s.conflictResolver.Check(ctx, req)
	if err != nil {
		return "", 0, l.notAttempted(fmt.Sprintf("the resolution could not be checked: %v", err), err)
	}
	if !landResolverNameValid(resp.Resolver) {
		return "", 0, l.notAttempted(fmt.Sprintf("the resolution could not be checked: the check names an invalid resolver %q", resp.Resolver), nil)
	}
	syntaxUnchecked := 0
	for i, v := range resp.Files {
		f := checked[i]
		if !v.OK {
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_REJECTED, v.Reason
			continue
		}
		if v.Syntax == resolver.SyntaxUnchecked {
			f.syntaxUnchecked = true
			syntaxUnchecked++
		}
	}
	s.logger.WithFields(log.Fields{
		"checked_by":       resp.Resolver,
		"checked":          len(checked),
		"syntax_unchecked": syntaxUnchecked,
		"not_checked":      unchecked,
	}).InfoContext(ctx, "graft: agent's resolution checked")
	return resp.Resolver, syntaxUnchecked, nil
}
