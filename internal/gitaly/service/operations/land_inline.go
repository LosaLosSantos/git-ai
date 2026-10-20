package operations

import (
	"context"
	"fmt"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// Inline resolution of the rules in the lane (PLAN10 15.8).
//
// A land with conflicts is resolved out of the lane, because a model takes minutes and the lane is the
// branch's. But a rule (trivial, union, rerere, a command that is a rule) takes milliseconds, and for the
// lands on a hot file the out-of-lane resolution costs a round trip through the lane for every landing:
// the member that lands alone in a batch of its own pays the batch's fixed cost (the pack, the reference
// update, the replay) once more. As in v0.2.0, the lane therefore resolves a member's missing files
// itself, so that the members ride the same batches, but only what a rule can do and only within a
// bound of time:
//
//   - the request asks for the rules only (Request.RulesOnly): graft-resolve never calls a model for it,
//     and says so in its answer (Response.RulesOnly). The split follows what the resolution is, not how
//     long the last one took;
//   - the call is bounded by inline_timeout (default 1s); if it does not end in time, or does not resolve
//     every missing file as a rule, the member goes back out of the lane, as it did before, to be resolved
//     there, with the queue of land_tickets.go;
//   - a resolver that does not echo RulesOnly is an older graft-resolve, which may have called a model: the
//     answer is dropped, and the lane never resolves inline again for the life of the process;
//   - with a verification (verify.enabled) the lane never resolves: every tree is verified out of the lane;
//   - the answer is validated like any other (the client's acceptance rules, the modes, the origin), and
//     decideLand is the same: an answer that is not a rule's is never taken here.

// landInlineEnabled reports whether the lane resolves the rules inline: it is configured (inline_timeout is
// not zero), there is no verification, and the resolver has not shown itself to be an older one.
func (s *Server) landInlineEnabled() bool {
	timeout, err := s.resolveCfg.InlineTimeoutOrDefault()
	return err == nil && timeout > 0 && !s.resolveCfg.Verify.Enabled && !s.landInlineOff.Load() && s.conflictResolver != nil
}

// landResolveInline resolves, in the lane, the conflicting files of the member that have no verdict, as
// the rules would, and keeps the verdicts. It reports whether every one of them has a verdict now; it
// changes nothing when it reports false.
func (s *Server) landResolveInline(ctx context.Context, in *gitalypb.UserLandRequest, tip, commitID git.ObjectID, l landListing, pre *landPreResolution) bool {
	if !s.landInlineEnabled() || pre.unverifiable != "" {
		return false
	}
	timeout, _ := s.resolveCfg.InlineTimeoutOrDefault()
	// An agent that may not update the branch gets nothing resolved, as out of the lane (the policy first):
	// the land goes out of the lane, which refuses it with the policy's message.
	reference := git.NewReferenceNameFromBranchName(string(in.GetBranch()))
	if err := hook.CheckAgentPolicy(ctx, s.graft, fmt.Sprintf("%s %s %s\n", tip, commitID, reference)); err != nil {
		return false
	}

	var pending []*landFileState
	var sent []resolver.File
	for _, f := range l.files {
		if _, ok := pre.verdicts[landKeyOf(f.conflict)]; ok {
			continue
		}
		if !f.sent() {
			// A file that may not be sent to the resolver is not resolved: the land goes out of the lane
			// and says why.
			return false
		}
		pending = append(pending, f)
		sent = append(sent, landRequestFile(f))
	}
	if len(pending) == 0 {
		return false
	}

	req := resolver.Request{
		Version: resolver.ProtocolVersion,
		Repository: resolver.Repository{
			Storage:      in.GetRepository().GetStorageName(),
			RelativePath: in.GetRepository().GetRelativePath(),
		},
		Branch:       string(in.GetBranch()),
		Commit:       commitID.String(),
		Messages:     resolver.Messages{Commit: l.commitMessage},
		Actor:        identity.Actor(ctx, ""),
		Files:        sent,
		Capabilities: resolver.ServerCapabilities(),
		RulesOnly:    true,
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := s.landResolveSlotted(callCtx, req)
	if err != nil {
		// Out of time, or the rules cannot resolve a file: the member is resolved out of the lane.
		s.logger.WithError(err).WithField("branch", string(in.GetBranch())).Debug("graft: inline resolution handed back")
		return false
	}
	if !resp.RulesOnly {
		if s.landInlineOff.CompareAndSwap(false, true) {
			s.logger.WithField("resolver", resp.Resolver).Warn("graft: the resolver did not honour a request for its rules only (an older graft-resolve): the lane resolves out of the lane from now on")
		}
		return false
	}
	if !landResolverNameValid(resp.Resolver) {
		return false
	}
	landNormalizeOrigins(resp)
	byPath := make(map[string]resolver.ResponseFile, len(resp.Files))
	for _, rf := range resp.Files {
		if rf.FromModel() || rf.Hold != "" {
			// Never a model's in the lane, whatever was asked.
			return false
		}
		byPath[rf.Path] = rf
	}
	for _, f := range pending {
		rf, ok := byPath[f.conflict.path]
		if !ok || landModeOfSides(f.conflict, rf) != nil {
			return false
		}
	}
	for _, f := range pending {
		pre.verdicts[landKeyOf(f.conflict)] = landVerdict{file: byPath[f.conflict.path], resolver: resp.Resolver}
	}
	return true
}
