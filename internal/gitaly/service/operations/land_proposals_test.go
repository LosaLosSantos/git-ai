package operations

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/helper/text"
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
	out, err := gittest.NewCommand(t, fx.cfg, "-C", fx.repoPath, "rev-parse", "--verify", "--quiet", landProposalRef(git.DefaultBranch, fx.commit).String()).Output()
	if err != nil {
		return ""
	}
	return text.ChompBytes(out)
}

func TestUserLand_proposals(t *testing.T) {
	t.Parallel()

	t.Run("a model's answer is proposed, never landed", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, landProposalRef(git.DefaultBranch, fx.commit).String(), pkg.GetProposalRef())

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
			require.Contains(t, f.GetReason(), "never lands a model's resolution", p)
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
		branchDir := landProposalBranchDir(git.DefaultBranch)
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
		require.Contains(t, refs, landProposalRef(git.DefaultBranch, fx.commit).String(), "the newest is kept")
		require.NotContains(t, refs, name(100), "past proposal_ttl")
		require.NotContains(t, refs, name(20), "the oldest beyond 20")
		require.NotContains(t, refs, name(19), "the oldest beyond 20")
		require.Contains(t, refs, name(18))
	})

	t.Run("a proposal reference that cannot be written fails the land with Internal", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		// A reference where the branch's directory of proposals goes: the update cannot create it.
		gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "update-ref", strings.TrimSuffix(landProposalBranchDir(git.DefaultBranch), "/"), fx.tip.String())

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		require.Equal(t, codes.Internal, status.Code(err), "err = %v", err)
		require.Contains(t, err.Error(), "keeping the resolver's answer as proposals")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
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

// TestLandUpdateProposalRefs_onlyProposalReferences: the one writer of the proposal references
// refuses any other reference before git runs, whatever the caller passes.
func TestLandUpdateProposalRefs_onlyProposalReferences(t *testing.T) {
	t.Parallel()

	fx := newResolveFixture(t, config.GraftResolver{})
	s := fx.server(t, config.GraftResolver{})
	valid := landProposalRef(git.DefaultBranch, fx.commit)
	require.True(t, landProposalRefNameValid(valid))
	for _, name := range []git.ReferenceName{
		"refs/heads/main",
		"refs/graft/green/main",
		"refs/graft/proposals/x/" + git.ReferenceName(fx.commit.String()),
		git.ReferenceName(strings.TrimSuffix(landProposalBranchDir(git.DefaultBranch), "/")),
		git.ReferenceName(landProposalBranchDir(git.DefaultBranch) + "not-an-oid"),
		git.ReferenceName(landProposalBranchDir(git.DefaultBranch) + fx.commit.String() + "/x"),
	} {
		err := s.landUpdateProposalRefs(fx.ctx, fx.repo, []landProposalRefOp{{name: valid, newOID: fx.tip, oldOID: gittest.DefaultObjectHash.ZeroOID}, {name: name, newOID: fx.tip, oldOID: gittest.DefaultObjectHash.ZeroOID}})
		require.ErrorContains(t, err, "not a proposal reference", "%s", name)
	}
	require.Empty(t, proposalRefTarget(t, fx), "nothing was written")
}
