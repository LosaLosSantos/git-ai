package internalgitaly

import (
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/relational"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
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
