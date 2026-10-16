package operations

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// landAncestrySetup creates the history of a scenario in repoPath, with main as the branch that
// lands, and returns the commit to land.
type landAncestrySetup func(t *testing.T, cfg config.Cfg, repoPath string) git.ObjectID

// landAncestrySummary is what a landing reports that must not depend on whether the graph index
// answers the ancestry questions. Commit IDs of rebased commits depend on the time of the rebase,
// so the summary carries the tree and the shape of the new tip instead.
type landAncestrySummary struct {
	code              codes.Code
	rewritten         bool
	alreadyLanded     bool
	attempts          int32
	oldTipIsAncestor  bool
	tipTree           string
	tipParentCount    int
	tipHasParentOfOld bool
}

func TestUserLand_graphAncestryMatchesGit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc  string
		setup landAncestrySetup
	}{
		{
			desc: "fast-forward",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.ObjectID {
				base := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithBranch(git.DefaultBranch),
					gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
				)
				return gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithParents(base),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
						gittest.TreeEntry{Mode: "100644", Path: "b", Content: "b"},
					),
				)
			},
		},
		{
			desc: "already landed",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.ObjectID {
				base := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithBranch(git.DefaultBranch),
					gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
				)
				gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithParents(base),
					gittest.WithBranch(git.DefaultBranch),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
						gittest.TreeEntry{Mode: "100644", Path: "b", Content: "b"},
					),
				)
				return base
			},
		},
		{
			desc: "rebase onto diverged tip",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.ObjectID {
				base := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithBranch(git.DefaultBranch),
					gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
				)
				gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithParents(base),
					gittest.WithBranch(git.DefaultBranch),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
						gittest.TreeEntry{Mode: "100644", Path: "t", Content: "t"},
					),
				)
				return gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithParents(base),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"},
						gittest.TreeEntry{Mode: "100644", Path: "c", Content: "c"},
					),
				)
			},
		},
		{
			desc: "merge conflict",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.ObjectID {
				base := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithBranch(git.DefaultBranch),
					gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base"}),
				)
				gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithParents(base),
					gittest.WithBranch(git.DefaultBranch),
					gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "tip"}),
				)
				return gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithParents(base),
					gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "commit"}),
				)
			},
		},
		{
			desc: "unrelated history",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.ObjectID {
				gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithBranch(git.DefaultBranch),
					gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}),
				)
				return gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "c", Content: "c"}),
				)
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			withoutGraph, storedWithout := runLandAncestryScenario(t, false, false, tc.setup)
			withGraph, storedWith := runLandAncestryScenario(t, true, false, tc.setup)
			withGraphNoStore, storedNoStore := runLandAncestryScenario(t, true, true, tc.setup)

			require.Equal(t, withoutGraph, withGraph, "UserLand outcome with and without graph_ancestry")
			require.Equal(t, withoutGraph, withGraphNoStore, "UserLand outcome with graph_ancestry and the derived store disabled")
			require.False(t, storedWithout, "git ancestry must not write the graph index")
			require.True(t, storedWith, "graph ancestry must write the graph index")
			require.False(t, storedNoStore, "graph ancestry must not write the graph index when the derived store is disabled")
		})
	}
}

// runLandAncestryScenario runs UserLand of the commit returned by setup on a fresh repository, with
// graph_ancestry set to graphAncestry and [graft.derived] disabled set to derivedDisabled. It
// returns the summary of the landing and whether the graph index was stored in the repository.
func runLandAncestryScenario(t *testing.T, graphAncestry, derivedDisabled bool, setup landAncestrySetup) (landAncestrySummary, bool) {
	t.Helper()

	cfg := testcfg.Build(t)
	cfg.Graft.Land.GraphAncestry = graphAncestry
	cfg.Graft.Derived.Disabled = derivedDisabled
	ctx, cfg, client := setupOperationsServiceWithCfg(t,
		featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false),
		cfg,
	)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commit := setup(t, cfg, repoPath)
	oldTip := gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/"+git.DefaultBranch)

	resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, commit))

	summary := landAncestrySummary{code: status.Code(err)}
	if err == nil {
		summary.rewritten = resp.GetRewritten()
		summary.alreadyLanded = resp.GetAlreadyLanded()
		summary.attempts = resp.GetAttempts()
	}

	tip := gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/"+git.DefaultBranch)
	summary.tipTree = string(bytes.TrimSpace(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", tip.String()+"^{tree}")))

	shape := strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-list", "--parents", "-n", "1", tip.String())))
	summary.tipParentCount = len(shape) - 1

	ancestors := strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-list", tip.String())))
	summary.oldTipIsAncestor = slices.Contains(ancestors, oldTip.String())
	summary.tipHasParentOfOld = slices.Contains(shape[1:], oldTip.String())

	_, statErr := os.Stat(filepath.Join(repoPath, "graft", "derived", "graph"))
	return summary, statErr == nil
}
