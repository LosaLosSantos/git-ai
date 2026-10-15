package cachetier

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type tierFixture struct {
	client gitalypb.ObjectServiceClient
	server *Server
	addr   string
}

// newTier starts a cache tier in front of a fresh upstream built from upstream.
func newTier(t *testing.T, upstream *fakeObjectService, cfg Config) tierFixture {
	t.Helper()

	upstreamAddr := startGRPC(t, func(srv *grpc.Server) {
		gitalypb.RegisterObjectServiceServer(srv, upstream)
		// The tier authorizes every object read with RepositoryExists.
		gitalypb.RegisterRepositoryServiceServer(srv, &fakeRepositoryService{})
	})
	return newTierOver(t, upstreamAddr, cfg)
}

// requireEntriesEqual compares listings by message content. proto.Equal is used
// because marshalling sets internal size caches that require.Equal would see.
func requireEntriesEqual(t *testing.T, want, got []*gitalypb.GetTreeResponse_Entry) {
	t.Helper()

	require.Len(t, got, len(want))
	for i := range want {
		require.True(t, proto.Equal(want[i], got[i]), "entry %d differs: want %v, got %v", i, want[i], got[i])
	}
}

func newTierOver(t *testing.T, upstreamAddr string, cfg Config) tierFixture {
	t.Helper()

	if cfg.Tier == "" {
		cfg.Tier = TierCache
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = 1 << 20
	}
	if cfg.ChunkSize == 0 {
		cfg.ChunkSize = testChunk
	}

	addr, server := startCacheTier(t, upstreamAddr, cfg)
	return tierFixture{client: newObjectClient(t, addr), server: server, addr: addr}
}

func getObjects(t *testing.T, ctx context.Context, client gitalypb.ObjectServiceClient, req *gitalypb.GetObjectsRequest) (map[string]*received, []string) {
	t.Helper()

	stream, err := client.GetObjects(ctx, req)
	require.NoError(t, err)
	return readObjects(t, stream)
}

// objectsErr drains a GetObjects stream and returns the first error it reports.
func objectsErr(ctx context.Context, client gitalypb.ObjectServiceClient, req *gitalypb.GetObjectsRequest) error {
	stream, err := client.GetObjects(ctx, req)
	if err != nil {
		return err
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// The tests in this file use a fake upstream. integration_test.go covers the
// same RPCs against a real Gitaly started with testserver.

func TestGetObjects_ServesChunksAndCachesInMemory(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/a.git")
	upstream := newFakeObjectService()

	blob := []byte("0123456789abcdefghijklmnopqrstuvwxyz") // 36 bytes, three chunks of 16
	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, blob, false)
	tier := newTier(t, upstream, Config{})

	req := &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}}

	objects, order := getObjects(t, ctx, tier.client, req)
	require.Equal(t, []string{oid}, order)
	require.Equal(t, blob, objects[oid].data)
	require.Equal(t, gitalypb.ObjectType_BLOB, objects[oid].typ)
	require.Equal(t, int64(len(blob)), objects[oid].size)
	require.Equal(t, 3, objects[oid].messages)
	require.LessOrEqual(t, objects[oid].maxChunk, testChunk)
	require.Equal(t, int64(1), upstream.getObjectsCalls.Load())

	objects, _ = getObjects(t, ctx, tier.client, req)
	require.Equal(t, blob, objects[oid].data)
	require.Equal(t, int64(1), upstream.getObjectsCalls.Load(), "second read must be served from memory")

	stats := tier.server.Stats()
	require.Equal(t, int64(1), stats.ObjectMisses)
	require.Equal(t, int64(1), stats.ObjectHits)
}

func TestGetObjects_MissingDuplicatesAndOrder(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/b.git")
	upstream := newFakeObjectService()

	a := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("first object"), false)
	b := upstream.put(repo, gitalypb.ObjectType_COMMIT, []byte("tree 0000\n\nsecond"), false)
	absent := gitObjectID("blob", []byte("never stored"), false)
	tier := newTier(t, upstream, Config{})

	objects, order := getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{
		Repository: repo,
		Oids:       []string{a, absent, a, b},
	})

	require.Equal(t, []string{a, absent, b}, order, "duplicates are returned once, in request order")
	require.True(t, objects[absent].missing)
	require.Equal(t, []byte("first object"), objects[a].data)
	require.Equal(t, gitalypb.ObjectType_COMMIT, objects[b].typ)
	require.Equal(t, []byte("tree 0000\n\nsecond"), objects[b].data)
}

