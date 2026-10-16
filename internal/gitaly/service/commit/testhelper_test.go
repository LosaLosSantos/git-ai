package commit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/keyvalue"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/keyvalue/databasemgr"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/helper"
	logrus "github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

// setupCommitService makes a basic configuration and starts the service with the client.
func setupCommitService(
	tb testing.TB,
	ctx context.Context,
	opts ...testserver.GitalyServerOpt,
) (config.Cfg, gitalypb.CommitServiceClient) {
	cfg := testcfg.Build(tb)
	cfg.SocketPath = startTestServices(tb, cfg, opts...)

	return cfg, newCommitServiceClient(tb, cfg.SocketPath)
}

func startTestServices(tb testing.TB, cfg config.Cfg, opts ...testserver.GitalyServerOpt) string {
	tb.Helper()
	return testserver.RunGitalyServer(tb, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterCommitServiceServer(srv, NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
		gitalypb.RegisterHookServiceServer(srv, hook.NewServer(deps))
	}, opts...)
}

func dial(tb testing.TB, serviceSocketPath string) *grpc.ClientConn {
	tb.Helper()

	conn, err := client.New(testhelper.Context(tb), serviceSocketPath)
	require.NoError(tb, err)
	tb.Cleanup(func() { conn.Close() })

	return conn
}

func newCommitServiceClient(tb testing.TB, serviceSocketPath string) gitalypb.CommitServiceClient {
	tb.Helper()
	return gitalypb.NewCommitServiceClient(dial(tb, serviceSocketPath))
}

type gitCommitsGetter interface {
	GetCommits() []*gitalypb.GitCommit
}

func createCommits(tb testing.TB, cfg config.Cfg, repoPath, branch string, commitCount int, parent git.ObjectID) git.ObjectID {
	for i := 0; i < commitCount; i++ {
		var parents []git.ObjectID
		if parent != "" {
			parents = append(parents, parent)
		}

		parent = gittest.WriteCommit(tb, cfg, repoPath,
			gittest.WithBranch(branch),
			gittest.WithMessage(fmt.Sprintf("%s branch Empty commit %d", branch, i)),
			gittest.WithParents(parents...),
		)
	}

	return parent
}

func getAllCommits(tb testing.TB, getter func() (gitCommitsGetter, error)) []*gitalypb.GitCommit {
	tb.Helper()

	var commits []*gitalypb.GitCommit
	for {
		resp, err := getter()
		if errors.Is(err, io.EOF) {
			return commits
		}
		require.NoError(tb, err)

		commits = append(commits, resp.GetCommits()...)
	}
}

func writeCommit(
	tb testing.TB,
	ctx context.Context,
	cfg config.Cfg,
	repo *localrepo.Repo,
	opts ...gittest.WriteCommitOption,
) (git.ObjectID, *gitalypb.GitCommit) {
	tb.Helper()

	repoPath, err := repo.Path(ctx)
	require.NoError(tb, err)

	commitID := gittest.WriteCommit(tb, cfg, repoPath, opts...)
	commitProto, err := repo.ReadCommit(ctx, commitID.Revision())
	require.NoError(tb, err)

	return commitID, commitProto
}

func dbSetup(t *testing.T, ctx context.Context, cfg config.Cfg, dbPath string, storageName string, logger logrus.Logger) (keyvalue.Store, *databasemgr.DBManager) {
	dbMgr, err := databasemgr.NewDBManager(
		ctx,
		cfg.Storages,
		func(log logrus.Logger, path string) (keyvalue.Store, error) {
			return keyvalue.NewBadgerStore(log, filepath.Join(dbPath, path))
		},
		helper.NewNullTickerFactory(),
		logger,
	)
	require.NoError(t, err)
	t.Cleanup(dbMgr.Close)

	db, err := dbMgr.GetDB(storageName)
	require.NoError(t, err)

	return db, dbMgr
}
