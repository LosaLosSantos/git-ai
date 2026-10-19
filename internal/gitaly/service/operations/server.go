package operations

import (
	"context"

	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/quarantine"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

//nolint:revive // This is unintentionally missing documentation.
type Server struct {
	gitalypb.UnimplementedOperationServiceServer
	logger           log.Logger
	hookManager      hook.Manager
	txManager        transaction.Manager
	locator          storage.Locator
	conns            *client.Pool
	catfileCache     catfile.Cache
	updater          *updateref.UpdaterWithHooks
	gitConfig        config.Git
	graft            config.Graft
	localRepoFactory localrepo.Factory
	// resolveCfg configures conflict resolution of UserLand.
	resolveCfg config.GraftResolver
	// conflictResolver answers the resolve requests of UserLand. Nil when resolution is disabled.
	conflictResolver *resolver.Client
	// landLanes batches the UserLand requests of a branch. Nil disables batching, and every
	// request then lands on its own. The lanes are shared by all servers of the process.
	landLanes *landBatchLanes
	// shutdownCtx is cancelled once the graceful shutdown period has elapsed. It ends the land
	// batches, which do not end with the request of any submitter.
	shutdownCtx context.Context
	// storageSync catches up the local repository from the storage core before a land is retried,
	// see doc/graft/STORAGE.md. It does nothing unless the storage core is authoritative.
	storageSync corestore.Freshener
	// graftStorage is the storage core. In its authoritative mode the land identities are looked
	// up in its database (doc/graft/PLAN11.md). Nil without a storage core.
	graftStorage *corestore.Core
	// testHook, when set by a test, is called at named points of a request.
	testHook func(point string)
}

// NewServer creates a new instance of a grpc OperationServiceServer
func NewServer(deps *service.Dependencies) *Server {
	resolveCfg := deps.GetCfg().Graft.Land.Resolver

	conflictResolver, err := resolver.New(resolveCfg)
	if err != nil {
		// An invalid resolver configuration disables resolution rather than the server. Landings
		// that ask for resolution then fail with the merge conflict, as without resolution.
		deps.GetLogger().WithError(err).Error("graft: conflict resolution of land is disabled")
		conflictResolver = nil
	}
	if conflictResolver != nil && resolverActorsUnmatchable(deps.GetCfg().Graft) {
		deps.GetLogger().Warn("graft: [graft.land.resolver] actors is set but no [[graft.agent]] is " +
			"configured, so callers carry no identity and no landing may resolve conflicts")
	}

	s := &Server{
		logger:           deps.GetLogger(),
		hookManager:      deps.GetHookManager(),
		txManager:        deps.GetTxManager(),
		locator:          deps.GetLocator(),
		conns:            deps.GetConnsPool(),
		catfileCache:     deps.GetCatfileCache(),
		updater:          deps.GetUpdaterWithHooks(),
		gitConfig:        deps.GetCfg().Git,
		graft:            deps.GetCfg().Graft,
		localRepoFactory: deps.GetRepositoryFactory(),
		resolveCfg:       resolveCfg,
		conflictResolver: conflictResolver,
		shutdownCtx:      deps.GetShutdownContext(),
		storageSync:      deps.GetGraftStorageSyncer(),
		graftStorage:     deps.GetGraftStorage(),
	}

	// The lanes may run the batches with the server, so they are taken after it is created.
	s.landLanes = newLandBatchLanes(s, deps.GetCfg().Graft, deps.GetGraftLandLanes(), deps.GetLogger())

	return s
}

// resolverActorsUnmatchable reports whether the actors of conflict resolution can match no caller.
// The actor is the authenticated identity, which only agent authentication sets: an agent name,
// or "shared" for callers of the shared token. Without [[graft.agent]] no caller has an identity,
// so a non-empty actor list, for example of GitLab user names, admits nobody.
func resolverActorsUnmatchable(cfg config.Graft) bool {
	return len(cfg.Land.Resolver.Actors) > 0 && len(cfg.Agents) == 0
}

func (s *Server) quarantinedRepo(ctx context.Context, repo *gitalypb.Repository) (*quarantine.Dir, *localrepo.Repo, func(), error) {
	quarantineDir, cleanup, err := quarantine.New(ctx, repo, s.logger, s.locator)
	if err != nil {
		return nil, nil, nil, structerr.NewInternal("creating object quarantine: %w", err)
	}

	quarantineRepo := s.localRepoFactory.Build(quarantineDir.QuarantinedRepo())
	return quarantineDir, quarantineRepo, cleanup, nil
}

// ensureStorageFresh catches up the local repository from the storage core. It does nothing unless
// the storage core is authoritative.
func (s *Server) ensureStorageFresh(ctx context.Context, repo *gitalypb.Repository) error {
	if s.storageSync == nil {
		return nil
	}
	return s.storageSync.EnsureFresh(ctx, repo)
}
