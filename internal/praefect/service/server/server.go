package server

import (
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/praefect/config"
	"github.com/feder-cr/git-ai/internal/praefect/service"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// Server is a ServerService server
type Server struct {
	gitalypb.UnimplementedServerServiceServer
	conf   config.Config
	logger log.Logger
	conns  service.Connections
	checks []service.CheckFunc
}

// NewServer creates a new instance of a grpc ServerServiceServer
func NewServer(conf config.Config, logger log.Logger, conns service.Connections, checks []service.CheckFunc) gitalypb.ServerServiceServer {
	s := &Server{
		conf:   conf,
		logger: logger,
		conns:  conns,
		checks: checks,
	}

	return s
}
