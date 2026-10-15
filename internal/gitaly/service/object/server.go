// Package object implements the ObjectService, one of the Graft extensions
// that move Gitaly toward the design of an agent-scale, server-centric VCS.
// See doc/graft/PLAN.md.
package object

import (
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedObjectServiceServer
	deps *service.Dependencies
}

// NewServer creates a new instance of the ObjectService server.
func NewServer(deps *service.Dependencies) gitalypb.ObjectServiceServer {
	return &server{deps: deps}
}
