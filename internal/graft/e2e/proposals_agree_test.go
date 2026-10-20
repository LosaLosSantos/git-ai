package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// otherModel is a second model for the agree_with table: it answers like fakeModel, except that it
// changes the contested line, so that it disagrees with the first one.
const otherModel = `import json, re, sys
req = json.load(sys.stdin)
m = re.search(r"===== BEGIN CODE ([0-9a-f]+) =====\n(.*?)===== END CODE ([0-9a-f]+) =====", req["prompt"], re.S)
n, code = m.group(1), m.group(2)
code = code.replace("line of", "LINE of")
sys.stdout.write("===== BEGIN PATCHED CODE %s =====\n%s===== END PATCHED CODE %s =====\n" % (n, code, n))
`

// agreeResolverConfig writes a resolve.toml with the llm strategy of fakeModel and an agree_with model
// (second, the source of a model), and returns the graft configuration that uses the real
// graft-resolve with it and a verification that always passes, and the file in which each run of that
// verification leaves a line.
func agreeResolverConfig(t *testing.T, second string) (exampleConfig, string) {
	t.Helper()

	dir := t.TempDir()
	first := filepath.Join(dir, "model.py")
	require.NoError(t, os.WriteFile(first, []byte(fakeModel), 0o600))
	other := filepath.Join(dir, "other.py")
	require.NoError(t, os.WriteFile(other, []byte(second), 0o600))
	resolveConfig := filepath.Join(dir, "resolve.toml")
	require.NoError(t, os.WriteFile(resolveConfig, []byte(`[[strategy]]
name = "model"
type = "llm"
paths = ["shared-log.txt"]

[strategy.llm]
type = "command"
command = ["python3", "-I", "`+first+`"]
model = "fake-a"

[strategy.llm.agree_with]
type = "command"
command = ["python3", "-I", "`+other+`"]
model = "fake-b"
`), 0o600))

	runs := filepath.Join(dir, "verify-runs")
	verify := filepath.Join(dir, "verify.sh")
	require.NoError(t, os.WriteFile(verify, []byte("#!/bin/sh\necho x >> '"+runs+"'\nexit 0\n"), 0o755))

	ex := loadExample(t)
	ex.graft.Land.Resolver = config.GraftResolver{
		Enabled: true,
		Command: []string{buildAgentsTool(t, "./cmd/graft-resolve"), "--config", resolveConfig},
		Verify:  config.GraftResolverVerify{Enabled: true, Command: []string{verify}},
	}
	return ex, runs
}

// verifyRunsOf counts the lines that the runs of the verification left in runs.
func verifyRunsOf(t *testing.T, runs string) int {
	t.Helper()
	raw, err := os.ReadFile(runs)
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	return len(raw) / 2
}

// TestAgreeWithTheModelsThatAgreeLandThroughVerify and ...ThatDisagreeAreHeld are the agreement gate
// end to end (PLAN10 15.13): the real graft-resolve with two models in its llm strategy, the server
// with a verification that always passes. Two models that give the same file land through the
// verification; when the second differs, the same land is a merge conflict whose package holds the
// first model's proposal with the hold, the branch does not move, and the command never runs.
func TestAgreeWithTheModelsThatAgreeLandThroughVerify(t *testing.T) {
	t.Parallel()

	ex, runs := agreeResolverConfig(t, fakeModel)
	s := startGitaly(t, ex.graft, ex.sharedToken)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	const branch = "agree"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)
	first, err := sharedChange(ctx, repo, "agent", base, 1, true)
	require.NoError(t, err)
	_, err = landOn(ctx, repo, branch, first, true)
	require.NoError(t, err)
	second, err := sharedChange(ctx, repo, "agent", base, 2, true)
	require.NoError(t, err)
	res, err := landOn(ctx, repo, branch, second, true)
	require.NoError(t, err, "two models that agree, and a verification that passes: the resolution lands")
	msg := s.git(t, "log", "-1", "--format=%B", res.LandedCommitID)
	require.Contains(t, msg, "Graft-Resolved-By: graft-resolve/model")
	require.Contains(t, msg, "Graft-Checked-By: verify:verify.sh")
	require.Equal(t, 1, verifyRunsOf(t, runs), "the tree that lands was verified once")
}

