package operations

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/proposals"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The tests of a model's resolution that lands through the operator's verification (PLAN10 15.9): it
// lands only if verify.command passed, out of the lane, on the exact tree that lands.

// countingVerify writes a verification command into a new temporary directory and returns the
// directory and the argv of the command. Every run appends a line to dir/vlines before body runs,
// and N is the number of runs so far. body may block, move the branch or fail.
func countingVerify(t *testing.T, body string) (string, []string) {
	t.Helper()

	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
DIR='%s'
echo x >> "$DIR/vlines"
N=$(wc -l < "$DIR/vlines" | tr -d ' ')
%s
`, dir, body)
	path := filepath.Join(dir, "verify.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return dir, []string{path}
}

// verifyRuns returns the number of times the verification of dir ran.
func verifyRuns(t *testing.T, dir string) int {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "vlines"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	require.NoError(t, err)
	return len(raw) / 2
}

// moveBranchBody makes the n-th run of a countingVerify command move the branch to the commit in
// dir/move-n, when the test wrote one (writeVerifyMoves), before it goes on.
var moveBranchBody = fmt.Sprintf(`if [ -f "$DIR/move-$N" ]; then
  "$(cat "$DIR/git")" -C "$(cat "$DIR/repo")" update-ref refs/heads/%s "$(cat "$DIR/move-$N")"
fi`, git.DefaultBranch)

// writeVerifyMoves lets the n-th run of the verification of dir move the branch of fx to moves[n-1].
func writeVerifyMoves(t *testing.T, dir string, fx resolveFixture, moves ...git.ObjectID) {
	t.Helper()

	writeMoves(t, dir, fx, moves...)
}

// verifiedFixture is a land whose resolver answers with a model's resolution, and whose
// verify.command is the countingVerify command with the given body.
func verifiedFixture(t *testing.T, answer resolver.Response, verifyBody string, tune func(*config.GraftResolver)) (resolveFixture, string, string) {
	t.Helper()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, answer)
	vdir, vargv := countingVerify(t, verifyBody)
	rcfg := withVerify(enabledResolver(rargv), vargv, "")
	if tune != nil {
		tune(&rcfg)
	}
	return newResolveFixture(t, rcfg), rdir, vdir
}

// landCommit returns the commit with the given ID.
func landedCommit(t *testing.T, fx resolveFixture, resp *gitalypb.UserLandResponse) (*gitalypb.GitCommit, git.ObjectID) {
	t.Helper()

	id := git.ObjectID(resp.GetLandedCommitId())
	commit, err := fx.repo.ReadCommit(fx.ctx, id.Revision())
	require.NoError(t, err)
	return commit, id
}

func TestUserLand_verifiedModelResolution(t *testing.T) {
	t.Parallel()

	t.Run("a model's resolution that passes the verification lands, signed", func(t *testing.T) {
		t.Parallel()

		fx, rdir, vdir := verifiedFixture(t, modelAnswer(), "grep -qx merged-a a && grep -qx merged-b b && grep -qx d d", nil)

		resp, err := fx.client.UserLand(fx.ctx, fx.request("Land agent work"))
		require.NoError(t, err)
		commit, landed := landedCommit(t, fx, resp)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, landed)
		require.Equal(t, []string{fx.tip.String(), fx.commit.String()}, commit.GetParentIds())
		require.Equal(t, "graft-resolve/llm", resp.GetResolver())
		require.Equal(t, "merged-a\n", fx.blobAt(t, landed, "a"))
		require.Equal(t, "merged-b\n", fx.blobAt(t, landed, "b"))
		body := string(commit.GetBody())
		require.Contains(t, body, "Graft-Resolved-By: graft-resolve/llm\n")
		require.Contains(t, body, "Graft-Checked-By: verify:verify.sh\n")
		require.Equal(t, 1, resolverCalls(t, rdir))
		// Once: out of the lane. The lane lands the verified tree without verifying it again.
		require.Equal(t, 1, verifyRuns(t, vdir))
		require.Empty(t, proposalRefTarget(t, fx), "nothing was proposed")
	})

	t.Run("a model's file next to a rule's file lands when the tree passes", func(t *testing.T) {
		t.Parallel()

		answer := modelAnswer()
		answer.Resolver = "graft-resolve/trivial,llm"
		answer.Files[1].Proposal = false
		fx, _, vdir := verifiedFixture(t, answer, "exit 0", nil)

		resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		require.NoError(t, err)
		_, landed := landedCommit(t, fx, resp)
		require.Equal(t, "merged-a\n", fx.blobAt(t, landed, "a"))
		require.Equal(t, "merged-b\n", fx.blobAt(t, landed, "b"))
		require.Equal(t, 1, verifyRuns(t, vdir))
	})

	t.Run("a verification that fails gives a proposal and the output", func(t *testing.T) {
		t.Parallel()

		fx, rdir, vdir := verifiedFixture(t, modelAnswer(), `echo "2 tests failed: TestFoo"; exit 3`, nil)

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, proposals.Ref(git.DefaultBranch, fx.commit).String(), pkg.GetProposalRef())
		require.NotEmpty(t, proposalRefTarget(t, fx))
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "verification failed: exit status 3: 2 tests failed: TestFoo", p)
			require.NotNil(t, f.GetProposal(), p)
			require.Equal(t, "merged-"+p+"\n", string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "cat-file", "blob", f.GetProposal().GetOid())))
		}
		require.Equal(t, 1, resolverCalls(t, rdir))
		require.Equal(t, 1, verifyRuns(t, vdir), "once, out of the lane")
	})

	t.Run("a verification that times out is a failure with its reason", func(t *testing.T) {
		t.Parallel()

		rdir, rargv := resolverScript(t, "")
		writeAnswer(t, rdir, modelAnswer())
		vdir, vargv := countingVerify(t, "sleep 30")
		fx := newResolveFixture(t, withVerify(enabledResolver(rargv), vargv, "300ms"))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		f := packageFile(t, landConflictDetail(err), "a")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome())
		require.Contains(t, f.GetReason(), "the command did not finish within 300ms")
		require.Equal(t, 1, verifyRuns(t, vdir))
	})

	t.Run("without a verify.command a model's resolution is a proposal", func(t *testing.T) {
		t.Parallel()

		rdir, rargv := resolverScript(t, "")
		writeAnswer(t, rdir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(rargv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, landConflictDetail(err), p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "no verify.command is configured", p)
			require.NotNil(t, f.GetProposal(), p)
		}
	})

	t.Run("nothing is verified when another file keeps the land from landing", func(t *testing.T) {
		t.Parallel()

		// b is written with a mode no side has: refused. Verifying the tree would cost the operator's
		// tests for a land that cannot land.
		answer := modelAnswer()
		answer.Files[1].Mode = "100755"
		fx, _, vdir := verifiedFixture(t, answer, "exit 0", nil)

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, packageFile(t, pkg, "a").GetOutcome())
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, packageFile(t, pkg, "b").GetOutcome())
		require.Equal(t, 0, verifyRuns(t, vdir))
	})

	t.Run("a tip that moved gets the new tree verified, then lands", func(t *testing.T) {
		t.Parallel()

		// While the first verification runs, a writer adds e to the branch: the tree to land is no
		// longer the verified one, so the land goes back out, rebuilds the tree with the same verdicts
		// (the resolver is not called again), verifies it, and lands that.
		fx, rdir, vdir := verifiedFixture(t, modelAnswer(), moveBranchBody, nil)
		writer := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip),
			gittest.WithTreeEntries(entries("a", "ours\n", "b", "ours\n", "c", "c\n", "e", "e\n")...))
		writeVerifyMoves(t, vdir, fx, writer)

		resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		require.NoError(t, err)
		commit, landed := landedCommit(t, fx, resp)
		require.Equal(t, []string{writer.String(), fx.commit.String()}, commit.GetParentIds())
		require.Equal(t, "e\n", fx.blobAt(t, landed, "e"))
		require.Equal(t, "merged-a\n", fx.blobAt(t, landed, "a"))
		require.Equal(t, 1, resolverCalls(t, rdir), "no resolver call: the files did not change")
		require.Equal(t, 2, verifyRuns(t, vdir), "the tree of the new tip is verified, out of the lane")
		require.Contains(t, string(commit.GetBody()), "Graft-Checked-By: verify:verify.sh\n")
	})

	t.Run("a tip that moved to the same tree keeps the verification", func(t *testing.T) {
		t.Parallel()

		// The tree is what is verified, not the tip: a writer that moves the branch to another commit
		// with the very tree the land started from leaves the verified tree the tree to land.
		fx, _, vdir := verifiedFixture(t, modelAnswer(), moveBranchBody, nil)
		same := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip), gittest.WithMessage("same tree, another commit"),
			gittest.WithTreeEntries(entries("a", "ours\n", "b", "ours\n", "c", "c\n")...))
		writeVerifyMoves(t, vdir, fx, same)

		resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		require.NoError(t, err)
		commit, _ := landedCommit(t, fx, resp)
		require.Equal(t, []string{same.String(), fx.commit.String()}, commit.GetParentIds())
		require.Equal(t, 1, verifyRuns(t, vdir), "the same tree: verified once")
	})

	t.Run("a land the lane hands back for the last round gives a proposal, not a failure", func(t *testing.T) {
		t.Parallel()

		// resolve_rounds is a cap: past it, a tree the branch moved away from is not verified in time
		// any more, whatever the clock says. The model's files are proposals, with the reason that the
		// branch moved again, and the resolver is not called again.
		fx, rdir, vdir := verifiedFixture(t, modelAnswer(), moveBranchBody, func(r *config.GraftResolver) {
			r.ResolveRounds = 1
		})
		writer := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip),
			gittest.WithTreeEntries(entries("a", "ours\n", "b", "ours\n", "c", "c\n", "e", "e\n")...))
		writeVerifyMoves(t, vdir, fx, writer)

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		require.NotContains(t, err.Error(), "files kept changing")
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, writer.String(), pkg.GetTip(), "the package is about the tip the branch moved to")
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "could not verify before the branch moved again", p)
			require.NotNil(t, f.GetProposal(), p)
		}
		require.Equal(t, 1, resolverCalls(t, rdir))
		require.Equal(t, 1, verifyRuns(t, vdir), "the give-up verifies nothing")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, writer)
	})

	t.Run("churn past the deadline gives a proposal, not a failure", func(t *testing.T) {
		t.Parallel()

		// The branch moves under every verification, faster than the land can verify. At the deadline
		// the model's files become proposals, with the reason, and the agent gets the package.
		fx, rdir, vdir := verifiedFixture(t, modelAnswer(), "sleep 0.4\n"+moveBranchBody, func(r *config.GraftResolver) {
			r.Timeout = "1s"
			r.Verify.Timeout = "2s"
			r.ResolveRounds = 1000
		})
		var moves []git.ObjectID
		parent := fx.tip
		for i := 0; i < 40; i++ {
			parent = gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(parent),
				gittest.WithTreeEntries(entries("a", "ours\n", "b", "ours\n", "c", "c\n", "e", fmt.Sprintf("e-%d\n", i))...))
			moves = append(moves, parent)
		}
		writeVerifyMoves(t, vdir, fx, moves...)

		// The bound is a check that the land does not hang, not a measure: the deadline of the land is
		// 3 s, and a loaded machine takes a multiple of that for the pass that follows it.
		start := time.Now()
		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		took := time.Since(start)
		t.Logf("the land ended after %s (deadline 3s)", took)
		require.Less(t, took, 3*time.Minute)
		requireMergeConflict(t, err)
		require.NotContains(t, err.Error(), "files kept changing")
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.NotEmpty(t, pkg.GetProposalRef())
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "could not verify before", p)
			require.NotNil(t, f.GetProposal(), p)
			require.Equal(t, "merged-"+p+"\n", string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "cat-file", "blob", f.GetProposal().GetOid())))
		}
		require.Equal(t, 1, resolverCalls(t, rdir), "the verdicts are reused: the resolver is called once")
		// One round or several, by the speed of the machine: a loaded one spends the whole deadline on
		// the first. What does not depend on speed is that the verification ran, that the writers did
		// not run out (the deadline ended the land), and that the resolver was called once. The round
		// that goes back out after a tip moved is the test above, deterministic.
		require.GreaterOrEqual(t, verifyRuns(t, vdir), 1)
		require.Less(t, verifyRuns(t, vdir), len(moves), "the deadline ended it, not the writers")
		// The branch was only moved by the writers: the model's resolution did not land.
		tip := gittest.ResolveRevision(t, fx.cfg, fx.repoPath, git.DefaultBranch)
		parents := strings.Fields(text.ChompBytes(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "rev-list", "--parents", "-n1", tip.String())))
		require.NotContains(t, parents, fx.commit.String())
	})
}

func TestUserLand_verifiedModelResolution_laneIsFree(t *testing.T) {
	t.Parallel()

	// The first verification blocks until the test releases it. While it does, the branch's lane is
	// free: a plain land of another change passes through it. When the verification is released, the
	// tree of the model's land is not the tree that was verified (the plain land moved the branch),
	// and the land verifies the new tree and lands on top of the plain land.
	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, modelAnswer())
	vdir, vargv := countingVerify(t, `if [ "$N" = 1 ]; then
  i=0
  while [ ! -f "$DIR/release" ] && [ "$i" -lt 1200 ]; do sleep 0.1; i=$((i+1)); done
fi`)
	graft := batchingGraft()
	graft.Land.Resolver = withVerify(enabledResolver(rargv), vargv, "")
	fx := newResolveFixtureIn(t, graft,
		entries("a", "base\n", "b", "base\n", "c", "c\n"),
		entries("a", "ours\n", "b", "ours\n", "c", "c\n"),
		entries("a", "theirs\n", "b", "theirs\n", "c", "c\n", "d", "d\n"),
	)
	plain := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base),
		gittest.WithTreeEntries(entries("a", "base\n", "b", "base\n", "c", "c\n", "z", "z\n")...))

	model := landAsync(fx, fx.commit, "Land the model's")
	require.Eventually(t, func() bool { return verifyRuns(t, vdir) == 1 }, 30*time.Second, 10*time.Millisecond, "the verification never started")

	// The verification holds until the test releases it (or two minutes pass): a plain land that ends
	// before that went through a free lane, whatever the speed of the machine.
	plainReq := fx.request("Land the plain one")
	plainReq.CommitId = plain.String()
	plainReq.ResolveConflicts = false
	plainDone := make(chan landResult, 1)
	go func() {
		resp, err := fx.client.UserLand(fx.ctx, plainReq)
		plainDone <- landResult{resp: resp, err: err}
	}()
	var plainResult landResult
	select {
	case plainResult = <-plainDone:
	case <-time.After(60 * time.Second):
		t.Cleanup(func() { _ = os.WriteFile(filepath.Join(vdir, "release"), nil, 0o600) })
		require.FailNow(t, "the plain land did not end while the verification ran: the lane is held")
	}
	require.NoError(t, plainResult.err, "the lane is free while the verification runs")
	plainResp := plainResult.resp
	_, notReleased := os.Stat(filepath.Join(vdir, "release"))
	require.ErrorIs(t, notReleased, fs.ErrNotExist, "the plain land ended before the release")
	select {
	case r := <-model:
		require.FailNow(t, "the model's land ended before the verification was released", "%v %v", r.resp, r.err)
	default:
	}
	require.Equal(t, 1, verifyRuns(t, vdir))

	require.NoError(t, os.WriteFile(filepath.Join(vdir, "release"), nil, 0o600))
	resp := requireLanded(t, model)
	commit, landed := landedCommit(t, fx, resp)
	require.Equal(t, []string{plainResp.GetLandedCommitId(), fx.commit.String()}, commit.GetParentIds())
	require.Equal(t, "z\n", fx.blobAt(t, landed, "z"))
	require.Equal(t, "merged-a\n", fx.blobAt(t, landed, "a"))
	require.Equal(t, 2, verifyRuns(t, vdir))
	require.Equal(t, 1, resolverCalls(t, rdir))
}

// TestUserLand_verifiedModelResolution_poolBoundsVerifications: with verify_concurrency 1, two lands
// at once never run the operator's command at once.
func TestUserLand_verifiedModelResolution_poolBoundsVerifications(t *testing.T) {
	t.Parallel()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, modelAnswer())
	vdir, vargv := countingVerify(t, `if ! mkdir "$DIR/lock" 2>/dev/null; then
  echo x >> "$DIR/overlap"
  sleep 0.3
  exit 0
fi
sleep 0.3
rmdir "$DIR/lock"`)
	rcfg := withVerify(enabledResolver(rargv), vargv, "")
	rcfg.VerifyConcurrency = 1
	graft := batchingGraft()
	graft.Land.Resolver = rcfg
	fx := newResolveFixtureIn(t, graft,
		entries("a", "base\n", "b", "base\n"),
		entries("a", "ours\n", "b", "ours\n"),
		entries("a", "theirs-0\n", "b", "theirs-0\n", "d0", "0\n"),
	)
	second := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base),
		gittest.WithTreeEntries(entries("a", "theirs-1\n", "b", "theirs-1\n", "d1", "1\n")...))

	first := landAsync(fx, fx.commit, "Land 0")
	other := landAsync(fx, second, "Land 1")
	requireLanded(t, first)
	requireLanded(t, other)

	require.GreaterOrEqual(t, verifyRuns(t, vdir), 2)
	_, err := os.Stat(filepath.Join(vdir, "overlap"))
	require.ErrorIs(t, err, fs.ErrNotExist, "two verifications ran at once")
}

// TestLandApplyVerdicts_neverVerifiesAModelsResolution: the lane writes a model's resolution only
// for a tree that verify.command passed on out of it, and never runs the command itself.
func TestLandApplyVerdicts_neverVerifiesAModelsResolution(t *testing.T) {
	t.Parallel()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, modelAnswer())
	vdir, vargv := countingVerify(t, "exit 0")
	rcfg := withVerify(enabledResolver(rargv), vargv, "")
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)
	in := fx.request("Land")

	// Out of the lane: the answer is resolved and its tree verified, once.
	pre := newLandPreResolution()
	_, _, err := s.landResolveAt(fx.ctx, in, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
	require.NoError(t, err)
	require.Equal(t, 1, verifyRuns(t, vdir))
	require.Len(t, pre.verified, 1)

	// In the lane, with the verified tree: it lands, and does not verify.
	res, err := s.landApplyVerdicts(fx.ctx, in, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
	require.NoError(t, err)
	require.Equal(t, "merged-a\n", fx.blobAt(t, res.commit, "a"))
	require.Equal(t, 1, verifyRuns(t, vdir), "the lane did not run the command")
	message := string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "log", "-1", "--format=%B", res.commit.String()))
	require.Contains(t, message, "Graft-Checked-By: verify:verify.sh\n")

	// In the lane, with no tree verified (the tip moved to a tree nobody verified): it writes nothing
	// and hands the land back to be verified; it does not verify either.
	unverified := &landPreResolution{verdicts: pre.verdicts, verified: map[git.ObjectID]bool{}}
	_, err = s.landApplyVerdicts(fx.ctx, in, fx.repo, fx.tip, fx.commit, fx.committer(), unverified)
	var need *landNeedsResolution
	require.ErrorAs(t, err, &need)
	require.NotEmpty(t, need.unverified, "the tree to verify")
	require.Equal(t, 1, verifyRuns(t, vdir), "the lane did not run the command")

	// Another verified tree is not this one: tree OIDs are compared.
	other := &landPreResolution{verdicts: pre.verdicts, verified: map[git.ObjectID]bool{"0000000000000000000000000000000000000000": true}}
	_, err = s.landApplyVerdicts(fx.ctx, in, fx.repo, fx.tip, fx.commit, fx.committer(), other)
	require.ErrorAs(t, err, &need)
	require.Equal(t, 1, verifyRuns(t, vdir))
}

// TestUserLand_verifiedRulesKeepTheirVerificationInTheLane: a resolution made by rules alone keeps its
// verify.command in the lane, as before (15.9): the next step moves it.
func TestUserLand_verifiedRulesKeepTheirVerificationInTheLane(t *testing.T) {
	t.Parallel()

	fx, _, vdir := verifiedFixture(t, validAnswer(), "grep -qx merged-a a", nil)

	resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	require.NoError(t, err)
	commit, _ := landedCommit(t, fx, resp)
	require.Equal(t, 1, verifyRuns(t, vdir))
	require.Contains(t, string(commit.GetBody()), "Graft-Resolved-By: graft-resolve/fake\n")
	require.NotContains(t, string(commit.GetBody()), "Graft-Checked-By", "rules are unchanged")

	failing, _, failingDir := verifiedFixture(t, validAnswer(), "exit 1", nil)
	_, err = failing.client.UserLand(failing.ctx, failing.request("Land"))
	requireMergeConflict(t, err)
	for _, f := range landConflictDetail(err).GetFiles() {
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, f.GetOutcome())
	}
	require.Equal(t, 1, verifyRuns(t, failingDir))
}

// TestUserLand_verifiedModelResolution_policyAndCancellation: a land whose caller gives up while it
// verifies fails with the caller's cancellation, and not as proposals.
func TestUserLand_verifiedModelResolution_callerGivesUp(t *testing.T) {
	t.Parallel()

	fx, _, vdir := verifiedFixture(t, modelAnswer(), "sleep 30", func(r *config.GraftResolver) { r.Verify.Timeout = "1m" })
	ctx, cancel := context.WithCancel(fx.ctx)
	req := fx.request("Land")
	done := make(chan error, 1)
	go func() {
		_, err := fx.client.UserLand(ctx, req)
		done <- err
	}()
	require.Eventually(t, func() bool { return verifyRuns(t, vdir) == 1 }, 30*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		require.Equal(t, codes.Canceled, status.Code(err), "%v", err)
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the land did not end when its caller gave up")
	}
	requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
}