func TestGetObjects_ResumeOffset(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/c.git")
	upstream := newFakeObjectService()

	blob := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, blob, false)
	tier := newTier(t, upstream, Config{})

	t.Run("resumes mid object", func(t *testing.T) {
		objects, _ := getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{
			Repository:   repo,
			Oids:         []string{oid},
			ResumeOid:    oid,
			ResumeOffset: 20,
		})
		require.Equal(t, int64(20), objects[oid].startOff)
		require.Equal(t, blob[20:], objects[oid].data)
		require.Equal(t, int64(len(blob)), objects[oid].size, "the header still describes the whole object")
	})

	t.Run("resuming at the end yields no data", func(t *testing.T) {
		objects, _ := getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{
			Repository:   repo,
			Oids:         []string{oid},
			ResumeOid:    oid,
			ResumeOffset: int64(len(blob)),
		})
		require.Empty(t, objects[oid].data)
	})

	t.Run("offset beyond the object is rejected", func(t *testing.T) {
		err := objectsErr(ctx, tier.client, &gitalypb.GetObjectsRequest{
			Repository:   repo,
			Oids:         []string{oid},
			ResumeOid:    oid,
			ResumeOffset: int64(len(blob)) + 1,
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})
}

func TestGetObjects_InvalidRequests(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/d.git")
	upstream := newFakeObjectService()
	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("x"), false)
	tier := newTier(t, upstream, Config{})

	for name, req := range map[string]*gitalypb.GetObjectsRequest{
		"no repository":        {Oids: []string{oid}},
		"no relative path":     {Repository: &gitalypb.Repository{StorageName: "default"}, Oids: []string{oid}},
		"malformed oid":        {Repository: repo, Oids: []string{"not-an-object-id"}},
		"uppercase oid":        {Repository: repo, Oids: []string{"ABCDEF" + oid[6:]}},
		"resume not requested": {Repository: repo, Oids: []string{oid}, ResumeOid: gitObjectID("blob", []byte("y"), false)},
		"negative offset":      {Repository: repo, Oids: []string{oid}, ResumeOid: oid, ResumeOffset: -1},
	} {
		t.Run(name, func(t *testing.T) {
			err := objectsErr(ctx, tier.client, req)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
		})
	}

	t.Run("empty request is rejected like the upstream", func(t *testing.T) {
		err := objectsErr(ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	})

	t.Run("resume offset without resume oid is rejected", func(t *testing.T) {
		err := objectsErr(ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}, ResumeOffset: 1})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	})
}

func TestGetObjects_MaxObjects(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/limit.git")
	upstream := newFakeObjectService()
	first := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("first"), false)
	second := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("second"), false)
	tier := newTier(t, upstream, Config{MaxObjects: 2})

	t.Run("limit counts requested IDs before deduplication", func(t *testing.T) {
		err := objectsErr(ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{first, first, second}})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	})

	t.Run("requests at the limit succeed with one upstream call", func(t *testing.T) {
		upstream.getObjectsCalls.Store(0)

		objects, _ := getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{first, second}})
		require.Equal(t, []byte("first"), objects[first].data)
		require.Equal(t, []byte("second"), objects[second].data)
		require.Equal(t, int64(1), upstream.getObjectsCalls.Load())
	})
}

func TestGetObjects_SHA256Repositories(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/sha256.git")
	upstream := newFakeObjectService()

	data := []byte("an object in a sha256 repository, longer than one chunk")
	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, data, true)
	require.Len(t, oid, sha256HexLen)
	tier := newTier(t, upstream, Config{})

	objects, _ := getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}})
	require.Equal(t, data, objects[oid].data)
	require.Equal(t, int64(0), tier.server.Stats().VerifyFailures)
}

