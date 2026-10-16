package objectpool

import (
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	housekeepingmgr "github.com/feder-cr/git-ai/internal/git/housekeeping/manager"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/counter"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/relational"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/storagemgr/partition/migration"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedObjectPoolServiceServer
	logger                 log.Logger
	locator                storage.Locator
	gitCmdFactory          gitcmd.CommandFactory
	catfileCache           catfile.Cache
	txManager              transaction.Manager
	node                   storage.Node
	housekeepingManager    housekeepingmgr.Manager
	repositoryCounter      *counter.RepositoryCounter
	localRepoFactory       localrepo.Factory
	migrationStateManager  migration.StateManager
	objectPoolStateManager relational.ObjectPoolStateManager
}

// NewServer creates a new instance of a gRPC repo server
func NewServer(deps *service.Dependencies) gitalypb.ObjectPoolServiceServer {
	return &server{
		logger:                 deps.GetLogger(),
		locator:                deps.GetLocator(),
		gitCmdFactory:          deps.GetGitCmdFactory(),
		catfileCache:           deps.GetCatfileCache(),
		txManager:              deps.GetTxManager(),
		node:                   deps.GetNode(),
		housekeepingManager:    deps.GetHousekeepingManager(),
		repositoryCounter:      deps.GetRepositoryCounter(),
		localRepoFactory:       deps.GetRepositoryFactory(),
		migrationStateManager:  deps.GetMigrationStateManager(),
		objectPoolStateManager: deps.GetObjectPoolStateManager(),
	}
}
