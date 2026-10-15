package praefect

import (
	"github.com/feder-cr/open_scion/internal/grpc/protoregistry"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/praefect/config"
	"github.com/feder-cr/open_scion/internal/praefect/datastore"
	"github.com/feder-cr/open_scion/internal/praefect/nodes"
	"github.com/feder-cr/open_scion/internal/praefect/service"
	"github.com/feder-cr/open_scion/internal/praefect/transactions"
	"github.com/feder-cr/open_scion/proxy"
)

// Dependencies consolidates Praefect service dependencies for injection.
type Dependencies struct {
	Config          config.Config
	Logger          log.Logger
	Coordinator     *Coordinator
	Director        proxy.StreamDirector
	NodeMgr         nodes.Manager
	TxMgr           *transactions.Manager
	Queue           datastore.ReplicationEventQueue
	RepositoryStore datastore.RepositoryStore
	AssignmentStore AssignmentStore
	Router          Router
	Registry        *protoregistry.Registry
	Conns           Connections
	PrimaryGetter   PrimaryGetter
	Checks          []service.CheckFunc
}