func TestAgreeWithTheModelsThatDisagreeAreHeld(t *testing.T) {
	t.Parallel()

	ex, runs := agreeResolverConfig(t, otherModel)
	s := startGitaly(t, ex.graft, ex.sharedToken)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	const branch = "held"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)
	first, err := sharedChange(ctx, repo, "agent", base, 1, true)
	require.NoError(t, err)
	_, err = landOn(ctx, repo, branch, first, true)
	require.NoError(t, err)
	tip := s.git(t, "rev-parse", branch)

	second, err := sharedChange(ctx, repo, "agent", base, 2, true)
	require.NoError(t, err)
	_, err = landOn(ctx, repo, branch, second, true)
	require.True(t, sdk.IsMergeConflict(err), "a held resolution must not land: %v", err)
	require.Equal(t, tip, s.git(t, "rev-parse", branch), "the branch did not move")
	pkg, ok := sdk.LandConflict(err)
	require.True(t, ok)
	require.Len(t, pkg.GetFiles(), 1)
	f := pkg.GetFiles()[0]
	require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome())
	require.Contains(t, f.GetReason(), "held: the second model (fake-b) disagreed")
	require.Contains(t, f.GetReason(), "fake-b")
	require.NotNil(t, f.GetProposal(), "the first model's proposal is kept for the agent")
	require.Contains(t, f.GetProposal().GetExplanation(), "held: the second model (fake-b) disagreed")
	require.Equal(t, 0, verifyRunsOf(t, runs), "a held resolution is not even verified: verify.command never ran")
}

// TestStorageProposalsInAuthoritativeMode: the references of the proposals are written and pruned
// through the reference transaction, so in authoritative storage-core mode they are in the database
// as well as on disk, and the node is consistent after a proposal is written and after the change
// that it was made for has landed (which drops it).
func TestStorageProposalsInAuthoritativeMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	model := filepath.Join(dir, "model.py")
	require.NoError(t, os.WriteFile(model, []byte(fakeModel), 0o600))
	resolveConfig := filepath.Join(dir, "resolve.toml")
	require.NoError(t, os.WriteFile(resolveConfig, []byte(`[[strategy]]
name = "model"
type = "llm"
paths = ["shared-log.txt"]

[strategy.llm]
type = "command"
command = ["python3", "-I", "`+model+`"]
model = "fake"
`), 0o600))

	graft, tokens := workerGraft(1)
	graft.Land.Resolver = config.GraftResolver{
		Enabled: true,
		Command: []string{buildAgentsTool(t, "./cmd/graft-resolve"), "--config", resolveConfig},
	}
	cluster := newStorageCluster(t, sqlstore.SQLite, corestore.ModeAuthoritative, unbatched(graft))
	node := cluster.addNode(t)
	rel := node.createRepository(t).GetRelativePath()
	who := workerName(0)
	repo := node.agent(t, rel, tokens[who])
	ctx := node.ctx

	base, err := commitFile(ctx, repo, who, "main", "", sharedLogPath, sharedLogHeader)
	require.NoError(t, err)
	const branch = "propose"
	_, err = landAs(ctx, repo, who, branch, base)
	require.NoError(t, err)
	first, err := sharedChange(ctx, repo, who, base, 1, true)
	require.NoError(t, err)
	_, err = landOn(ctx, repo, branch, first, true)
	require.NoError(t, err)

	second, err := sharedChange(ctx, repo, who, base, 2, true)
	require.NoError(t, err)
	_, err = landOn(ctx, repo, branch, second, true)
	require.True(t, sdk.IsMergeConflict(err), "a model's resolution must not land: %v", err)
	pkg, ok := sdk.LandConflict(err)
	require.True(t, ok)
	ref := pkg.GetProposalRef()
	require.NotEmpty(t, ref)

	// The proposal reference went through the voter: it is in the database and on disk.
	require.Contains(t, node.dbRefs(t, rel), ref, "the proposal reference is in the database")
	require.Contains(t, node.localRefs(t, rel), ref, "and on disk")
	node.requireConsistent(t, rel)

	// The agent lands the proposal as its own resolution: the proposal of the change is dropped, again
	// through the voter, and the node stays consistent.
	sides, err := repo.ConflictSides(ctx, pkg)
	require.NoError(t, err)
	_, err = repo.Land(ctx, sdk.LandOptions{
		Branch:     branch,
		Commit:     second,
		User:       sdk.User{Name: who, Email: who + "@example.com"},
		Strategy:   sdk.LandMerge,
		Resolution: sdk.ResolutionFromProposals(pkg, sides),
	})
	require.NoError(t, err)
	require.NotContains(t, node.dbRefs(t, rel), ref, "the proposal of the change that landed is gone from the database")
	require.NotContains(t, node.localRefs(t, rel), ref, "and from the disk")
	node.requireConsistent(t, rel)
}
