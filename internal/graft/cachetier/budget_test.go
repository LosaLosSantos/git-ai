package cachetier

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// noReadLimit lets a test read any object from the disk store.
const noReadLimit = math.MaxInt64

// inflightCount returns the object and tree flights that are still open.
func inflightCount(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inflight) + len(s.treeInflight)
}

// TestDiskStore_ReadHonoursTheLimit checks that the disk store refuses to load
// an object larger than the caller's limit, and that it keeps the file.
func TestDiskStore_ReadHonoursTheLimit(t *testing.T) {
	t.Parallel()

	store, err := newDiskStore(t.TempDir(), 1<<20)
	require.NoError(t, err)

	data := bytes.Repeat([]byte("x"), 80)
	key := objectKey{storage: "default", relativePath: "@hashed/limit.git", oid: gitObjectID("blob", data, false)}
	require.NoError(t, store.write(key, &object{typ: gitalypb.ObjectType_BLOB, data: data}))

	got, err := store.read(key, 79)
	require.ErrorIs(t, err, errObjectOverLimit)
	require.Nil(t, got)

	_, err = os.Stat(store.path(key))
	require.NoError(t, err, "an object over the limit is not corrupt and must stay on disk")

	got, err = store.read(key, 80)
	require.NoError(t, err)
	require.Equal(t, data, got.data)
}

// TestGetObjects_DiskHitsCountAgainstTheRequestBudget checks that objects read
// from the disk store count against the request's byte budget. Without that, a
// request naming many objects that only the disk store holds would buffer all of
// them at once, however large they are.
func TestGetObjects_DiskHitsCountAgainstTheRequestBudget(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/disk-budget.git")
	upstream := newFakeObjectService()

	var oids []string
	contents := make(map[string][]byte)
	for i := 0; i < 5; i++ {
		data := bytes.Repeat([]byte{byte('a' + i)}, 80)
		oid := upstream.put(repo, gitalypb.ObjectType_BLOB, data, false)
		oids = append(oids, oid)
		contents[oid] = data
	}

	// An 80-byte object costs more than MaxBytes in the LRU, so only the disk
	// store keeps the objects.
	tier := newTier(t, upstream, Config{MaxRequestBytes: 100, MaxBytes: 150, DiskDir: t.TempDir()})

	for _, oid := range oids {
		objects, _ := getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}})
		require.Equal(t, contents[oid], objects[oid].data)
	}
	require.Equal(t, int64(len(oids)), upstream.getObjectsCalls.Load())
	require.Zero(t, tier.server.Stats().MemoryEntries)

	// One object fits the budget and is served from disk.
	objects, err := tier.server.loadObjects(ctx, repo, oids[:1])
	require.NoError(t, err)
	require.Equal(t, contents[oids[0]], objects[oids[0]].data)
	require.Equal(t, int64(1), tier.server.Stats().DiskHits)
	require.Equal(t, int64(len(oids)), upstream.getObjectsCalls.Load())

	// All five objects are 400 bytes, over the budget of 100.
	_, err = tier.server.loadObjects(ctx, repo, oids)
	require.ErrorIs(t, err, errOverRequestBudget)
	require.Zero(t, inflightCount(tier.server), "every flight must be resolved when the budget is exceeded")

	// The RPC streams the request through the upstream instead.
	streamed, order := getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: oids})
	require.Equal(t, oids, order)
	for _, oid := range oids {
		require.Equal(t, contents[oid], streamed[oid].data)
	}
	require.Equal(t, int64(len(oids)+1), upstream.getObjectsCalls.Load(), "the request over budget must be forwarded")
}

// TestGetObjects_CoalescedObjectsCountAgainstTheRequestBudget checks that the
// objects a request takes over from other requests' fills count against its own
// budget. It keeps them alive after those requests finish.
func TestGetObjects_CoalescedObjectsCountAgainstTheRequestBudget(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/coalesced-budget.git")
	upstream := newFakeObjectService()

	first := bytes.Repeat([]byte("1"), 80)
	second := bytes.Repeat([]byte("2"), 80)
	firstOID := upstream.put(repo, gitalypb.ObjectType_BLOB, first, false)
	secondOID := upstream.put(repo, gitalypb.ObjectType_BLOB, second, false)
	upstream.gate = make(chan struct{})
	upstream.started = make(chan struct{}, 10)

	tier := newTier(t, upstream, Config{MaxRequestBytes: 100, MaxBytes: 150})

	owners := make(chan error, 2)
	for _, oid := range []string{firstOID, secondOID} {
		go func() {
			_, err := tier.server.loadObjects(ctx, repo, []string{oid})
			owners <- err
		}()
		<-upstream.started
	}

	waiter := make(chan error, 1)
	go func() {
		_, err := tier.server.loadObjects(ctx, repo, []string{firstOID, secondOID})
		waiter <- err
	}()
	require.Eventually(t, func() bool {
		return tier.server.Stats().Coalesced == 2
	}, 5*time.Second, time.Millisecond, "the waiter must join both fills")

	close(upstream.gate)
	require.NoError(t, <-owners)
	require.NoError(t, <-owners)
	require.ErrorIs(t, <-waiter, errOverRequestBudget, "two 80-byte objects exceed a budget of 100")

	objects, _ := getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{firstOID, secondOID}})
	require.Equal(t, first, objects[firstOID].data)
	require.Equal(t, second, objects[secondOID].data)
}

