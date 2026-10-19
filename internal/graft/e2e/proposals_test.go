package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// fakeModel is a model for the llm strategy's command provider: it answers every region with the
// CODE of the request unchanged, OURS' version, between the PATCHED CODE lines. No network.
const fakeModel = `import json, re, sys
req = json.load(sys.stdin)
m = re.search(r"===== BEGIN CODE ([0-9a-f]+) =====\n(.*?)===== END CODE ([0-9a-f]+) =====", req["prompt"], re.S)
n, code = m.group(1), m.group(2)
sys.stdout.write("===== BEGIN PATCHED CODE %s =====\n%s===== END PATCHED CODE %s =====\n" % (n, code, n))
`

// TestPhase2ModelResolutionIsProposedThenLandedByTheAgent is decision 1 of PLAN10 14 end to end:
// the real graft-resolve answers with its llm strategy, the server lands nothing and proposes the
// answer in the conflict package, the SDK reads the proposal, and the agent lands it as its own.
// The strategy is named "model", not "llm": the server's fallback on the resolver's name cannot
// catch it, so the proposal flag that graft-resolve sets is what makes it a proposal.
func TestPhase2ModelResolutionIsProposedThenLandedByTheAgent(t *testing.T) {
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

	ex := loadExample(t)
	ex.graft.Land.Resolver = config.GraftResolver{
		Enabled: true,
		Command: []string{buildAgentsTool(t, "./cmd/graft-resolve"), "--config", resolveConfig},
	}
	s := startGitaly(t, ex.graft, ex.sharedToken)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	const branch = "propose"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)
	first, err := sharedChange(ctx, repo, "agent", base, 1, true)
	require.NoError(t, err)
	_, err = landOn(ctx, repo, branch, first, true)
	require.NoError(t, err)
	tip := s.git(t, "rev-parse", branch)

	// The second change conflicts; the model resolves it, and the server only proposes that.
	second, err := sharedChange(ctx, repo, "agent", base, 2, true)
	require.NoError(t, err)
	_, err = landOn(ctx, repo, branch, second, true)
	require.True(t, sdk.IsMergeConflict(err), "a model's resolution must not land: %v", err)
	require.Equal(t, tip, s.git(t, "rev-parse", branch), "the branch did not move")
	pkg, ok := sdk.LandConflict(err)
	require.True(t, ok)
	require.NotEmpty(t, pkg.GetProposalRef())
	require.Len(t, pkg.GetFiles(), 1)
	f := pkg.GetFiles()[0]
	require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome())
	require.Equal(t, "graft-resolve/model", f.GetProposal().GetResolver(), "the name holds no \"llm\": the flag decided")

	sides, err := repo.ConflictSides(ctx, pkg)
	require.NoError(t, err)
	prop := sides[sharedLogPath].Proposal
	require.NotNil(t, prop)
	require.Equal(t, string(sides[sharedLogPath].Ours), string(prop.Content), "the fake model kept OURS")

	// The agent lands the proposal as its own resolution.
	res, err := repo.Land(ctx, sdk.LandOptions{
		Branch:     branch,
		Commit:     second,
		User:       sdk.User{Name: "planner", Email: "planner@example.com"},
		Strategy:   sdk.LandMerge,
		Resolution: sdk.ResolutionFromProposals(pkg, sides),
	})
	require.NoError(t, err)
	msg := s.git(t, "log", "-1", "--format=%B", res.LandedCommitID)
	require.Contains(t, msg, "Graft-Resolved-By: agent/planner")
	require.NotContains(t, msg, "Graft-Resolved-By: graft-resolve")
	require.Empty(t, s.git(t, "for-each-ref", pkg.GetProposalRef()), "the proposal of the change that landed is gone")
}
