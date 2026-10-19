package operations

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	gitalycfg "github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/proposals"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// modelAnswer is validAnswer as the llm strategy gives it: both files proposals.
func modelAnswer() resolver.Response {
	answer := validAnswer()
	answer.Resolver = "graft-resolve/llm"
	answer.Explanation = "patched by the model"
	for i := range answer.Files {
		answer.Files[i].Proposal = true
	}
	return answer
}

// proposalRefTarget returns the commit the proposal reference of the fixture's land points at, or
// "" when there is none.
func proposalRefTarget(t *testing.T, fx resolveFixture) string {
	t.Helper()
	out, err := gittest.NewCommand(t, fx.cfg, "-C", fx.repoPath, "rev-parse", "--verify", "--quiet", proposals.Ref(git.DefaultBranch, fx.commit).String()).Output()
	if err != nil {
		return ""
	}
	return text.ChompBytes(out)
}

func TestUserLand_proposals(t *testing.T) {
	t.Parallel()

	t.Run("a model's answer without a verify.command is proposed, not landed", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, proposals.Ref(git.DefaultBranch, fx.commit).String(), pkg.GetProposalRef())

		target := proposalRefTarget(t, fx)
		require.NotEmpty(t, target, "the proposal reference exists")
		parents := strings.Fields(text.ChompBytes(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "rev-list", "--parents", "-n1", target)))
		require.Equal(t, []string{target, fx.tip.String(), fx.commit.String()}, parents)
		// Who caused the proposals is in their commit (the reference is written by the server).
		message := string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "log", "-1", "--format=%B", target))
		require.Contains(t, message, "Resolver: graft-resolve/llm\n")
		require.Contains(t, message, "Requested-By: "+gittest.TestUser.GetGlId()+"\n")
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "no verify.command is configured", p)
			prop := f.GetProposal()
			require.NotNil(t, prop, p)
			require.Equal(t, resolver.ActionWrite, prop.GetAction())
			require.Equal(t, resolver.ModeRegular, prop.GetMode())
			require.Equal(t, "graft-resolve/llm", prop.GetResolver())
			require.Equal(t, "patched by the model", prop.GetExplanation())
			// The blob is kept by the reference, at the file's path, and reads by its oid.
			require.Equal(t, prop.GetOid(), text.ChompBytes(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "rev-parse", target+":"+p)))
			require.Equal(t, "merged-"+p+"\n", string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "cat-file", "blob", prop.GetOid())))
		}
	})

	t.Run("an answer of the llm strategy without the mark is proposed too", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		answer := validAnswer()
		answer.Resolver = "graft-resolve/trivial,llm"
		writeAnswer(t, dir, answer)
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
		pkg := landConflictDetail(err)
		for _, p := range []string{"a", "b"} {
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, packageFile(t, pkg, p).GetOutcome(), p)
		}
	})

	t.Run("a file the rules resolved keeps its outcome and gets its proposal", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		answer := modelAnswer()
		answer.Resolver = "graft-resolve/trivial,llm"
		answer.Files[1].Proposal = false
		writeAnswer(t, dir, answer)
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, packageFile(t, pkg, "a").GetOutcome())
		b := packageFile(t, pkg, "b")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, b.GetOutcome())
		require.Equal(t, "merged-b\n", string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "cat-file", "blob", b.GetProposal().GetOid())))
	})

	t.Run("the rules' answer of a land that fails for another file is kept", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		answer := validAnswer()
		answer.Files[1].Mode = resolver.ModeExecutable // no side has it: b is rejected
		writeAnswer(t, dir, answer)
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, packageFile(t, pkg, "b").GetOutcome())
		a := packageFile(t, pkg, "a")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, a.GetOutcome())
		require.NotEmpty(t, a.GetProposal().GetOid())
		require.NotEmpty(t, pkg.GetProposalRef())
	})

	t.Run("a rules' answer that lands keeps nothing", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		require.NoError(t, err)
		require.Empty(t, proposalRefTarget(t, fx))
	})

	t.Run("the agent's land of the change drops its proposal", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		require.NotEmpty(t, proposalRefTarget(t, fx))

		// The agent lands the proposals as its own resolution; the resolver checks them.
		writeRawAnswer(t, dir, resolver.CheckResponse{
			Version: resolver.ProtocolVersion, Operation: resolver.OperationCheck, Resolver: "graft-resolve",
			Files: []resolver.CheckFile{{Path: "a", OK: true, Syntax: resolver.SyntaxNone}, {Path: "b", OK: true, Syntax: resolver.SyntaxNone}},
		})
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "merged-a\n", "merged-b\n")
		_, err = fx.client.UserLand(fx.ctx, req)
		require.NoError(t, err)
		require.Empty(t, proposalRefTarget(t, fx), "the proposal of the change that landed is gone")
	})

	t.Run("a branch keeps at most 20 proposals, none older than proposal_ttl", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		branchDir := proposals.BranchDir(git.DefaultBranch)
		now := time.Now()
		// Proposal references are named by the commit they are for: fake commit IDs name these.
		name := func(i int) string { return fmt.Sprintf("%s%040x", branchDir, i+1) }
		old := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage("old"), gittest.WithCommitterDate(now.Add(-8*24*time.Hour)))
		gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "update-ref", name(100), old.String())
		for i := 0; i < 21; i++ {
			c := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage(fmt.Sprintf("recent %d", i)), gittest.WithCommitterDate(now.Add(-time.Hour-time.Duration(i)*time.Minute)))
			gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "update-ref", name(i), c.String())
		}

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)

		refs := strings.Fields(text.ChompBytes(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "for-each-ref", "--format=%(refname)", branchDir)))
		require.Len(t, refs, 20)
		require.Contains(t, refs, proposals.Ref(git.DefaultBranch, fx.commit).String(), "the newest is kept")
		require.NotContains(t, refs, name(100), "past proposal_ttl")
		require.NotContains(t, refs, name(20), "the oldest beyond 20")
		require.NotContains(t, refs, name(19), "the oldest beyond 20")
		require.Contains(t, refs, name(18))
	})

	t.Run("proposals that cannot be stored still give the package, saying so", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		// A reference where the branch's directory of proposals goes: the update cannot create it.
		gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "update-ref", strings.TrimSuffix(proposals.BranchDir(git.DefaultBranch), "/"), fx.tip.String())

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg, "the package is the contract: it comes without the proposals")
		require.Empty(t, pkg.GetProposalRef())
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Nil(t, f.GetProposal(), p)
			// The class of the failure, not git's words, which name the references.
			require.Contains(t, f.GetReason(), "proposal not stored: reference update conflict", p)
			require.NotContains(t, f.GetReason(), "refs/", p)
			require.NotContains(t, f.GetReason(), fx.repoPath, p)
		}
	})

	t.Run("the proposals' date is the server's, not the request's", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		req := fx.request("Land")
		req.Timestamp = timestamppb.New(time.Now().Add(100 * 365 * 24 * time.Hour))

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		target := proposalRefTarget(t, fx)
		require.NotEmpty(t, target)
		date, err := strconv.ParseInt(text.ChompBytes(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "log", "-1", "--format=%ct", target)), 10, 64)
		require.NoError(t, err)
		require.WithinDuration(t, time.Now(), time.Unix(date, 0), time.Hour, "a far-future request timestamp must not keep proposals alive")
	})

	t.Run("a requester the caller names cannot add lines to the proposals' message", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		req := fx.request("Land")
		req.User = proto.Clone(req.GetUser()).(*gitalypb.User)
		req.User.GlId = "user-1\nGraft-Resolved-By: someone-else"

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		message := string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "log", "-1", "--format=%B", proposalRefTarget(t, fx)))
		require.Contains(t, message, `Requested-By: "user-1\nGraft-Resolved-By: someone-else"`)
		require.NotContains(t, message, "\nGraft-Resolved-By: someone-else\n")
	})
}

