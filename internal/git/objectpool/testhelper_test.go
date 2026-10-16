package objectpool

import (
	"context"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	housekeepingmgr "github.com/feder-cr/git-ai/internal/git/housekeeping/manager"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

type setupObjectPoolConfig struct {
	logger log.Logger
}

type setupObjectPoolOption func(*setupObjectPoolConfig)

func withLogger(logger log.Logger) setupObjectPoolOption {
	return func(cfg *setupObjectPoolConfig) {
		cfg.logger = logger
	}
}

func setupObjectPool(t *testing.T, ctx context.Context, opts ...setupObjectPoolOption) (config.Cfg, *ObjectPool, *localrepo.Repo) {
	t.Helper()

	cfg := testcfg.Build(t)
	pool, repo := setupObjectPoolWithCfg(t, ctx, cfg, opts...)
	return cfg, pool, repo
}

func setupObjectPoolWithCfg(t *testing.T, ctx context.Context, cfg config.Cfg, opts ...setupObjectPoolOption) (*ObjectPool, *localrepo.Repo) {
	t.Helper()

	var setupCfg setupObjectPoolConfig
	for _, opt := range opts {
		opt(&setupCfg)
	}
	if setupCfg.logger == nil {
		setupCfg.logger = testhelper.NewLogger(t)
	}

	repoProto, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	gitCommandFactory := gittest.NewCommandFactory(t, cfg, gitcmd.WithSkipHooks())

	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)
	txManager := transaction.NewManager(cfg, setupCfg.logger, backchannel.NewRegistry())

	pool, err := Create(
		ctx,
		setupCfg.logger,
		config.NewLocator(cfg),
		gitCommandFactory,
		catfileCache,
		txManager,
		housekeepingmgr.New(cfg.Prometheus, setupCfg.logger, txManager, nil),
		&gitalypb.ObjectPool{
			Repository: &gitalypb.Repository{
				StorageName:  repo.GetStorageName(),
				RelativePath: gittest.NewObjectPoolName(t),
			},
		},
		repo,
	)
	require.NoError(t, err)

	return pool, repo
}

func hashDependentSize(tb testing.TB, sha1Size, sha256Size uint64) uint64 {
	return gittest.ObjectHashDependent(tb, map[string]uint64{
		"sha1":   sha1Size,
		"sha256": sha256Size,
	})
}
