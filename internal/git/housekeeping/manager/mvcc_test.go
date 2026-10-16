package manager

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	gitalycfgprom "github.com/feder-cr/git-ai/internal/gitaly/config/prometheus"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func TestRunMVCCGarbageCollection(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	t.Run("does nothing for a repository on another reference backend", func(t *testing.T) {
		t.Parallel()

		cfg := testcfg.Build(t)
		repoProto, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
			SkipCreationViaService: true,
		})
		repo := localrepo.NewTestRepo(t, cfg, repoProto)

		manager := New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil)
		require.NoError(t, manager.RunMVCCGarbageCollection(ctx, repo, housekeeping.DefaultMVCCConfig()))
	})

	t.Run("collects an MVCC repository", func(t *testing.T) {
		t.Parallel()

		cfg := testcfg.Build(t)
		repoProto, repoPath := gittest.CreateMVCCRepository(t, ctx, cfg)
		repo := localrepo.NewTestRepo(t, cfg, repoProto)

		// The commit publishes a manifest, so there is something to judge reachability
		// by. Nothing is old enough to be removed yet.
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

		manager := New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil)
		require.NoError(t, manager.RunMVCCGarbageCollection(ctx, repo, housekeeping.DefaultMVCCConfig()))
	})

	t.Run("fails when the configuration allows no work", func(t *testing.T) {
		t.Parallel()

		cfg := testcfg.Build(t)
		repoProto, _ := gittest.CreateMVCCRepository(t, ctx, cfg)
		repo := localrepo.NewTestRepo(t, cfg, repoProto)

		manager := New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil)
		require.ErrorContains(t,
			manager.RunMVCCGarbageCollection(ctx, repo, housekeeping.MVCCConfig{}),
			"at least one manifest must be kept")
	})
}