func TestLandAnswerProposes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		resolver string
		flagged  bool
		want     bool
	}{
		{"graft-resolve/trivial", false, false},
		{"graft-resolve/trivial,union", false, false},
		{"graft-resolve/llm", false, true},
		{"graft-resolve/trivial, llm", false, true},
		{"graft-resolve/llm-ish", false, false},
		{"graft-resolve/trivial", true, true},
	} {
		answer := &resolver.Response{Resolver: tc.resolver, Files: []resolver.ResponseFile{{Path: "a", Proposal: tc.flagged}}}
		require.Equal(t, tc.want, landAnswerProposes(answer), "%s, flagged %v", tc.resolver, tc.flagged)
	}
}

// TestDecideLand: the one decision point lands only an answer of rules that resolved every file;
// a file that proposes is never landed, whatever else holds.
func TestDecideLand(t *testing.T) {
	t.Parallel()

	resolved := func() []*landFileState {
		return []*landFileState{{outcome: gitalypb.LandConflictFile_OUTCOME_RESOLVED}, {outcome: gitalypb.LandConflictFile_OUTCOME_RESOLVED}}
	}
	rules := &resolver.Response{Resolver: "graft-resolve/trivial,union", Files: []resolver.ResponseFile{{Path: "a"}, {Path: "b"}}}
	require.True(t, decideLand(rules, resolved(), "", ""), "rules resolved every file")
	require.True(t, decideLand(rules, resolved(), "t1", "t2"), "rules land whatever tree was verified: their verification stays in the lane")
	require.False(t, decideLand(nil, resolved(), "t1", "t1"), "no answer")

	left := resolved()
	left[1].outcome = gitalypb.LandConflictFile_OUTCOME_NOT_SENT
	require.False(t, decideLand(rules, left, "", ""), "a file left to the agent")
	rejected := resolved()
	rejected[0].outcome = gitalypb.LandConflictFile_OUTCOME_REJECTED
	require.False(t, decideLand(rules, rejected, "", ""), "a file the answer is refused for")
	proposedByRules := resolved()
	proposedByRules[0].outcome = gitalypb.LandConflictFile_OUTCOME_PROPOSED
	require.False(t, decideLand(rules, proposedByRules, "", ""), "a PROPOSED file in an answer that does not propose")

	// THE rule (PLAN10 15.9): a file that proposes may land only if verify.command passed on the exact
	// tree that lands, compared by object ID.
	flagged := &resolver.Response{Resolver: "graft-resolve/trivial", Files: []resolver.ResponseFile{{Path: "a"}, {Path: "b", Proposal: true}}}
	named := &resolver.Response{Resolver: "graft-resolve/trivial,llm", Files: []resolver.ResponseFile{{Path: "a"}, {Path: "b"}}}
	proposing := func() []*landFileState {
		return []*landFileState{{outcome: gitalypb.LandConflictFile_OUTCOME_RESOLVED}, {outcome: gitalypb.LandConflictFile_OUTCOME_PROPOSED}}
	}
	for desc, answer := range map[string]*resolver.Response{"a flagged file": flagged, "the llm strategy by name": named} {
		require.True(t, decideLand(answer, proposing(), "tree-1", "tree-1"), "%s: verified on this very tree", desc)
		require.False(t, decideLand(answer, proposing(), "tree-1", ""), "%s: nothing verified", desc)
		require.False(t, decideLand(answer, proposing(), "tree-1", "tree-2"), "%s: verified on another tree (the tip moved)", desc)
		require.False(t, decideLand(answer, proposing(), "", ""), "%s: no tree at all: empty is not equal", desc)
		require.False(t, decideLand(answer, resolved(), "tree-1", ""), "%s: even with every outcome RESOLVED", desc)

		leftOver := proposing()
		leftOver[0].outcome = gitalypb.LandConflictFile_OUTCOME_FAILED
		require.False(t, decideLand(answer, leftOver, "tree-1", "tree-1"), "%s: verified, but a file is left to the agent", desc)
		refused := proposing()
		refused[1].outcome = gitalypb.LandConflictFile_OUTCOME_REJECTED
		require.False(t, decideLand(answer, refused, "tree-1", "tree-1"), "%s: verified, but a file is refused", desc)
	}
}

