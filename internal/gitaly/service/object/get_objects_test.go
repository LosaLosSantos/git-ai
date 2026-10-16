package object

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// fetchedObject is the reassembled result of all messages belonging to one object.
type fetchedObject struct {
	oid         string
	typ         gitalypb.ObjectType
	size        int64
	missing     bool
	firstOffset int64
	messages    int
	data        []byte
}

// foldObjects groups the streamed messages by object. It asserts the invariants of the protocol:
// chunks never exceed maxChunk, offsets are contiguous, and type and size are only set on the
// first message of an object.
func foldObjects(t testing.TB, maxChunk int64, responses []*gitalypb.GetObjectsResponse) []fetchedObject {
	t.Helper()

	var objects []fetchedObject
	for _, response := range responses {
		if len(objects) == 0 || objects[len(objects)-1].oid != response.GetOid() {
			require.True(t, response.GetMissing() || response.GetType() != gitalypb.ObjectType_UNKNOWN,
				"first message of object %s must carry its type", response.GetOid())

			objects = append(objects, fetchedObject{
				oid:         response.GetOid(),
				typ:         response.GetType(),
				size:        response.GetSize(),
				missing:     response.GetMissing(),
				firstOffset: response.GetOffset(),
			})
		} else {
			require.Equal(t, gitalypb.ObjectType_UNKNOWN, response.GetType(), "type is only set on the first message")
			require.Zero(t, response.GetSize(), "size is only set on the first message")
		}

		object := &objects[len(objects)-1]
		object.messages++

		require.LessOrEqual(t, int64(len(response.GetData())), maxChunk, "chunk exceeds the configured size")
		require.Equal(t, object.firstOffset+int64(len(object.data)), response.GetOffset(), "offsets must be contiguous")

		object.data = append(object.data, response.GetData()...)
	}

	return objects
}

// rawObject returns the raw contents of an object as reported by git-cat-file(1).
func rawObject(t testing.TB, cfg config.Cfg, repoPath, objectType string, oid git.ObjectID) []byte {
	t.Helper()
	return gittest.Exec(t, cfg, "-C", repoPath, "cat-file", objectType, oid.String())
}

func getObjects(t testing.TB, ctx context.Context, client gitalypb.ObjectServiceClient, req *gitalypb.GetObjectsRequest) ([]*gitalypb.GetObjectsResponse, error) {
	t.Helper()

	stream, err := client.GetObjects(ctx, req)
	require.NoError(t, err)

	return testhelper.Receive(stream.Recv)
}

