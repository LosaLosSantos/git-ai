package server

import (
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedServerServiceServer
	logger            log.Logger
	gitCmdFactory     gitcmd.CommandFactory
	storages          []config.Storage
	gitSigningKeyPath string
}

// NewServer creates a new instance of a grpc ServerServiceServer
func NewServer(deps *service.Dependencies) gitalypb.ServerServiceServer {
	return &server{
		logger:            deps.GetLogger(),
		gitCmdFactory:     deps.GetGitCmdFactory(),
		storages:          deps.GetCfg().Storages,
		gitSigningKeyPath: deps.GetCfg().Git.SigningKey,
	}
}
