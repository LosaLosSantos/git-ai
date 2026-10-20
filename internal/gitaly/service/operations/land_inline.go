package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

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
//     long the last one took. The request announces no capability of the server but the origin, so that
//     a graft-resolve that gates the models on the capabilities (any since the capabilities were added)
//     refuses to call one even if it were to ignore RulesOnly; v0.2.0 has no gating and may call one;
//   - the lane never waits: the call takes a slot of a pool of its own (a model that holds all the
//     resolve slots does not stop the rules), and if there is none the member goes out of the lane;
//   - the time the lane may lose to calls that fail is bounded once per batch (landInlineBudget): a call
//     is given inline_timeout (default 1s) or what is left of the budget, and a call that fails, in time or
//     not, is charged to it; once it is spent the other members of the batch go out of the lane without a
//     call. A call that succeeds costs the batch nothing but its own time;
//   - a member that the rules cannot resolve (the resolver says so) goes out of the lane without a ticket:
//     without a verification what a model resolves never lands, so a ticket would protect nothing;
//   - a resolver that does not echo RulesOnly is an older graft-resolve, which may have called a model:
//     the answer is dropped, and the lane never resolves inline again for the life of the process;
//   - with a verification (verify.enabled) the lane never resolves: every tree is verified out of the lane;
//   - the answer is validated like any other (the client's acceptance rules, the modes, the origin), and
//     decideLand is the same: an answer that is not a rule's is never taken here.

// landInlineBudget is the time a batch attempt may lose to inline calls that fail (land_inline.go). The
// lane applies the members of an attempt one after the other, so it needs no lock.
type landInlineBudget struct {
	lost time.Duration
}

type landInlineBudgetKey struct{}

// withLandInlineBudget returns ctx carrying b, for the members of one batch attempt to share.
func withLandInlineBudget(ctx context.Context, b *landInlineBudget) context.Context {
	return context.WithValue(ctx, landInlineBudgetKey{}, b)
}

func landInlineBudgetFrom(ctx context.Context) *landInlineBudget {
	b, _ := ctx.Value(landInlineBudgetKey{}).(*landInlineBudget)
	return b
}

// landInlineResult is what became of the attempt to resolve a member inline.
type landInlineResult int

const (
	// landInlineNotTried: the lane does not resolve this member inline.
	landInlineNotTried landInlineResult = iota
	// landInlineResolved: every missing file has a verdict now.
	landInlineResolved
	// landInlineRulesCannot: the rules cannot resolve a file of the member (or it may not be sent): out of
	// the lane, for a model or the package.
	landInlineRulesCannot
	// landInlineSlow: the call did not end in time, found no slot, or the batch had spent its budget, or the
	// resolver is an older one: out of the lane, to be resolved there.
	landInlineSlow
)

// landInlineEnabled reports whether the lane resolves the rules inline: it is configured (inline_timeout is
// not zero), there is no verification, and the resolver has not shown itself to be an older one.
func (s *Server) landInlineEnabled() bool {
	timeout, err := s.resolveCfg.InlineTimeoutOrDefault()
	return err == nil && timeout > 0 && !s.resolveCfg.Verify.Enabled && !s.landInlineOff.Load() && s.conflictResolver != nil
}

