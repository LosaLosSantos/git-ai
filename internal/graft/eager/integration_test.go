package eager_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	housekeepingmgr "github.com/feder-cr/open_scion/internal/git/housekeeping/manager"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/hook"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	hookservice "github.com/feder-cr/open_scion/internal/gitaly/service/hook"
	"github.com/feder-cr/open_scion/internal/gitaly/service/operations"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/gitaly/transaction"
	"github.com/feder-cr/open_scion/internal/gitlab"
	"github.com/feder-cr/open_scion/internal/graft/derived"
	_ "github.com/feder-cr/open_scion/internal/graft/derived/graph" // registers the graph warmer
	"github.com/feder-cr/open_scion/internal/graft/eager"
	"github.com/feder-cr/open_scion/internal/grpc/backchannel"
	grpcclient "github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// TestLand_EagerTriggers lands a commit through the real UserLand RPC and checks
// that the committed update reached the eager observer: the commit-graph chain
// was written and the graph entries of the landed history are stored, without
// any query having computed them.
func TestLand_EagerTriggers(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	testcfg.BuildGitalyHooks(t, cfg)
	testcfg.BuildGitalySSH(t, cfg)

	logger := testhelper.NewLogger(t)
	locator := config.NewLocator(cfg)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)

	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)

	build := func(repo storage.Repository) *localrepo.Repo {
		return localrepo.New(logger, locator, gitCmdFactory, catfileCache, repo)
	}

	pool := derived.NewPool(logger, 2, 16, eager.WarmEnvOpener(build, logger, catfileCache))
	poolCtx, cancelPool := context.WithCancel(ctx)
	t.Cleanup(cancelPool)
	go pool.Run(poolCtx)
	t.Cleanup(pool.Close)

	housekeepingManager := housekeepingmgr.New(cfg.Prometheus, logger, nil, nil)

	observer := eager.NewObserver(ctx, logger,
		eager.Options{Eager: true, CommitGraph: true, Debounce: 10 * time.Millisecond},
		eager.Deps{
			Enqueuer:         pool,
			ListNewCommits:   eager.NewCommitsLister(build),
			WriteCommitGraph: eager.CommitGraphWriterFor(build, housekeepingManager),
		},
	)
	t.Cleanup(observer.Close)

	hookMgr := hook.NewManager(
		cfg, locator, logger, gitCmdFactory, nil,
		transaction.NewManager(cfg, logger, backchannel.NewRegistry()),
		gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive),
		hook.NewPostReceiveRegistry(),
		hook.WithEagerObserver(observer),
	)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	child := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

	serverSocketPath := testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterOperationServiceServer(srv, operations.NewServer(deps))
		gitalypb.RegisterHookServiceServer(srv, hookservice.NewServer(deps))
	}, testserver.WithHookManager(hookMgr), testserver.WithHousekeepingManager(housekeepingManager))
	cfg.SocketPath = serverSocketPath

	conn, err := grpcclient.New(testhelper.Context(t), serverSocketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	client := gitalypb.NewOperationServiceClient(conn)
	callCtx := testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))

	resp, err := client.UserLand(callCtx, &gitalypb.UserLandRequest{
		Repository: repoProto,
		User:       gittest.TestUser,
		Branch:     []byte(git.DefaultBranch),
		CommitId:   child.String(),
	})
	require.NoError(t, err)
	require.Equal(t, child.String(), resp.GetBranchUpdate().GetCommitId())

	require.Eventually(t, func() bool {
		return fileExists(filepath.Join(repoPath, "objects", "info", "commit-graphs", "commit-graph-chain"))
	}, 10*time.Second, 10*time.Millisecond, "the land writes the commit-graph chain")

	store := derived.NewStore(repoPath)
	for _, commit := range []git.ObjectID{base, child} {
		require.Eventually(t, func() bool {
			_, ok, err := store.Kind(graphKindName, graphKindVersion).Get(commit)
			return err == nil && ok
		}, 10*time.Second, 10*time.Millisecond, "the graph entry of %s is warmed", commit)
	}
}

// graphKindName and graphKindVersion identify the graph kind in the derived
// store, see the graph package.
const (
	graphKindName    = "graph"
	graphKindVersion = 1
)

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