func TestGetObjects_CoalescesConcurrentMisses(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/e.git")
	upstream := newFakeObjectService()

	blob := []byte("a popular object that many clients ask for at once")
	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, blob, false)
	upstream.gate = make(chan struct{})
	upstream.started = make(chan struct{}, 100)
	tier := newTier(t, upstream, Config{})

	const clients = 10
	req := &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}}
	results := make(chan []byte, clients)
	errs := make(chan error, clients)

	var wg sync.WaitGroup
	fetch := func() {
		defer wg.Done()
		stream, err := tier.client.GetObjects(ctx, req)
		if err != nil {
			errs <- err
			return
		}
		var data []byte
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				errs <- err
				return
			}
			data = append(data, msg.GetData()...)
		}
		results <- data
	}

	// The first client becomes the owner and blocks upstream until the others
	// have joined the in-flight load.
	wg.Add(1)
	go fetch()
	select {
	case <-upstream.started:
	case <-time.After(10 * time.Second):
		t.Fatal("upstream was never called")
	}

	for i := 1; i < clients; i++ {
		wg.Add(1)
		go fetch()
	}
	require.Eventually(t, func() bool {
		return tier.server.Stats().Coalesced == clients-1
	}, 10*time.Second, 5*time.Millisecond)

	close(upstream.gate)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	for data := range results {
		require.Equal(t, blob, data)
	}
	require.Equal(t, int64(1), upstream.getObjectsCalls.Load(), "all concurrent misses share one upstream call")
}

func TestGetObjects_RejectsCorruptUpstreamContent(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/f.git")
	upstream := newFakeObjectService()

	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("genuine content"), false)
	upstream.corruptOID(oid)
	tier := newTier(t, upstream, Config{})

	req := &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}}
	require.Equal(t, codes.DataLoss, status.Code(objectsErr(ctx, tier.client, req)))
	require.Equal(t, int64(1), tier.server.Stats().VerifyFailures)

	// Nothing was cached, so the next request goes upstream again.
	require.Equal(t, codes.DataLoss, status.Code(objectsErr(ctx, tier.client, req)))
	require.Equal(t, int64(2), upstream.getObjectsCalls.Load())
	require.Equal(t, int64(0), tier.server.Stats().ObjectHits)
}

func TestGetObjects_EvictsWithinBudget(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/g.git")
	upstream := newFakeObjectService()

	payload := func(c byte) []byte {
		data := make([]byte, 1000)
		for i := range data {
			data[i] = c
		}
		return data
	}
	a := upstream.put(repo, gitalypb.ObjectType_BLOB, payload('a'), false)
	b := upstream.put(repo, gitalypb.ObjectType_BLOB, payload('b'), false)
	c := upstream.put(repo, gitalypb.ObjectType_BLOB, payload('c'), false)

	// Room for exactly two entries of 1000 bytes plus per-entry overhead.
	budget := 2*(1000+entryOverhead) + 10
	tier := newTier(t, upstream, Config{MaxBytes: int64(budget)})

	for _, oid := range []string{a, b, c} {
		getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}})
	}
	stats := tier.server.Stats()
	require.Equal(t, int64(3), upstream.getObjectsCalls.Load())
	require.Equal(t, int64(1), stats.Evictions)
	require.LessOrEqual(t, stats.MemoryBytes, int64(budget))

	// c is still resident, a was evicted and must be fetched again.
	getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{c}})
	require.Equal(t, int64(3), upstream.getObjectsCalls.Load())

	objects, _ := getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{a}})
	require.Equal(t, payload('a'), objects[a].data)
	require.Equal(t, int64(4), upstream.getObjectsCalls.Load())
}

func TestGetObjects_IsolatesRepositories(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repoA := testRepo("@hashed/one.git")
	repoB := testRepo("@hashed/two.git")
	upstream := newFakeObjectService()

	data := []byte("the same content stored in two repositories")
	oid := upstream.put(repoA, gitalypb.ObjectType_BLOB, data, false)
	upstream.put(repoB, gitalypb.ObjectType_BLOB, data, false)
	tier := newTier(t, upstream, Config{})

	getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repoA, Oids: []string{oid}})
	require.Equal(t, int64(1), upstream.getObjectsCalls.Load())

	getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repoB, Oids: []string{oid}})
	require.Equal(t, int64(2), upstream.getObjectsCalls.Load(), "repository B must not be served from repository A's entry")

	getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repoA, Oids: []string{oid}})
	require.Equal(t, int64(2), upstream.getObjectsCalls.Load())
}