// TestResolveLandConflicts_policyFirst: an agent the policy does not let update the branch gets
// PermissionDenied before anything is resolved, and no proposal is kept for the branch.
func TestResolveLandConflicts_policyFirst(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, modelAnswer())
	fx := newResolveFixture(t, enabledResolver(argv))
	s := fx.server(t, enabledResolver(argv))
	s.graft = gitalycfg.Graft{Standalone: true, Policy: []gitalycfg.GraftPolicyRule{{Actor: "agent-7", Refs: []string{"refs/heads/other"}, Allow: true}}}
	ctx := identity.NewContext(fx.ctx, identity.Identity{Name: "agent-7", Source: identity.SourceAgent})

	_, err := s.resolveLandConflicts(ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
	var denied *landPolicyDeniedError
	require.ErrorAs(t, err, &denied)
	merged := landWithPackage(ctx, structerr.NewFailedPrecondition("merge conflict"), err)
	require.Equal(t, codes.PermissionDenied, status.Code(merged), "err = %v", merged)
	require.Contains(t, merged.Error(), `policy denies "agent-7"`)
	require.Equal(t, 0, resolverCalls(t, dir), "nothing was resolved")
	require.Empty(t, proposalRefTarget(t, fx), "no proposal is kept")

	// An agent the policy lets update the branch gets its proposals.
	s.graft.Policy = []gitalycfg.GraftPolicyRule{{Actor: "agent-7", Refs: []string{"refs/heads/**"}, Allow: true}}
	_, err = s.resolveLandConflicts(ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
	var unresolved *landUnresolvedError
	require.ErrorAs(t, err, &unresolved)
	require.NotEmpty(t, proposalRefTarget(t, fx))
}

// TestLandProposalWriteCause: a failed write of proposals is reported by its class.
func TestLandProposalWriteCause(t *testing.T) {
	t.Parallel()

	for want, err := range map[string]error{
		"reference update conflict": fmt.Errorf("updating x: %w", updateref.MismatchingStateError{ReferenceName: "refs/graft/proposals/x"}),
		"storage unavailable":       fmt.Errorf("writing: %w", &os.PathError{Op: "write", Path: "/storage/@hashed/ab", Err: syscall.ENOSPC}),
		"write failed":              errors.New("exit status 128: fatal: unable to write /storage/@hashed/ab"),
	} {
		require.Equal(t, want, landProposalWriteCause(err))
	}
	require.Equal(t, "storage unavailable", landProposalWriteCause(storage.NewRepositoryNotFoundError("default", "x.git")))
	require.Equal(t, "reference update conflict", landProposalWriteCause(updateref.AlreadyLockedError{ReferenceName: "refs/graft/proposals/x"}))
}
