package ref

import (
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/gitaly/transaction"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedRefServiceServer
	logger           log.Logger
	txManager        transaction.Manager
	locator          storage.Locator
	catfileCache     catfile.Cache
	localRepoFactory localrepo.Factory
}

// NewServer creates a new instance of a grpc RefServer
func NewServer(deps *service.Dependencies) gitalypb.RefServiceServer {
	return &server{
		logger:           deps.GetLogger(),
		txManager:        deps.GetTxManager(),
		locator:          deps.GetLocator(),
		catfileCache:     deps.GetCatfileCache(),
		localRepoFactory: deps.GetRepositoryFactory(),
	}
}
