package service

import (
	"context"

	"github.com/feder-cr/open_scion/internal/backup"
	"github.com/feder-cr/open_scion/internal/bundleuri"
	"github.com/feder-cr/open_scion/internal/cache"
	"github.com/feder-cr/open_scion/internal/cgroups"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	housekeepingmgr "github.com/feder-cr/open_scion/internal/git/housekeeping/manager"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	gitalyhook "github.com/feder-cr/open_scion/internal/gitaly/hook"
	"github.com/feder-cr/open_scion/internal/gitaly/hook/updateref"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/gitaly/storage/counter"
	"github.com/feder-cr/open_scion/internal/gitaly/storage/relational"
	"github.com/feder-cr/open_scion/internal/gitaly/storage/storagemgr/partition/migration"
	"github.com/feder-cr/open_scion/internal/gitaly/transaction"
	"github.com/feder-cr/open_scion/internal/gitlab"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/landlane"
	"github.com/feder-cr/open_scion/internal/grpc/backchannel"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/grpc/middleware/limithandler"
	"github.com/feder-cr/open_scion/internal/limiter"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/streamcache"
)

// Dependencies assembles set of components required by different kinds of services.
type Dependencies struct {
	Logger                 log.Logger
	Cfg                    config.Cfg
	GitalyHookManager      gitalyhook.Manager
	TransactionManager     transaction.Manager
	StorageLocator         storage.Locator
	ClientPool             *client.Pool
	GitCmdFactory          gitcmd.CommandFactory
	BackchannelRegistry    *backchannel.Registry
	GitlabClient           gitlab.Client
	CatfileCache           catfile.Cache
	DiskCache              cache.Cache
	PackObjectsCache       streamcache.Cache
	PackObjectsLimiter     limiter.Limiter
	LimitHandler           *limithandler.LimiterMiddleware
	RepositoryCounter      *counter.RepositoryCounter
	UpdaterWithHooks       *updateref.UpdaterWithHooks
	HousekeepingManager    housekeepingmgr.Manager
	Node                   storage.Node
	BackupSink             *backup.Sink
	BackupLocator          backup.Locator
	PostReceiveRegistry    *gitalyhook.PostReceiveRegistry
	BundleURIManager       *bundleuri.GenerationManager
	LocalRepositoryFactory localrepo.Factory
	MigrationStateManager  migration.StateManager
	ArchiveCache           streamcache.Cache
	PoolMetadataStore      relational.PoolStore
	ObjectPoolStateManager relational.ObjectPoolStateManager
	CgroupsManager         cgroups.Manager
	// GraftLandLanes are the land batching lanes of the process. Services are registered once per
	// listener, and every operations server takes its lanes from here, so that the requests for
	// one branch are batched in one lane whichever listener they arrive on.
	GraftLandLanes *landlane.Shared
	// GraftStorage is the storage core (doc/graft/STORAGE.md), nil when it is off.
	GraftStorage *corestore.Core
	// GraftStorageSyncer catches up the local repositories in authoritative mode. It is nil when
	// the storage core is off.
	GraftStorageSyncer corestore.Freshener
	// ShutdownContext is cancelled when the server begins shutting down. Services can
	// use it to terminate work that intentionally ignores the per-request context, so
	// that such work does not outlive the graceful shutdown period.
	ShutdownContext context.Context
}

// GetShutdownContext returns a context that is cancelled once the graceful
// shutdown period has elapsed. It returns a non-cancellable context.Background()
// when no shutdown context was wired up, so callers can register cancellation
// via context.AfterFunc without a nil check (AfterFunc on a parent that can
// never be cancelled registers nothing).
func (dc *Dependencies) GetShutdownContext() context.Context {
	if dc.ShutdownContext == nil {
		return context.Background()
	}
	return dc.ShutdownContext
}

// GetLogger returns the logger.
func (dc *Dependencies) GetLogger() log.Logger {
	return dc.Logger
}

// GetCfg returns service configuration.
func (dc *Dependencies) GetCfg() config.Cfg {
	return dc.Cfg
}

// GetHookManager returns hook manager.
func (dc *Dependencies) GetHookManager() gitalyhook.Manager {
	return dc.GitalyHookManager
}

// GetTxManager returns transaction manager.
func (dc *Dependencies) GetTxManager() transaction.Manager {
	return dc.TransactionManager
}

// GetLocator returns storage locator.
func (dc *Dependencies) GetLocator() storage.Locator {
	return dc.StorageLocator
}

// GetConnsPool returns gRPC connection pool.
func (dc *Dependencies) GetConnsPool() *client.Pool {
	return dc.ClientPool
}