func TestGetObjects_DiskStoreSurvivesRestart(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/h.git")
	upstream := newFakeObjectService()

	data := []byte("persisted to the disk store of the cache tier")
	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, data, false)
	upstreamAddr := startGRPC(t, func(srv *grpc.Server) {
		gitalypb.RegisterObjectServiceServer(srv, upstream)
		gitalypb.RegisterRepositoryServiceServer(srv, &fakeRepositoryService{})
	})

	diskDir := t.TempDir()
	cfg := Config{Tier: TierCache, DiskDir: diskDir}
	req := &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}}

	first := newTierOver(t, upstreamAddr, cfg)
	getObjects(t, ctx, first.client, req)
	require.Equal(t, int64(1), upstream.getObjectsCalls.Load())

	// A fresh tier with empty memory reads the object back from disk.
	second := newTierOver(t, upstreamAddr, cfg)
	objects, _ := getObjects(t, ctx, second.client, req)
	require.Equal(t, data, objects[oid].data)
	require.Equal(t, int64(1), upstream.getObjectsCalls.Load())
	require.Equal(t, int64(1), second.server.Stats().DiskHits)

	// A corrupted file is discarded and the object is refetched.
	key := objectKey{storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(), oid: oid}
	require.NoError(t, os.WriteFile(second.server.disk.path(key), []byte("blob 4\nnope"), 0o644))

	third := newTierOver(t, upstreamAddr, cfg)
	objects, _ = getObjects(t, ctx, third.client, req)
	require.Equal(t, data, objects[oid].data)
	require.Equal(t, int64(2), upstream.getObjectsCalls.Load())
}

func TestNewServer_Validation(t *testing.T) {
	t.Parallel()

	conn := dialAddr(t, "127.0.0.1:1")

	for name, tc := range map[string]struct {
		conn    bool
		cfg     Config
		wantErr bool
	}{
		"valid cache tier":          {conn: true, cfg: Config{Tier: TierCache, MaxBytes: 1}},
		"valid daemon tier":         {conn: true, cfg: Config{Tier: TierDaemon, MaxBytes: 1}},
		"no upstream":               {conn: false, cfg: Config{Tier: TierCache, MaxBytes: 1}, wantErr: true},
		"unknown tier":              {conn: true, cfg: Config{Tier: "edge", MaxBytes: 1}, wantErr: true},
		"no budget":                 {conn: true, cfg: Config{Tier: TierCache}, wantErr: true},
		"daemon tier with disk dir": {conn: true, cfg: Config{Tier: TierDaemon, MaxBytes: 1, DiskDir: t.TempDir()}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			var upstream *grpc.ClientConn
			if tc.conn {
				upstream = conn
			}
			server, err := NewServer(upstream, tc.cfg)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, server)
		})
	}
}

func TestGetTree_CachesTreeOIDListings(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/i.git")
	upstream := newFakeObjectService()

	treeOID := gitObjectID("tree", []byte("tree body"), false)
	entries := make([]*gitalypb.GetTreeResponse_Entry, 0, 5)
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		entries = append(entries, &gitalypb.GetTreeResponse_Entry{
			Path: []byte(name),
			Oid:  gitObjectID("blob", []byte(name), false),
			Type: gitalypb.ObjectType_BLOB,
			Mode: 0o100644,
			Size: int64(i),
		})
	}
	upstream.putTree(treeOID, entries)
	upstream.putTree("rev:HEAD", entries)

	tier := newTier(t, upstream, Config{TreePageSize: 3})

	list := func(req *gitalypb.GetTreeRequest) ([]*gitalypb.GetTreeResponse_Entry, []*gitalypb.GetTreeResponse) {
		stream, err := tier.client.GetTree(ctx, req)
		require.NoError(t, err)
		var got []*gitalypb.GetTreeResponse_Entry
		var messages []*gitalypb.GetTreeResponse
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			messages = append(messages, msg)
			got = append(got, msg.GetEntries()...)
		}
		return got, messages
	}

	got, messages := list(&gitalypb.GetTreeRequest{Repository: repo, TreeOid: treeOID, Recursive: true})
	requireEntriesEqual(t, entries, got)
	require.Len(t, messages, 2, "entries are paged by TreePageSize")
	require.Equal(t, treeOID, messages[0].GetTreeOid())
	require.Empty(t, messages[1].GetTreeOid(), "tree_oid is only set on the first message")
	require.Equal(t, int64(1), upstream.getTreeCalls.Load())

	got, _ = list(&gitalypb.GetTreeRequest{Repository: repo, TreeOid: treeOID, Recursive: true})
	requireEntriesEqual(t, entries, got)
	require.Equal(t, int64(1), upstream.getTreeCalls.Load(), "second listing comes from cache")
	require.Equal(t, int64(1), tier.server.Stats().TreeHits)

	// Recursion changes the listing, so it is a different cache entry.
	list(&gitalypb.GetTreeRequest{Repository: repo, TreeOid: treeOID, Recursive: false})
	require.Equal(t, int64(2), upstream.getTreeCalls.Load())

	// Revision-based requests are not cached.
	list(&gitalypb.GetTreeRequest{Repository: repo, Revision: []byte("HEAD")})
	list(&gitalypb.GetTreeRequest{Repository: repo, Revision: []byte("HEAD")})
	require.Equal(t, int64(4), upstream.getTreeCalls.Load())
}

