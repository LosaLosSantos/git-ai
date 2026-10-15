// Package search implements the SearchService, one of the Graft extensions
// that move Gitaly toward the design of an agent-scale, server-centric VCS.
// See doc/graft/PLAN5.md.
package search

import (
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedSearchServiceServer
	deps *service.Dependencies
}

// NewServer creates a new instance of the SearchService server.
func NewServer(deps *service.Dependencies) gitalypb.SearchServiceServer {
	return &server{deps: deps}
}
