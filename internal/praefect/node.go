package praefect

import (
	"context"
	"fmt"

	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/internal/grpc/sidechannel"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/praefect/config"
	"github.com/feder-cr/git-ai/internal/praefect/nodes"
	"github.com/feder-cr/git-ai/internal/praefect/nodes/tracker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// Node is a storage node in a virtual storage.
type Node struct {
	// Storage is the name of the storage node.
	Storage string
	// Address is the address of the node.
	Address string
	// Token is the authentication token of the node.
	Token string
	// Connection is a gRPC connection to the storage node.
	Connection *grpc.ClientConn
	// HealthCheckConn is a dedicated gRPC connection solely
	// for the purpose of health checks. By giving it its dedicated
	// connection, health checks are not affected if the single
	// Connection above reached its MaxConcurrentStreams.
	HealthCheckConn *grpc.ClientConn
}

// NodeSet contains nodes by their virtual storage and storage names.
type NodeSet map[string]map[string]Node

// Close closes the connections in the NodeSet. Errors on closing are ignored.
func (set NodeSet) Close() {
	for _, nodes := range set {
		for _, node := range nodes {
			if node.Connection != nil {
				_ = node.Connection.Close()
			}
			// There are other places where a NodeSet is created
			// which does not create a health check connection.
			// One example is Praefect's `subcmd_list_untracked_repositories`.
			// In some cases, such as Praefect's sub-commands, having a
			// dedicated health check connection is overkill, as the process
			// it short-lived. So here we guard against nil connections.
			if node.HealthCheckConn != nil {
				_ = node.HealthCheckConn.Close()
			}
		}
	}
}

// HealthClients is a convenience method to return the HealthClients from the NodeSet.
func (set NodeSet) HealthClients() nodes.HealthClients {
	clients := make(nodes.HealthClients, len(set))
	for virtualStorage, nodes := range set {
		clients[virtualStorage] = make(map[string]grpc_health_v1.HealthClient, len(nodes))
		for _, node := range nodes {
			clients[virtualStorage][node.Storage] = grpc_health_v1.NewHealthClient(node.HealthCheckConn)
		}
	}

	return clients
}

// Connections is a convenience method to return the connections from the NodeSet.
func (set NodeSet) Connections() Connections {
	conns := make(Connections, len(set))
	for virtualStorage, nodes := range set {
		conns[virtualStorage] = make(map[string]*grpc.ClientConn, len(nodes))
		for _, node := range nodes {
			conns[virtualStorage][node.Storage] = node.Connection
		}
	}

	return conns
}

// NodeSetFromNodeManager converts connections set up by the node manager
// in to a NodeSet. This is a temporary adapter required due to cyclic
// imports between the praefect and nodes packages.
func NodeSetFromNodeManager(mgr nodes.Manager) NodeSet {
	nodes := mgr.Nodes()

	set := make(NodeSet, len(nodes))
	for virtualStorage, nodes := range nodes {
		set[virtualStorage] = make(map[string]Node, len(nodes))
		for _, node := range nodes {
			set[virtualStorage][node.GetStorage()] = toNode(node)
		}
	}

	return set
}

func toNode(node nodes.Node) Node {
	return Node{
		Storage:    node.GetStorage(),
		Address:    node.GetAddress(),
		Token:      node.GetToken(),
		Connection: node.GetConnection(),
	}
}

// DialNodes dials the configured storage nodes.
func DialNodes(
	ctx context.Context,
	virtualStorages []*config.VirtualStorage,
	registry *protoregistry.Registry,
	errorTracker tracker.ErrorTracker,
	handshaker client.Handshaker,
	sidechannelRegistry *sidechannel.Registry,
	log log.Logger,
) (NodeSet, error) {
	set := make(NodeSet, len(virtualStorages))
	for _, virtualStorage := range virtualStorages {
		set[virtualStorage.Name] = make(map[string]Node, len(virtualStorage.Nodes))
		for _, node := range virtualStorage.Nodes {
			rpcConn, err := nodes.Dial(ctx, node, registry, errorTracker, handshaker, sidechannelRegistry, log)
			if err != nil {
				return nil, fmt.Errorf("dial %q/%q: %w", virtualStorage.Name, node.Storage, err)
			}

			hcConn, err := nodes.DialHealthCheck(ctx, node)
			if err != nil {
				_ = rpcConn.Close()
				return nil, fmt.Errorf("dial %q/%q: %w", virtualStorage.Name, node.Storage, err)
			}

			set[virtualStorage.Name][node.Storage] = Node{
				Storage:         node.Storage,
				Address:         node.Address,
				Token:           node.Token,
				Connection:      rpcConn,
				HealthCheckConn: hcConn,
			}
		}
	}

	return set, nil
}