// GetGitCmdFactory returns git commands factory.
func (dc *Dependencies) GetGitCmdFactory() gitcmd.CommandFactory {
	return dc.GitCmdFactory
}

// GetCgroupsManager returns the cgroups manager.
func (dc *Dependencies) GetCgroupsManager() cgroups.Manager {
	return dc.CgroupsManager
}

// GetBackchannelRegistry returns a registry of the backchannels.
func (dc *Dependencies) GetBackchannelRegistry() *backchannel.Registry {
	return dc.BackchannelRegistry
}

// GetGitlabClient returns client to access GitLab API.
func (dc *Dependencies) GetGitlabClient() gitlab.Client {
	return dc.GitlabClient
}

// GetCatfileCache returns catfile cache.
func (dc *Dependencies) GetCatfileCache() catfile.Cache {
	return dc.CatfileCache
}

// GetDiskCache returns the disk cache.
func (dc *Dependencies) GetDiskCache() cache.Cache {
	return dc.DiskCache
}

// GetPackObjectsCache returns the pack-objects cache.
func (dc *Dependencies) GetPackObjectsCache() streamcache.Cache {
	return dc.PackObjectsCache
}

// GetLimitHandler returns the RPC limit handler.
func (dc *Dependencies) GetLimitHandler() *limithandler.LimiterMiddleware {
	return dc.LimitHandler
}

// GetRepositoryCounter returns the repository counter.
func (dc *Dependencies) GetRepositoryCounter() *counter.RepositoryCounter {
	return dc.RepositoryCounter
}

// GetUpdaterWithHooks returns the updater with hooks executor.
func (dc *Dependencies) GetUpdaterWithHooks() *updateref.UpdaterWithHooks {
	return dc.UpdaterWithHooks
}

// GetHousekeepingManager returns the housekeeping manager.
func (dc *Dependencies) GetHousekeepingManager() housekeepingmgr.Manager {
	return dc.HousekeepingManager
}

// GetPackObjectsLimiter returns the pack-objects limiter.
func (dc *Dependencies) GetPackObjectsLimiter() limiter.Limiter {
	return dc.PackObjectsLimiter
}

// GetNode returns the Node.
func (dc *Dependencies) GetNode() storage.Node {
	return dc.Node
}

// GetBackupSink returns the backup.Sink.
func (dc *Dependencies) GetBackupSink() *backup.Sink {
	return dc.BackupSink
}

// GetBackupLocator returns the backup.Locator.
func (dc *Dependencies) GetBackupLocator() backup.Locator {
	return dc.BackupLocator
}

// GetPostReceiveRegistry returns the PostReceiveRegistry.
func (dc *Dependencies) GetPostReceiveRegistry() *gitalyhook.PostReceiveRegistry {
	return dc.PostReceiveRegistry
}

// GetRepositoryFactory returns the RepositoryFactory
func (dc *Dependencies) GetRepositoryFactory() localrepo.Factory {
	return dc.LocalRepositoryFactory
}

// GetBundleURIManager returns the bundleuri.GenerationManager
func (dc *Dependencies) GetBundleURIManager() *bundleuri.GenerationManager {
	return dc.BundleURIManager
}

// GetMigrationStateManager returns the migration.StateManager
func (dc *Dependencies) GetMigrationStateManager() migration.StateManager {
	return dc.MigrationStateManager
}

// GetArchiveCache returns the archive cache
func (dc *Dependencies) GetArchiveCache() streamcache.Cache {
	return dc.ArchiveCache
}

// GetPoolMetadataStore returns the pool metadata store
func (dc *Dependencies) GetPoolMetadataStore() relational.PoolStore {
	return dc.PoolMetadataStore
}

// GetGraftLandLanes returns the land batching lanes of the process. It is nil when the dependencies
// were built without them.
func (dc *Dependencies) GetGraftLandLanes() *landlane.Shared {
	return dc.GraftLandLanes
}

// GetGraftStorage returns the storage core, nil when it is off.
func (dc *Dependencies) GetGraftStorage() *corestore.Core {
	return dc.GraftStorage
}

// GetGraftStorageSyncer returns the syncer of the storage core. It never returns nil: when the
// storage core is off it returns a Freshener that does nothing.
func (dc *Dependencies) GetGraftStorageSyncer() corestore.Freshener {
	if dc.GraftStorageSyncer == nil {
		return corestore.NopFreshener{}
	}
	return dc.GraftStorageSyncer
}

// GetObjectPoolStateManager returns the object pool state manager
func (dc *Dependencies) GetObjectPoolStateManager() relational.ObjectPoolStateManager {
	return dc.ObjectPoolStateManager
}
