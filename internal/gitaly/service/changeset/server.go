// Package changeset implements the ChangesetService, one of the Graft
// extensions that move Gitaly toward the design of an agent-scale,
// server-centric VCS. See doc/graft/PLAN.md.
package changeset

import (
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The changeset log is per node: behind Praefect, both ListChangesets and
// WatchChangesets are accessors that may be routed to a replica, which keeps its
// own log and sequences. Clients that need a complete history must read from
// the node that records the updates.
type server struct {
	gitalypb.UnimplementedChangesetServiceServer
	deps *service.Dependencies
}

// NewServer creates a new instance of the ChangesetService server.
func NewServer(deps *service.Dependencies) gitalypb.ChangesetServiceServer {
	return &server{deps: deps}
}
