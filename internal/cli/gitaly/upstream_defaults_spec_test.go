package gitaly

import (
	"path/filepath"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/service/setup"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/stretchr/testify/require"
)

// commitGraphPath is where the commit-graph writer puts its file in a repository.
func commitGraphPath(repoPath string) string {
	return filepath.Join(repoPath, "objects", "info", "commit-graph")
}

// TestSpecPlainConfigDerivedDefaults checks that a configuration without any
// [graft] settings has the derived features switched off.
func TestSpecPlainConfigDerivedDefaults(t *testing.T) {
	cfg := testcfg.Build(t)

	require.False(t, cfg.Graft.Derived.CommitGraph, "commit_graph defaults to false (opt-in)")
	require.False(t, cfg.Graft.Derived.Eager, "eager warming must not be on by default")
	require.False(t, cfg.Graft.Derived.Disabled, "derived features are not disabled by default")
}

// TestSpecPlainConfigStartsNoCommitGraphWork checks that a server started with
// a plain configuration writes no commit-graph into a repository it serves.
func TestSpecPlainConfigStartsNoCommitGraphWork(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	_, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	testserver.RunGitalyServer(t, cfg, setup.RegisterAll)

	require.NoFileExists(t, commitGraphPath(repoPath))
}

// TestSpecCommitGraphDisabledOverridesRequest checks that commit_graph = true
// has no effect when derived features are disabled.
func TestSpecCommitGraphDisabledOverridesRequest(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Derived.CommitGraph = true
	cfg.Graft.Derived.Disabled = true

	_, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	testserver.RunGitalyServer(t, cfg, setup.RegisterAll)

	require.NoFileExists(t, commitGraphPath(repoPath))
}

// TestSpecEagerOnlyWritesNoCommitGraph checks that eager warming on its own
// does not write a commit-graph; only commit_graph = true does that.
func TestSpecEagerOnlyWritesNoCommitGraph(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Derived.Eager = true
	cfg.Graft.Derived.CommitGraph = false

	_, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	testserver.RunGitalyServer(t, cfg, setup.RegisterAll)

	require.NoFileExists(t, commitGraphPath(repoPath))
}

// TestSpecDerivedFlagsReadBack checks that the commit_graph and eager settings
// are carried in the configuration as set, so the serve block can branch on them.
func TestSpecDerivedFlagsReadBack(t *testing.T) {
	cfg := testcfg.Build(t)
	cfg.Graft.Derived.CommitGraph = true
	cfg.Graft.Derived.Eager = true

	require.True(t, cfg.Graft.Derived.CommitGraph)
	require.True(t, cfg.Graft.Derived.Eager)
	require.False(t, cfg.Graft.Derived.Disabled)
}
