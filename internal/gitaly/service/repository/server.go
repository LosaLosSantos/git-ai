package repository

import (
	"context"

	"github.com/feder-cr/git-ai/internal/backup"
	"github.com/feder-cr/git-ai/internal/bundleuri"
	"github.com/feder-cr/git-ai/internal/cgroups"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	housekeepingmgr "github.com/feder-cr/git-ai/internal/git/housekeeping/manager"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/quarantine"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/counter"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/storagemgr/partition/migration"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/streamcache"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/unarycache"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedRepositoryServiceServer
	logger                log.Logger
	conns                 *client.Pool
	locator               storage.Locator
	txManager             transaction.Manager
	node                  storage.Node
	gitCmdFactory         gitcmd.CommandFactory
	cfg                   config.Cfg
	loggingCfg            config.Logging
	catfileCache          catfile.Cache
	housekeepingManager   housekeepingmgr.Manager
	backupSink            *backup.Sink
	backupLocator         backup.Locator
	repositoryCounter     *counter.RepositoryCounter
	localRepoFactory      localrepo.Factory
	licenseCache          *unarycache.Cache[git.ObjectID, *gitalypb.FindLicenseResponse]
	bundleURIManager      *bundleuri.GenerationManager
	migrationStateManager migration.StateManager
	archiveCache          streamcache.Cache
	cgroupsManager        cgroups.Manager
}

// NewServer creates a new instance of a gRPC repo server
func NewServer(deps *service.Dependencies) gitalypb.RepositoryServiceServer {
	return &server{
		logger:                deps.GetLogger(),
		locator:               deps.GetLocator(),
		txManager:             deps.GetTxManager(),
		node:                  deps.GetNode(),
		gitCmdFactory:         deps.GetGitCmdFactory(),
		conns:                 deps.GetConnsPool(),
		cfg:                   deps.GetCfg(),
		loggingCfg:            deps.GetCfg().Logging,
		catfileCache:          deps.GetCatfileCache(),
		housekeepingManager:   deps.GetHousekeepingManager(),
		backupSink:            deps.GetBackupSink(),
		backupLocator:         deps.GetBackupLocator(),
		repositoryCounter:     deps.GetRepositoryCounter(),
		localRepoFactory:      deps.GetRepositoryFactory(),
		licenseCache:          newLicenseCache(),
		bundleURIManager:      deps.GetBundleURIManager(),
		migrationStateManager: deps.GetMigrationStateManager(),
		archiveCache:          deps.GetArchiveCache(),
		cgroupsManager:        deps.GetCgroupsManager(),
	}
}

func (s *server) quarantinedRepo(ctx context.Context, repo *gitalypb.Repository) (*quarantine.Dir, *localrepo.Repo, func(), error) {
	quarantineDir, cleanup, err := quarantine.New(ctx, repo, s.logger, s.locator)
	if err != nil {
		return nil, nil, nil, structerr.NewInternal("creating object quarantine: %w", err)
	}

	quarantineRepo := s.localRepoFactory.Build(quarantineDir.QuarantinedRepo())
	return quarantineDir, quarantineRepo, cleanup, nil
}
