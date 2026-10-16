package server

import (
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
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
