package conflicts

import (
	"context"

	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/git/quarantine"
	"github.com/feder-cr/open_scion/internal/gitaly/hook"
	"github.com/feder-cr/open_scion/internal/gitaly/hook/updateref"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedConflictsServiceServer
	logger           log.Logger
	locator          storage.Locator
	catfileCache     catfile.Cache
	pool             *client.Pool
	hookManager      hook.Manager
	updater          *updateref.UpdaterWithHooks
	localRepoFactory localrepo.Factory
}

// NewServer creates a new instance of a grpc ConflictsServer
func NewServer(deps *service.Dependencies) gitalypb.ConflictsServiceServer {
	return &server{
		logger:           deps.GetLogger(),
		hookManager:      deps.GetHookManager(),
		locator:          deps.GetLocator(),
		catfileCache:     deps.GetCatfileCache(),
		pool:             deps.GetConnsPool(),
		updater:          deps.GetUpdaterWithHooks(),
		localRepoFactory: deps.GetRepositoryFactory(),
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
