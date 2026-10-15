package internalgitaly

import (
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/gitaly/storage/relational"
	"github.com/feder-cr/open_scion/internal/gitlab"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedInternalGitalyServer
	logger       log.Logger
	storages     []config.Storage
	locator      storage.Locator
	poolStore    relational.PoolStore
	gitlabClient gitlab.Client
}

// NewServer return an instance of the Gitaly service.
func NewServer(deps *service.Dependencies) gitalypb.InternalGitalyServer {
	return &server{
		logger:       deps.GetLogger(),
		storages:     deps.GetCfg().Storages,
		locator:      deps.GetLocator(),
		poolStore:    deps.GetPoolMetadataStore(),
		gitlabClient: deps.GetGitlabClient(),
	}
}
