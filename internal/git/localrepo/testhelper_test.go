package localrepo

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

type setupRepoConfig struct {
	// disableHooks will disable the use of hooks.
	disableHooks bool
}

type setupRepoOption func(*setupRepoConfig)

func withDisabledHooks() setupRepoOption {
	return func(cfg *setupRepoConfig) {
		cfg.disableHooks = true
	}
}

func setupRepo(t *testing.T, opts ...setupRepoOption) (config.Cfg, *Repo, string) {
	t.Helper()

	var setupRepoCfg setupRepoConfig
	for _, opt := range opts {
		opt(&setupRepoCfg)
	}

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	var commandFactoryOpts []gitcmd.ExecCommandFactoryOption
	if setupRepoCfg.disableHooks {
		commandFactoryOpts = append(commandFactoryOpts, gitcmd.WithSkipHooks())
	}

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	gitCmdFactory := gittest.NewCommandFactory(t, cfg, commandFactoryOpts...)
	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)
	return cfg, New(testhelper.NewLogger(t), config.NewLocator(cfg), gitCmdFactory, catfileCache, repoProto), repoPath
}
