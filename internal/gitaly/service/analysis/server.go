package analysis

import (
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedAnalysisServiceServer
	logger           log.Logger
	locator          storage.Locator
	catfileCache     catfile.Cache
	localRepoFactory localrepo.Factory
}

// NewServer creates a new instance of the gRPC AnalysisService.
func NewServer(deps *service.Dependencies) gitalypb.AnalysisServiceServer {
	return &server{
		logger:           deps.GetLogger(),
		locator:          deps.GetLocator(),
		catfileCache:     deps.GetCatfileCache(),
		localRepoFactory: deps.GetRepositoryFactory(),
	}
}
