package sdk

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"google.golang.org/grpc"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/client"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// DefaultStorage is the storage name used when WithStorage is not given.
const DefaultStorage = "default"

// DialOption configures Dial.
type DialOption func(*dialConfig)

type dialConfig struct {
	token           string
	agentToken      string
	storage         string
	objectBatchSize int
	grpcOpts        []grpc.DialOption
}

// WithObjectBatchSize sets the maximum number of object IDs that GetObjects
// sends in one request. Use it to match a server whose object_service
// max_objects is known to be low. Without it, GetObjects starts at 500 and
// lowers the size to the server's limit when a request is refused for having
// too many IDs. Values below 1 are ignored.
func WithObjectBatchSize(n int) DialOption {
	return func(c *dialConfig) { c.objectBatchSize = n }
}

// WithToken authenticates every request with the server's shared token
// (the auth.token setting), using the HMAC v2 token format.
func WithToken(token string) DialOption {
	return func(c *dialConfig) { c.token = token }
}

// WithAgentToken authenticates every request with a per-agent token
// (a [[graft.agent]] entry), sent as a plain bearer token.
func WithAgentToken(token string) DialOption {
	return func(c *dialConfig) { c.agentToken = token }
}

// WithStorage selects the storage that Client.Repo addresses. Defaults to
// DefaultStorage.
func WithStorage(storage string) DialOption {
	return func(c *dialConfig) { c.storage = storage }
}

// WithGRPCOptions adds raw gRPC dial options, for example transport
// credentials or interceptors.
func WithGRPCOptions(opts ...grpc.DialOption) DialOption {
	return func(c *dialConfig) { c.grpcOpts = append(c.grpcOpts, opts...) }
}

// Client is a connection to a Graft or Gitaly server. It is safe for
// concurrent use. Close it when it is no longer needed.
type Client struct {
	conn    *grpc.ClientConn
	storage string
	// objectBatch is the current number of object IDs per GetObjects request.
	// It only decreases, when the server refuses a request as too large.
	objectBatch atomic.Int64

	objects    gitalypb.ObjectServiceClient
	changesets gitalypb.ChangesetServiceClient
	derived    gitalypb.DerivedDataServiceClient
	operations gitalypb.OperationServiceClient
	commits    gitalypb.CommitServiceClient
}

// Dial connects to the server at address. Valid addresses are the ones
// accepted by the Gitaly client: "unix:<path>", "tcp://<host:port>" and
// "tls://<host:port>". Dial does not wait for the connection to be ready;
// the first RPC does.
func Dial(ctx context.Context, address string, opts ...DialOption) (*Client, error) {
	var cfg dialConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.token != "" && cfg.agentToken != "" {
		return nil, errors.New("sdk: WithToken and WithAgentToken are mutually exclusive")
	}
	if cfg.storage == "" {
		cfg.storage = DefaultStorage
	}

	grpcOpts := append([]grpc.DialOption(nil), cfg.grpcOpts...)
	switch {
	case cfg.token != "":
		grpcOpts = append(grpcOpts, grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(cfg.token)))
	case cfg.agentToken != "":
		grpcOpts = append(grpcOpts, grpc.WithPerRPCCredentials(bearerCredentials{token: cfg.agentToken}))
	}

	conn, err := client.DialContext(ctx, address, client.WithGrpcOptions(grpcOpts))
	if err != nil {
		return nil, fmt.Errorf("sdk: dial %q: %w", address, err)
	}

	c := &Client{
		conn:       conn,
		storage:    cfg.storage,
		objects:    gitalypb.NewObjectServiceClient(conn),
		changesets: gitalypb.NewChangesetServiceClient(conn),
		derived:    gitalypb.NewDerivedDataServiceClient(conn),
		operations: gitalypb.NewOperationServiceClient(conn),
		commits:    gitalypb.NewCommitServiceClient(conn),
	}
	batch := objectBatchSize
	if cfg.objectBatchSize > 0 {
		batch = cfg.objectBatchSize
	}
	c.objectBatch.Store(int64(batch))
	return c, nil
}

// objectBatchSize returns the number of object IDs to send per GetObjects
// request.
func (c *Client) objectBatchSize() int {
	if n := c.objectBatch.Load(); n > 0 {
		return int(n)
	}
	return objectBatchSize
}

// lowerObjectBatchSize lowers the GetObjects batch size to at most n. The size
// is shared by every Repo of the client, because the limit is the server's.
func (c *Client) lowerObjectBatchSize(n int) {
	n = max(n, 1)
	for {
		current := c.objectBatch.Load()
		if (current > 0 && current <= int64(n)) || c.objectBatch.CompareAndSwap(current, int64(n)) {
			return
		}
	}
}

// Close releases the connection. Repo handles obtained from the client must
// not be used afterwards.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Storage returns the storage name that Repo handles address.
func (c *Client) Storage() string {
	return c.storage
}

// Repo returns a handle to the repository at relativePath within the
// client's storage. Creating a handle does not contact the server.
func (c *Client) Repo(relativePath string) *Repo {
	return &Repo{client: c, relativePath: relativePath}
}

// bearerCredentials sends a plain bearer token, as used for agent tokens.
type bearerCredentials struct {
	token string
}

func (bearerCredentials) RequireTransportSecurity() bool { return false }

func (c bearerCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + c.token}, nil
}
