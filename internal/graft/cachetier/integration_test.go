package cachetier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	objectservice "github.com/feder-cr/open_scion/internal/gitaly/service/object"
	"github.com/feder-cr/open_scion/internal/gitaly/service/repository"
	grpcclient "github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// rpcObject is the reassembled content of one object of a GetObjects stream.
type rpcObject struct {
	typ     gitalypb.ObjectType
	data    []byte
	missing bool
}

// rpcObjects reads a complete GetObjects stream and reassembles every object.
func rpcObjects(ctx context.Context, client gitalypb.ObjectServiceClient, repo *gitalypb.Repository, oids ...string) (map[string]rpcObject, error) {
	stream, err := client.GetObjects(ctx, &gitalypb.GetObjectsRequest{Repository: repo, Oids: oids})
	if err != nil {
		return nil, err
	}

	result := make(map[string]rpcObject)
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}

		obj := result[msg.GetOid()]
		if msg.GetMissing() {
			obj.missing = true
		} else {
			if msg.GetOffset() == 0 {
				obj.typ = msg.GetType()
			}
			obj.data = append(obj.data, msg.GetData()...)
		}
		result[msg.GetOid()] = obj
	}
}

// rpcTree reads a complete GetTree stream.
func rpcTree(ctx context.Context, client gitalypb.ObjectServiceClient, req *gitalypb.GetTreeRequest) ([]*gitalypb.GetTreeResponse_Entry, error) {
	stream, err := client.GetTree(ctx, req)
	if err != nil {
		return nil, err
	}

	var entries []*gitalypb.GetTreeResponse_Entry
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, msg.GetEntries()...)
	}
}

// TestRealGitaly runs the cache tier in front of an in-process Gitaly with the
// real ObjectService. It checks that the cache and the upstream agree on the
// wire protocol, on listings and on limits.
func TestRealGitaly(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.ObjectService.ChunkSize = 7
	cfg.Graft.ObjectService.MaxObjects = 4

	upstreamAddr := testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterObjectServiceServer(srv, objectservice.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	})

	cfg.SocketPath = upstreamAddr
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	content := bytes.Repeat([]byte("graft cache object "), 12)
	blobID := gittest.WriteBlob(t, cfg, repoPath, content)
	treeID := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
		{OID: blobID, Mode: "100644", Path: "file.txt"},
	})
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTree(treeID))

	blob, tree, commit := blobID.String(), treeID.String(), commitID.String()
	// The object format follows the test mode, so the missing OID must match it.
	missing := gitObjectID("blob", []byte("never written"), gittest.ObjectHashIsSHA256())

	upstreamConn, err := grpcclient.New(ctx, upstreamAddr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upstreamConn.Close() })
	upstream := gitalypb.NewObjectServiceClient(upstreamConn)

	tierConn, err := DialUpstream(ctx, upstreamAddr, testLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tierConn.Close() })

	server, err := NewServer(tierConn, Config{
		Tier:       TierDaemon,
		MaxBytes:   1 << 20,
		ChunkSize:  16,
		MaxObjects: 4,
		Logger:     testLogger(t),
	})
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := NewGRPCServer(server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	tierConnClient := dialAddr(t, listener.Addr().String())
	tierClient := gitalypb.NewObjectServiceClient(tierConnClient)

	t.Run("objects match the upstream byte for byte", func(t *testing.T) {
		oids := []string{blob, missing, commit, tree}

		want, err := rpcObjects(ctx, upstream, repo, oids...)
		require.NoError(t, err)
		got, err := rpcObjects(ctx, tierClient, repo, oids...)
		require.NoError(t, err)

		require.Equal(t, want, got)
		require.Equal(t, content, got[blob].data)
		require.Equal(t, gitalypb.ObjectType_BLOB, got[blob].typ)
		require.Equal(t, gitalypb.ObjectType_COMMIT, got[commit].typ)
		require.Equal(t, gitalypb.ObjectType_TREE, got[tree].typ)
		require.True(t, got[missing].missing)
	})

	t.Run("repeated reads are served from memory", func(t *testing.T) {
		before := server.Stats().ObjectHits

		got, err := rpcObjects(ctx, tierClient, repo, blob)
		require.NoError(t, err)
		require.Equal(t, content, got[blob].data)
		require.Greater(t, server.Stats().ObjectHits, before)
	})

	t.Run("requests above max_objects are rejected like the upstream", func(t *testing.T) {
		tooMany := []string{blob, commit, tree, missing, gitObjectID("blob", []byte("x"), gittest.ObjectHashIsSHA256())}

		_, err := rpcObjects(ctx, upstream, repo, tooMany...)
		require.Equal(t, codes.InvalidArgument, status.Code(err))

		_, err = rpcObjects(ctx, tierClient, repo, tooMany...)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("tree listings match the upstream", func(t *testing.T) {
		for _, req := range []*gitalypb.GetTreeRequest{
			{Repository: repo, TreeOid: tree},
			{Repository: repo, TreeOid: tree, IncludeSizes: true},
			{Repository: repo, TreeOid: tree, Recursive: true, IncludeSizes: true},
		} {
			want, err := rpcTree(ctx, upstream, req)
			require.NoError(t, err)
			got, err := rpcTree(ctx, tierClient, req)
			require.NoError(t, err)
			require.Equal(t, want, got)
			require.NotEmpty(t, got)
		}
	})

	t.Run("revision listings are forwarded and match the upstream", func(t *testing.T) {
		req := &gitalypb.GetTreeRequest{Repository: repo, Revision: []byte(commit)}

		want, err := rpcTree(ctx, upstream, req)
		require.NoError(t, err)
		got, err := rpcTree(ctx, tierClient, req)
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.NotEmpty(t, got)
	})

	t.Run("unary RPCs are proxied to the real upstream", func(t *testing.T) {
		resp, err := gitalypb.NewRepositoryServiceClient(tierConnClient).RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo})
		require.NoError(t, err)
		require.True(t, resp.GetExists())
	})

	t.Run("concurrent readers get consistent objects", func(t *testing.T) {
		const readers = 10

		var wg sync.WaitGroup
		errs := make(chan error, readers)
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				got, err := rpcObjects(ctx, tierClient, repo, blob, commit, missing)
				if err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(got[blob].data, content) || !got[missing].missing {
					errs <- fmt.Errorf("unexpected objects: blob %d bytes, missing %t", len(got[blob].data), got[missing].missing)
				}
			}()
		}
		wg.Wait()
		close(errs)

		for err := range errs {
			require.NoError(t, err)
		}
	})
}
