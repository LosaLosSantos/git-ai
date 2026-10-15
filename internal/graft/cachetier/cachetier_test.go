package cachetier

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/proxy"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// testChunk is the message size used by the fake upstream, small enough that
// most test objects span several messages.
const testChunk = 16

// gitObjectID computes the Git object ID of data for the given type name.
func gitObjectID(typ string, data []byte, sha256Format bool) string {
	header := fmt.Sprintf("%s %d\x00", typ, len(data))
	if sha256Format {
		h := sha256.New()
		h.Write([]byte(header))
		h.Write(data)
		return hex.EncodeToString(h.Sum(nil))
	}
	h := sha1.New()
	h.Write([]byte(header))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func testRepo(relativePath string) *gitalypb.Repository {
	return &gitalypb.Repository{StorageName: "default", RelativePath: relativePath}
}

func repoKey(repo *gitalypb.Repository) string {
	return repo.GetStorageName() + "/" + repo.GetRelativePath()
}

// fakeObjectService is an upstream ObjectService that serves objects it was
// given with put, and counts how often each RPC is invoked.
type fakeObjectService struct {
	gitalypb.UnimplementedObjectServiceServer

	mu      sync.Mutex
	objects map[string]map[string]*object
	corrupt map[string]bool
	trees   map[string][]*gitalypb.GetTreeResponse_Entry

	// gate, if set, blocks GetObjects until it is closed.
	gate chan struct{}
	// started, if set, receives a value whenever GetObjects is entered.
	started chan struct{}
	// reject, if set, makes GetObjects fail with InvalidArgument on the first
	// object ID it rejects, after sending the objects before it.
	reject func(oid string) bool

	getObjectsCalls atomic.Int64
	getTreeCalls    atomic.Int64

	// chunkSize, if set, replaces testChunk as the size of the data messages.
	chunkSize int
}

func newFakeObjectService() *fakeObjectService {
	return &fakeObjectService{
		objects: make(map[string]map[string]*object),
		corrupt: make(map[string]bool),
		trees:   make(map[string][]*gitalypb.GetTreeResponse_Entry),
	}
}

// put stores data as a Git object of the given type in repo and returns its ID.
func (f *fakeObjectService) put(repo *gitalypb.Repository, typ gitalypb.ObjectType, data []byte, sha256Format bool) string {
	name, ok := typeName(typ)
	if !ok {
		panic("unsupported type")
	}
	oid := gitObjectID(name, data, sha256Format)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objects[repoKey(repo)] == nil {
		f.objects[repoKey(repo)] = make(map[string]*object)
	}
	f.objects[repoKey(repo)][oid] = &object{typ: typ, data: append([]byte(nil), data...)}
	return oid
}

func (f *fakeObjectService) corruptOID(oid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.corrupt[oid] = true
}

func (f *fakeObjectService) lookup(repo *gitalypb.Repository, oid string) (*object, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[repoKey(repo)][oid]
	return obj, ok
}

func (f *fakeObjectService) GetObjects(req *gitalypb.GetObjectsRequest, stream gitalypb.ObjectService_GetObjectsServer) error {
	f.getObjectsCalls.Add(1)
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	if f.gate != nil {
		<-f.gate
	}

	for _, oid := range req.GetOids() {
		if f.reject != nil && f.reject(oid) {
			return status.Errorf(codes.InvalidArgument, "invalid object ID %q", oid)
		}

		obj, ok := f.lookup(req.GetRepository(), oid)
		if !ok {
			if err := stream.Send(&gitalypb.GetObjectsResponse{Oid: oid, Missing: true}); err != nil {
				return err
			}
			continue
		}

		data := obj.data
		f.mu.Lock()
		corrupt := f.corrupt[oid]
		f.mu.Unlock()
		if corrupt && len(data) > 0 {
			data = append([]byte(nil), data...)
			data[0] ^= 0xff
		}

		for off := 0; ; {
			chunk := testChunk
			if f.chunkSize > 0 {
				chunk = f.chunkSize
			}
			end := min(off+chunk, len(data))
			msg := &gitalypb.GetObjectsResponse{Oid: oid, Offset: int64(off), Data: data[off:end]}
			if off == 0 {
				msg.Type = obj.typ
				msg.Size = int64(len(data))
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
			off = end
			if off >= len(data) {
				break
			}
		}
	}
	return nil
}

// putTree registers the listing returned for key. key is a tree ID for
// tree_oid requests, or "rev:<revision>" for revision requests.
func (f *fakeObjectService) putTree(key string, entries []*gitalypb.GetTreeResponse_Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trees[key] = entries
}

func (f *fakeObjectService) GetTree(req *gitalypb.GetTreeRequest, stream gitalypb.ObjectService_GetTreeServer) error {
	f.getTreeCalls.Add(1)

	key := req.GetTreeOid()
	if key == "" {
		key = "rev:" + string(req.GetRevision())
	}

	f.mu.Lock()
	entries, ok := f.trees[key]
	f.mu.Unlock()
	if !ok {
		return status.Error(codes.NotFound, "tree not found")
	}

	const page = 2
	if len(entries) == 0 {
		return stream.Send(&gitalypb.GetTreeResponse{TreeOid: req.GetTreeOid()})
	}
	for start := 0; start < len(entries); start += page {
		end := min(start+page, len(entries))
		msg := &gitalypb.GetTreeResponse{Entries: entries[start:end]}
		if start == 0 {
			msg.TreeOid = req.GetTreeOid()
		}
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
	return nil
}

type fakeRepositoryService struct {
	gitalypb.UnimplementedRepositoryServiceServer
	calls atomic.Int64
}

func (f *fakeRepositoryService) RepositoryExists(context.Context, *gitalypb.RepositoryExistsRequest) (*gitalypb.RepositoryExistsResponse, error) {
	f.calls.Add(1)
	return &gitalypb.RepositoryExistsResponse{Exists: true}, nil
}

// fakeOperationService counts the frames of client-streaming UserCommitFiles calls.
type fakeOperationService struct {
	gitalypb.UnimplementedOperationServiceServer
	frames atomic.Int64
}

func (f *fakeOperationService) UserCommitFiles(stream gitalypb.OperationService_UserCommitFilesServer) error {
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		f.frames.Add(1)
	}
	return stream.SendAndClose(&gitalypb.UserCommitFilesResponse{})
}

// startGRPC serves the services registered by register on a local TCP port and
// returns its address. The server is stopped when the test ends.
func startGRPC(t *testing.T, register func(*grpc.Server)) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := grpc.NewServer()
	register(srv)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(srv.Stop)

	return listener.Addr().String()
}

func dialAddr(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// dialUpstream dials a cache tier's upstream the way DialUpstream does in
// production: with the proxy codec forced as the default call option.
func dialUpstream(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient("passthrough:///"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(proxy.NewCodec())),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// startCacheTier starts a cache tier that proxies to upstreamAddr and returns
// its address together with the server for inspection.
func startCacheTier(t *testing.T, upstreamAddr string, cfg Config) (string, *Server) {
	t.Helper()

	cfg.Logger = testLogger(t)
	server, err := NewServer(dialUpstream(t, upstreamAddr), cfg)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := NewGRPCServer(server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	return listener.Addr().String(), server
}

// testLog is configured once in TestMain. log.Configure replaces grpc's global
// logger, so calling it from parallel tests would race with running servers.
var testLog log.Logger

func TestMain(m *testing.M) {
	logger, err := log.Configure(io.Discard, "text", "error")
	if err != nil {
		panic(err)
	}
	testLog = logger
	testhelper.Run(m)
}

func testLogger(t *testing.T) log.Logger {
	t.Helper()
	return testLog
}

// received is one object as reassembled from GetObjects messages.
type received struct {
	typ      gitalypb.ObjectType
	size     int64
	startOff int64
	data     []byte
	missing  bool
	messages int
	maxChunk int
}

// readObjects drains a GetObjects stream and reassembles each object. It checks
// that every object's chunks are contiguous.
func readObjects(t *testing.T, stream grpc.ServerStreamingClient[gitalypb.GetObjectsResponse]) (map[string]*received, []string) {
	t.Helper()

	objects := make(map[string]*received)
	var order []string
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		r, ok := objects[msg.GetOid()]
		if !ok {
			r = &received{}
			objects[msg.GetOid()] = r
			order = append(order, msg.GetOid())
		}
		r.messages++

		if msg.GetMissing() {
			r.missing = true
			continue
		}

		if r.messages == 1 {
			r.typ = msg.GetType()
			r.size = msg.GetSize()
			r.startOff = msg.GetOffset()
		}
		require.Equal(t, r.startOff+int64(len(r.data)), msg.GetOffset(),
			"chunks of %s must be contiguous", msg.GetOid())
		if n := len(msg.GetData()); n > r.maxChunk {
			r.maxChunk = n
		}
		r.data = append(r.data, msg.GetData()...)
	}
	return objects, order
}

func newObjectClient(t *testing.T, addr string) gitalypb.ObjectServiceClient {
	t.Helper()
	return gitalypb.NewObjectServiceClient(dialAddr(t, addr))
}