func TestGetObjects(t *testing.T) {
	t.Parallel()

	t.Run("objects are returned in request order", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		blobOID := gittest.WriteBlob(t, cfg, repoPath, []byte("hello world\n"))
		emptyOID := gittest.WriteBlob(t, cfg, repoPath, []byte{})
		treeOID := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
			{OID: blobOID, Mode: "100644", Path: "file"},
		})
		commitOID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
			gittest.TreeEntry{OID: blobOID, Mode: "100644", Path: "file"},
		))
		tagOID := gittest.WriteTag(t, cfg, repoPath, "v1", git.Revision(commitOID), gittest.WriteTagConfig{Message: "annotated"})

		requested := []git.ObjectID{commitOID, treeOID, blobOID, emptyOID, tagOID}
		oids := make([]string, 0, len(requested))
		for _, oid := range requested {
			oids = append(oids, oid.String())
		}

		responses, err := getObjects(t, ctx, client, &gitalypb.GetObjectsRequest{
			Repository: repo,
			Oids:       oids,
		})
		require.NoError(t, err)

		objects := foldObjects(t, math.MaxInt64, responses)
		require.Equal(t, []fetchedObject{
			{oid: commitOID.String(), typ: gitalypb.ObjectType_COMMIT, size: int64(len(rawObject(t, cfg, repoPath, "commit", commitOID))), messages: 1, data: rawObject(t, cfg, repoPath, "commit", commitOID)},
			{oid: treeOID.String(), typ: gitalypb.ObjectType_TREE, size: int64(len(rawObject(t, cfg, repoPath, "tree", treeOID))), messages: 1, data: rawObject(t, cfg, repoPath, "tree", treeOID)},
			{oid: blobOID.String(), typ: gitalypb.ObjectType_BLOB, size: 12, messages: 1, data: []byte("hello world\n")},
			{oid: emptyOID.String(), typ: gitalypb.ObjectType_BLOB, size: 0, messages: 1},
			{oid: tagOID.String(), typ: gitalypb.ObjectType_TAG, size: int64(len(rawObject(t, cfg, repoPath, "tag", tagOID))), messages: 1, data: rawObject(t, cfg, repoPath, "tag", tagOID)},
		}, objects)
	})

	t.Run("large object is split into chunks", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, func(cfg *config.Cfg) {
			cfg.Graft.ObjectService.ChunkSize = 7
		})
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		content := strings.Repeat("0123456789", 5)
		blobOID := gittest.WriteBlob(t, cfg, repoPath, []byte(content))

		responses, err := getObjects(t, ctx, client, &gitalypb.GetObjectsRequest{
			Repository: repo,
			Oids:       []string{blobOID.String()},
		})
		require.NoError(t, err)

		require.Equal(t, []fetchedObject{
			{oid: blobOID.String(), typ: gitalypb.ObjectType_BLOB, size: 50, messages: 8, data: []byte(content)},
		}, foldObjects(t, 7, responses))
	})

	t.Run("resume continues at the given offset", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, func(cfg *config.Cfg) {
			cfg.Graft.ObjectService.ChunkSize = 7
		})
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		firstContent := "first object"
		content := strings.Repeat("0123456789", 5)
		firstOID := gittest.WriteBlob(t, cfg, repoPath, []byte(firstContent))
		blobOID := gittest.WriteBlob(t, cfg, repoPath, []byte(content))

		responses, err := getObjects(t, ctx, client, &gitalypb.GetObjectsRequest{
			Repository:   repo,
			Oids:         []string{firstOID.String(), blobOID.String()},
			ResumeOid:    blobOID.String(),
			ResumeOffset: 20,
		})
		require.NoError(t, err)

		require.Equal(t, []fetchedObject{
			{oid: firstOID.String(), typ: gitalypb.ObjectType_BLOB, size: int64(len(firstContent)), messages: 2, data: []byte(firstContent)},
			{oid: blobOID.String(), typ: gitalypb.ObjectType_BLOB, size: 50, firstOffset: 20, messages: 5, data: []byte(content[20:])},
		}, foldObjects(t, 7, responses))
	})

	t.Run("resume at the end of an object yields an empty message", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		blobOID := gittest.WriteBlob(t, cfg, repoPath, []byte("0123456789"))

		responses, err := getObjects(t, ctx, client, &gitalypb.GetObjectsRequest{
			Repository:   repo,
			Oids:         []string{blobOID.String()},
			ResumeOid:    blobOID.String(),
			ResumeOffset: 10,
		})
		require.NoError(t, err)

		require.Equal(t, []fetchedObject{
			{oid: blobOID.String(), typ: gitalypb.ObjectType_BLOB, size: 10, firstOffset: 10, messages: 1},
		}, foldObjects(t, math.MaxInt64, responses))
	})

	t.Run("missing objects are reported without failing the request", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		blobOID := gittest.WriteBlob(t, cfg, repoPath, []byte("present"))
		missingOID := gittest.DefaultObjectHash.HashData([]byte("never written"))

		responses, err := getObjects(t, ctx, client, &gitalypb.GetObjectsRequest{
			Repository: repo,
			Oids:       []string{missingOID.String(), blobOID.String()},
		})
		require.NoError(t, err)

		require.Equal(t, []fetchedObject{
			{oid: missingOID.String(), missing: true, messages: 1},
			{oid: blobOID.String(), typ: gitalypb.ObjectType_BLOB, size: 7, messages: 1, data: []byte("present")},
		}, foldObjects(t, math.MaxInt64, responses))
	})

	t.Run("duplicates are returned once in first occurrence order", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		blobA := gittest.WriteBlob(t, cfg, repoPath, []byte("a"))
		blobB := gittest.WriteBlob(t, cfg, repoPath, []byte("b"))

		responses, err := getObjects(t, ctx, client, &gitalypb.GetObjectsRequest{
			Repository: repo,
			Oids:       []string{blobB.String(), blobA.String(), blobB.String(), blobA.String()},
		})
		require.NoError(t, err)

		objects := foldObjects(t, math.MaxInt64, responses)
		require.Len(t, objects, 2)
		require.Equal(t, blobB.String(), objects[0].oid)
		require.Equal(t, []byte("b"), objects[0].data)
		require.Equal(t, blobA.String(), objects[1].oid)
		require.Equal(t, []byte("a"), objects[1].data)
	})

	t.Run("object count limit", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, func(cfg *config.Cfg) {
			cfg.Graft.ObjectService.MaxObjects = 2
		})
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		blobA := gittest.WriteBlob(t, cfg, repoPath, []byte("a"))
		blobB := gittest.WriteBlob(t, cfg, repoPath, []byte("b"))
		blobC := gittest.WriteBlob(t, cfg, repoPath, []byte("c"))

		t.Run("at the limit", func(t *testing.T) {
			responses, err := getObjects(t, ctx, client, &gitalypb.GetObjectsRequest{
				Repository: repo,
				Oids:       []string{blobA.String(), blobB.String()},
			})
			require.NoError(t, err)
			require.Len(t, foldObjects(t, math.MaxInt64, responses), 2)
		})

		t.Run("above the limit", func(t *testing.T) {
			_, err := getObjects(t, ctx, client, &gitalypb.GetObjectsRequest{
				Repository: repo,
				Oids:       []string{blobA.String(), blobB.String(), blobC.String()},
			})
			testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
		})
	})

	t.Run("invalid requests", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		blobA := gittest.WriteBlob(t, cfg, repoPath, []byte("a"))
		blobB := gittest.WriteBlob(t, cfg, repoPath, []byte("bb"))
		unrequested := gittest.DefaultObjectHash.HashData([]byte("unrequested"))

		for _, tc := range []struct {
			desc string
			req  *gitalypb.GetObjectsRequest
		}{
			{
				desc: "unset repository",
				req:  &gitalypb.GetObjectsRequest{Oids: []string{blobA.String()}},
			},
			{
				desc: "no object IDs",
				req:  &gitalypb.GetObjectsRequest{Repository: repo},
			},
			{
				desc: "short object ID",
				req:  &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{"abc"}},
			},
			{
				desc: "uppercase object ID",
				req:  &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{strings.ToUpper(blobA.String())}},
			},
			{
				desc: "non-hex object ID",
				req:  &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{strings.Repeat("z", len(blobA))}},
			},
			{
				desc: "abbreviated object ID",
				req:  &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{blobA.String()[:8]}},
			},
			{
				desc: "resume offset without resume object",
				req:  &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{blobA.String()}, ResumeOffset: 1},
			},
			{
				desc: "negative resume offset",
				req:  &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{blobA.String()}, ResumeOid: blobA.String(), ResumeOffset: -1},
			},
			{
				desc: "resume object not requested",
				req:  &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{blobA.String()}, ResumeOid: unrequested.String()},
			},
			{
				desc: "invalid resume object",
				req:  &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{blobA.String()}, ResumeOid: "nope"},
			},
			{
				desc: "resume offset past the end of the object",
				req:  &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{blobB.String()}, ResumeOid: blobB.String(), ResumeOffset: 3},
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				_, err := getObjects(t, ctx, client, tc.req)
				testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
			})
		}
	})

	t.Run("nonexistent repository", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)

		_, err := getObjects(t, ctx, client, &gitalypb.GetObjectsRequest{
			Repository: &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: "does-not-exist.git"},
			Oids:       []string{gittest.DefaultObjectHash.ZeroOID.String()},
		})
		testhelper.RequireGrpcCode(t, err, codes.NotFound)
	})

	t.Run("sha256 repository", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
			ObjectFormat: git.ObjectHashSHA256.Format,
		})

		blobOID := gittest.WriteBlob(t, cfg, repoPath, []byte("sha256 content"))
		require.Len(t, blobOID.String(), git.ObjectHashSHA256.EncodedLen())

		responses, err := getObjects(t, ctx, client, &gitalypb.GetObjectsRequest{
			Repository: repo,
			Oids:       []string{blobOID.String()},
		})
		require.NoError(t, err)

		require.Equal(t, []fetchedObject{
			{oid: blobOID.String(), typ: gitalypb.ObjectType_BLOB, size: 14, messages: 1, data: []byte("sha256 content")},
		}, foldObjects(t, math.MaxInt64, responses))

		// A SHA1 sized object ID is not valid in a SHA256 repository.
		_, err = getObjects(t, ctx, client, &gitalypb.GetObjectsRequest{
			Repository: repo,
			Oids:       []string{git.ObjectHashSHA1.ZeroOID.String()},
		})
		testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
	})

	t.Run("concurrent requests", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, func(cfg *config.Cfg) {
			cfg.Graft.ObjectService.ChunkSize = 4
		})
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		const blobCount = 10
		contents := make([]string, 0, blobCount)
		oids := make([]string, 0, blobCount)
		for i := range blobCount {
			content := fmt.Sprintf("blob %d with some content", i)
			contents = append(contents, content)
			oids = append(oids, gittest.WriteBlob(t, cfg, repoPath, []byte(content)).String())
		}

		const workers = 16
		results := make([][]*gitalypb.GetObjectsResponse, workers)
		errs := make([]error, workers)

		var wg sync.WaitGroup
		for worker := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()

				// Every worker asks for the same objects in a different order.
				request := make([]string, 0, blobCount)
				for i := range blobCount {
					request = append(request, oids[(i+worker)%blobCount])
				}

				stream, err := client.GetObjects(ctx, &gitalypb.GetObjectsRequest{Repository: repo, Oids: request})
				if err != nil {
					errs[worker] = err
					return
				}

				results[worker], errs[worker] = testhelper.Receive(stream.Recv)
			}()
		}
		wg.Wait()

		for worker := range workers {
			require.NoError(t, errs[worker])

			objects := foldObjects(t, 4, results[worker])
			require.Len(t, objects, blobCount)
			for i, object := range objects {
				index := (i + worker) % blobCount
				require.Equal(t, oids[index], object.oid)
				require.Equal(t, contents[index], string(object.data))
			}
		}
	})
}