// landResolveInline resolves, in the lane, the conflicting files of the member that have no verdict, as
// the rules would, and keeps the verdicts. It changes nothing unless it returns landInlineResolved.
func (s *Server) landResolveInline(ctx context.Context, in *gitalypb.UserLandRequest, tip, commitID git.ObjectID, l landListing, pre *landPreResolution) landInlineResult {
	if !s.landInlineEnabled() || pre.unverifiable != "" {
		return landInlineNotTried
	}
	timeout, _ := s.resolveCfg.InlineTimeoutOrDefault()
	// An agent that may not update the branch gets nothing resolved, as out of the lane (the policy first):
	// the land goes out of the lane, which refuses it with the policy's message.
	reference := git.NewReferenceNameFromBranchName(string(in.GetBranch()))
	if err := hook.CheckAgentPolicy(ctx, s.graft, fmt.Sprintf("%s %s %s\n", tip, commitID, reference)); err != nil {
		return landInlineNotTried
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
			return landInlineRulesCannot
		}
		pending = append(pending, f)
		sent = append(sent, landRequestFile(f))
	}
	if len(pending) == 0 {
		return landInlineNotTried
	}

	budget := landInlineBudgetFrom(ctx)
	if budget == nil {
		budget = &landInlineBudget{}
	}
	allowed := min(timeout, timeout-budget.lost)
	if allowed <= 0 {
		return landInlineSlow
	}
	slot, ok := s.conflictResolver.TryAcquireInlineSlot()
	if !ok {
		return landInlineSlow
	}
	defer slot.Release()

	req := resolver.Request{
		Version: resolver.ProtocolVersion,
		Repository: resolver.Repository{
			Storage:      in.GetRepository().GetStorageName(),
			RelativePath: in.GetRepository().GetRelativePath(),
		},
		Branch:   string(in.GetBranch()),
		Commit:   commitID.String(),
		Messages: resolver.Messages{Commit: l.commitMessage},
		Actor:    identity.Actor(ctx, ""),
		Files:    sent,
		// Only the origin: a graft-resolve that gates a model on the other capabilities refuses to call one.
		Capabilities: []string{resolver.CapabilityOrigin},
		RulesOnly:    true,
	}
	callCtx, cancel := context.WithTimeout(ctx, allowed)
	defer cancel()
	start := time.Now()
	resp, err := slot.Resolve(callCtx, req)
	if err != nil {
		budget.lost += time.Since(start)
		var refusal *resolver.UnresolvableError
		if errors.As(err, &refusal) || errors.Is(err, resolver.ErrUnresolvable) {
			// The rules cannot resolve a file of the member.
			return landInlineRulesCannot
		}
		s.logger.WithError(err).WithField("branch", string(in.GetBranch())).Debug("graft: inline resolution handed back")
		return landInlineSlow
	}
	fail := func(result landInlineResult) landInlineResult {
		budget.lost += time.Since(start)
		return result
	}
	if !resp.RulesOnly {
		if s.landInlineOff.CompareAndSwap(false, true) {
			s.logger.WithField("resolver", resp.Resolver).Warn("graft: the resolver did not honour a request for its rules only (an older graft-resolve): the lane resolves out of the lane from now on")
		}
		return fail(landInlineSlow)
	}
	if !landResolverNameValid(resp.Resolver) {
		return fail(landInlineSlow)
	}
	landNormalizeOrigins(resp)
	byPath := make(map[string]resolver.ResponseFile, len(resp.Files))
	for _, rf := range resp.Files {
		if rf.FromModel() || rf.Hold != "" {
			// Never a model's in the lane, whatever was asked.
			return fail(landInlineRulesCannot)
		}
		byPath[rf.Path] = rf
	}
	for _, f := range pending {
		rf, ok := byPath[f.conflict.path]
		if !ok || landModeOfSides(f.conflict, rf) != nil {
			return fail(landInlineRulesCannot)
		}
	}
	for _, f := range pending {
		pre.verdicts[landKeyOf(f.conflict)] = landVerdict{file: byPath[f.conflict.path], resolver: resp.Resolver}
	}
	return landInlineResolved
}

// landHasModelVerdict reports whether a file of the listing has a model's (or a held) verdict. Without a
// verification such a land does not land, so no other land has to give way to it.
func landHasModelVerdict(s *Server, l landListing, pre *landPreResolution) bool {
	if s.resolveCfg.Verify.Enabled {
		return false
	}
	for _, f := range l.files {
		if v, ok := pre.verdicts[landKeyOf(f.conflict)]; ok && (v.file.FromModel() || v.file.Hold != "") {
			return true
		}
	}
	return false
}