func TestGetTree_InvalidAndMissing(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/j.git")
	upstream := newFakeObjectService()
	tier := newTier(t, upstream, Config{})

	treeOID := gitObjectID("tree", []byte("present"), false)

	for name, tc := range map[string]struct {
		req  *gitalypb.GetTreeRequest
		code codes.Code
	}{
		"malformed tree oid":       {req: &gitalypb.GetTreeRequest{Repository: repo, TreeOid: "xyz"}, code: codes.InvalidArgument},
		"tree oid with revision":   {req: &gitalypb.GetTreeRequest{Repository: repo, TreeOid: treeOID, Revision: []byte("HEAD")}, code: codes.InvalidArgument},
		"missing repository":       {req: &gitalypb.GetTreeRequest{TreeOid: treeOID}, code: codes.InvalidArgument},
		"tree unknown to upstream": {req: &gitalypb.GetTreeRequest{Repository: repo, TreeOid: treeOID}, code: codes.NotFound},
	} {
		t.Run(name, func(t *testing.T) {
			stream, err := tier.client.GetTree(ctx, tc.req)
			if err == nil {
				_, err = stream.Recv()
			}
			require.Equal(t, tc.code, status.Code(err), "%v", err)
		})
	}
}

func TestChainedTiers(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/k.git")
	upstream := newFakeObjectService()

	data := []byte("read through two cache tiers")
	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, data, false)
	upstreamAddr := startGRPC(t, func(srv *grpc.Server) {
		gitalypb.RegisterObjectServiceServer(srv, upstream)
		gitalypb.RegisterRepositoryServiceServer(srv, &fakeRepositoryService{})
	})

	regional := newTierOver(t, upstreamAddr, Config{Tier: TierCache})
	daemon := newTierOver(t, regional.addr, Config{Tier: TierDaemon})

	req := &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}}

	objects, _ := getObjects(t, ctx, daemon.client, req)
	require.Equal(t, data, objects[oid].data)
	require.Equal(t, int64(1), upstream.getObjectsCalls.Load())

	objects, _ = getObjects(t, ctx, daemon.client, req)
	require.Equal(t, data, objects[oid].data)
	require.Equal(t, int64(1), upstream.getObjectsCalls.Load(), "the daemon tier serves the repeat read")
	require.Equal(t, int64(1), daemon.server.Stats().ObjectHits)
}

// TestGetObjects_LargeObjectIsAccountedExactly checks that an object larger than the
// preallocation cap keeps no spare capacity alive, so that the memory budget and the
// graft_cache_bytes metric count the memory the cache really holds.
func TestGetObjects_LargeObjectIsAccountedExactly(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/large.git")
	upstream := newFakeObjectService()
	upstream.chunkSize = 1 << 20

	data := make([]byte, 40<<20+3)
	for i := range data {
		data[i] = byte(i * 7)
	}
	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, data, false)
	tier := newTier(t, upstream, Config{MaxBytes: 64 << 20})

	objects, err := tier.server.loadObjects(ctx, repo, []string{oid})
	require.NoError(t, err)
	require.Equal(t, data, objects[oid].data)
	require.Equal(t, len(data), cap(objects[oid].data), "the object must not keep spare capacity")

	entries, bytes := tier.server.memory.usage()
	require.Equal(t, 1, entries)
	require.Equal(t, int64(len(data))+entryOverhead, bytes)
}

func TestAppendBounded(t *testing.T) {
	t.Parallel()

	const limit = 100
	var data []byte
	for len(data) < limit {
		chunk := make([]byte, min(7, limit-len(data)))
		for i := range chunk {
			chunk[i] = byte(len(data) + i)
		}
		data = appendBounded(data, chunk, limit)
		require.LessOrEqual(t, cap(data), limit)
	}

	require.Len(t, data, limit)
	require.Equal(t, limit, cap(data))
	for i, b := range data {
		require.Equal(t, byte(i), b)
	}
}