// gatedTreeService is an upstream whose GetTree sends a fixed listing in pages,
// optionally waiting on gate after the first page and on eofGate before EOF.
type gatedTreeService struct {
	gitalypb.UnimplementedObjectServiceServer

	entries  []*gitalypb.GetTreeResponse_Entry
	pageSize int
	// firstSent receives a value once the first page of a call has been sent.
	firstSent chan struct{}
	gate      chan struct{}
	eofGate   chan struct{}

	calls atomic.Int64
}

func (g *gatedTreeService) GetTree(req *gitalypb.GetTreeRequest, stream gitalypb.ObjectService_GetTreeServer) error {
	g.calls.Add(1)

	for start := 0; start < len(g.entries); start += g.pageSize {
		end := min(start+g.pageSize, len(g.entries))
		msg := &gitalypb.GetTreeResponse{Entries: g.entries[start:end]}
		if start == 0 {
			msg.TreeOid = req.GetTreeOid()
		}
		if err := stream.Send(msg); err != nil {
			return err
		}
		if start == 0 {
			if g.firstSent != nil {
				select {
				case g.firstSent <- struct{}{}:
				default:
				}
			}
			if g.gate != nil {
				<-g.gate
			}
		}
	}
	if g.eofGate != nil {
		<-g.eofGate
	}
	return nil
}

func newGatedTreeService(count, pageSize int) *gatedTreeService {
	entries := make([]*gitalypb.GetTreeResponse_Entry, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("dir/file-%04d", i)
		entries = append(entries, &gitalypb.GetTreeResponse_Entry{
			Path: []byte(name),
			Oid:  gitObjectID("blob", []byte(name), false),
			Type: gitalypb.ObjectType_BLOB,
			Mode: 0o100644,
		})
	}
	return &gatedTreeService{entries: entries, pageSize: pageSize, firstSent: make(chan struct{}, 10)}
}

func newTreeTier(t *testing.T, upstream *gatedTreeService, cfg Config) tierFixture {
	t.Helper()

	addr := startGRPC(t, func(srv *grpc.Server) {
		gitalypb.RegisterObjectServiceServer(srv, upstream)
		gitalypb.RegisterRepositoryServiceServer(srv, &fakeRepositoryService{})
	})
	return newTierOver(t, addr, cfg)
}

// readTree drains a GetTree stream.
func readTree(stream grpc.ServerStreamingClient[gitalypb.GetTreeResponse]) ([]*gitalypb.GetTreeResponse_Entry, []*gitalypb.GetTreeResponse, error) {
	var entries []*gitalypb.GetTreeResponse_Entry
	var messages []*gitalypb.GetTreeResponse
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return entries, messages, nil
		}
		if err != nil {
			return nil, nil, err
		}
		messages = append(messages, msg)
		entries = append(entries, msg.GetEntries()...)
	}
}

// Each test entry costs about 180 bytes, so with a MaxBytes of 4096 three pages
// of ten entries exceed the tree budget, and two do not.
const treeBudgetTestMaxBytes = 4096

// TestGetTree_LargeListingIsStreamedNotBuffered checks that a listing larger
// than the tree budget reaches the client before the upstream has finished it,
// so that the tier never buffers a whole large listing, and that it is not cached.
func TestGetTree_LargeListingIsStreamedNotBuffered(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/large-tree.git")
	upstream := newGatedTreeService(50, 10)
	upstream.eofGate = make(chan struct{})
	tier := newTreeTier(t, upstream, Config{MaxBytes: treeBudgetTestMaxBytes, TreePageSize: 10})

	treeOID := gitObjectID("tree", []byte("large"), false)
	req := &gitalypb.GetTreeRequest{Repository: repo, TreeOid: treeOID, Recursive: true}

	stream, err := tier.client.GetTree(ctx, req)
	require.NoError(t, err)

	type recvResult struct {
		msg *gitalypb.GetTreeResponse
		err error
	}
	firstMsg := make(chan recvResult, 1)
	go func() {
		msg, err := stream.Recv()
		firstMsg <- recvResult{msg: msg, err: err}
	}()

	var first recvResult
	select {
	case first = <-firstMsg:
	case <-time.After(30 * time.Second):
		close(upstream.eofGate)
		require.FailNow(t, "the client got nothing while the upstream was still listing: the tier buffers the whole listing")
	}
	require.NoError(t, first.err)
	require.Equal(t, treeOID, first.msg.GetTreeOid())
	require.NotEmpty(t, first.msg.GetEntries())

	close(upstream.eofGate)
	rest, messages, err := readTree(stream)
	require.NoError(t, err)
	for _, msg := range messages {
		require.Empty(t, msg.GetTreeOid(), "tree_oid is only set on the first message")
	}
	requireEntriesEqual(t, upstream.entries, append(first.msg.GetEntries(), rest...))

	require.Zero(t, tier.server.Stats().MemoryEntries, "a listing over the tree budget must not be cached")
	require.Zero(t, inflightCount(tier.server))

	// The listing is not cached, so the next request streams it again.
	stream, err = tier.client.GetTree(ctx, req)
	require.NoError(t, err)
	got, _, err := readTree(stream)
	require.NoError(t, err)
	requireEntriesEqual(t, upstream.entries, got)
	require.Equal(t, int64(2), upstream.calls.Load())
}

// TestGetTree_CoalescedRequests checks that requests for a listing that is being
// filled wait for the fill. A listing within the budget is fetched once and
// shared. A waiter on a listing over the budget streams it from the upstream
// itself, and a cancelled waiter reports its own cancellation.
func TestGetTree_CoalescedRequests(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	for _, tc := range []struct {
		desc      string
		count     int
		wantCalls int64
		cached    bool
	}{
		{desc: "listing within the budget is shared", count: 15, wantCalls: 1, cached: true},
		{desc: "listing over the budget is streamed by each request", count: 50, wantCalls: 2},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			repo := testRepo("@hashed/coalesced-tree.git")
			upstream := newGatedTreeService(tc.count, 10)
			upstream.gate = make(chan struct{})
			tier := newTreeTier(t, upstream, Config{MaxBytes: treeBudgetTestMaxBytes, TreePageSize: 10})

			req := &gitalypb.GetTreeRequest{Repository: repo, TreeOid: gitObjectID("tree", []byte(tc.desc), false), Recursive: true}

			type listing struct {
				entries []*gitalypb.GetTreeResponse_Entry
				err     error
			}
			list := func() listing {
				stream, err := tier.client.GetTree(ctx, req)
				if err != nil {
					return listing{err: err}
				}
				entries, _, err := readTree(stream)
				return listing{entries: entries, err: err}
			}

			owner := make(chan listing, 1)
			go func() { owner <- list() }()
			<-upstream.firstSent

			waiter := make(chan listing, 1)
			go func() { waiter <- list() }()
			require.Eventually(t, func() bool {
				return tier.server.Stats().TreeMisses == 2
			}, 5*time.Second, time.Millisecond, "the second request must wait for the first one's fill")

			close(upstream.gate)
			for _, ch := range []chan listing{owner, waiter} {
				got := <-ch
				require.NoError(t, got.err)
				requireEntriesEqual(t, upstream.entries, got.entries)
			}

			require.Equal(t, tc.wantCalls, upstream.calls.Load())
			require.Equal(t, tc.cached, tier.server.Stats().MemoryEntries == 1)
			require.Zero(t, inflightCount(tier.server))
		})
	}

	t.Run("cancelled waiter", func(t *testing.T) {
		t.Parallel()

		repo := testRepo("@hashed/cancelled-tree.git")
		upstream := newGatedTreeService(15, 10)
		upstream.gate = make(chan struct{})
		tier := newTreeTier(t, upstream, Config{MaxBytes: treeBudgetTestMaxBytes, TreePageSize: 10})

		req := &gitalypb.GetTreeRequest{Repository: repo, TreeOid: gitObjectID("tree", []byte("cancelled"), false)}

		owner := make(chan error, 1)
		go func() {
			stream, err := tier.client.GetTree(ctx, req)
			if err == nil {
				_, _, err = readTree(stream)
			}
			owner <- err
		}()
		<-upstream.firstSent

		waiterCtx, cancel := context.WithCancel(ctx)
		waiter := make(chan error, 1)
		go func() {
			stream, err := tier.client.GetTree(waiterCtx, req)
			if err == nil {
				_, _, err = readTree(stream)
			}
			waiter <- err
		}()
		require.Eventually(t, func() bool {
			return tier.server.Stats().TreeMisses == 2
		}, 5*time.Second, time.Millisecond)

		cancel()
		require.Equal(t, codes.Canceled, status.Code(<-waiter))

		close(upstream.gate)
		require.NoError(t, <-owner)
		require.Equal(t, int64(1), upstream.calls.Load())
	})
}
