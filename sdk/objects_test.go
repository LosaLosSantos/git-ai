package sdk_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
)

func TestRepo_GetObjects(t *testing.T) {
	t.Parallel()

	t.Run("objects spanning several messages are reassembled in request order", func(t *testing.T) {
		t.Parallel()

		fakes := newFakeServices()
		fakes.objects.chunkSize = 4
		fakes.objects.objects = map[string][]byte{
			"blob-a": []byte("0123456789"),
			"tree-b": []byte("tree content"),
			"empty":  {},
		}
		fakes.objects.types = map[string]gitalypb.ObjectType{"tree-b": gitalypb.ObjectType_TREE}
		repo := dialFake(t, fakes.register).Repo("repo")

		objects, err := repo.GetObjects(t.Context(), []string{"tree-b", "blob-a", "empty"})
		require.NoError(t, err)
		require.Equal(t, []sdk.Object{
			{OID: "tree-b", Type: sdk.ObjectTree, Size: 12, Data: []byte("tree content")},
			{OID: "blob-a", Type: sdk.ObjectBlob, Size: 10, Data: []byte("0123456789")},
			{OID: "empty", Type: sdk.ObjectBlob, Size: 0, Data: nil},
		}, objects)
	})

	t.Run("missing objects are reported and do not fail the call", func(t *testing.T) {
		t.Parallel()

		fakes := newFakeServices()
		fakes.objects.objects = map[string][]byte{"present": []byte("x")}
		repo := dialFake(t, fakes.register).Repo("repo")

		objects, err := repo.GetObjects(t.Context(), []string{"absent", "present"})
		require.NoError(t, err)
		require.Len(t, objects, 2)
		require.Equal(t, "absent", objects[0].OID)
		require.True(t, objects[0].Missing)
		require.Empty(t, objects[0].Data)
		require.Equal(t, "present", objects[1].OID)
		require.False(t, objects[1].Missing)
		require.Equal(t, []byte("x"), objects[1].Data)
	})

	t.Run("duplicates are requested once and returned once", func(t *testing.T) {
		t.Parallel()

		fakes := newFakeServices()
		fakes.objects.objects = map[string][]byte{"a": []byte("A"), "b": []byte("B")}
		repo := dialFake(t, fakes.register).Repo("repo")

		objects, err := repo.GetObjects(t.Context(), []string{"b", "a", "b", "a"})
		require.NoError(t, err)
		require.Equal(t, []string{"b", "a"}, oids(objects))
		require.Equal(t, []string{"b", "a"}, fakes.objects.getObjectsRequests[0].GetOids())
	})

	t.Run("large sets are split into batches", func(t *testing.T) {
		t.Parallel()

		fakes := newFakeServices()
		fakes.objects.objects = map[string][]byte{"real": []byte("content")}
		repo := dialFake(t, fakes.register).Repo("repo")

		var requested []string
		for i := range 1200 {
			requested = append(requested, fmt.Sprintf("missing-%d", i))
		}
		requested = append(requested, "real")

		objects, err := repo.GetObjects(t.Context(), requested)
		require.NoError(t, err)
		require.Len(t, objects, 1201)
		require.Equal(t, requested, oids(objects))
		require.Equal(t, []byte("content"), objects[1200].Data)
		require.Len(t, fakes.objects.getObjectsRequests, 3)
	})

	t.Run("batches shrink to the limit the server names", func(t *testing.T) {
		t.Parallel()

		fakes := newFakeServices()
		fakes.objects.maxObjects = 100
		fakes.objects.objects = map[string][]byte{"real": []byte("content")}
		client := dialFake(t, fakes.register)
		repo := client.Repo("repo")

		var requested []string
		for i := range 249 {
			requested = append(requested, fmt.Sprintf("missing-%d", i))
		}
		requested = append(requested, "real")

		objects, err := repo.GetObjects(t.Context(), requested)
		require.NoError(t, err)
		require.Equal(t, requested, oids(objects))
		require.Equal(t, []byte("content"), objects[249].Data)

		requests := fakes.objects.getObjectsRequests
		require.Len(t, requests, 4, "one refused request, then three within the limit")
		require.Len(t, requests[0].GetOids(), 250)
		for _, req := range requests[1:] {
			require.LessOrEqual(t, len(req.GetOids()), 100)
		}

		// The client remembers the limit, also for other repositories.
		_, err = client.Repo("other").GetObjects(t.Context(), requested[:150])
		require.NoError(t, err)
		requests = fakes.objects.getObjectsRequests[4:]
		require.Len(t, requests, 2)
		require.Len(t, requests[0].GetOids(), 100)
		require.Len(t, requests[1].GetOids(), 50)
	})

	t.Run("batches are halved when the server does not name its limit", func(t *testing.T) {
		t.Parallel()

		fakes := newFakeServices()
		fakes.objects.maxObjects = 3
		fakes.objects.hideMaximum = true
		repo := dialFake(t, fakes.register).Repo("repo")

		var requested []string
		for i := range 10 {
			requested = append(requested, fmt.Sprintf("missing-%d", i))
		}

		objects, err := repo.GetObjects(t.Context(), requested)
		require.NoError(t, err)
		require.Equal(t, requested, oids(objects))
		var sizes []int
		for _, req := range fakes.objects.getObjectsRequests {
			sizes = append(sizes, len(req.GetOids()))
		}
		require.Equal(t, []int{10, 5, 2, 2, 2, 2, 2}, sizes)
	})

	t.Run("a configured batch size is used from the start", func(t *testing.T) {
		t.Parallel()

		fakes := newFakeServices()
		fakes.objects.maxObjects = 2
		repo := dialFake(t, fakes.register, sdk.WithObjectBatchSize(2)).Repo("repo")

		objects, err := repo.GetObjects(t.Context(), []string{"a", "b", "c"})
		require.NoError(t, err)
		require.Equal(t, []string{"a", "b", "c"}, oids(objects))
		require.Len(t, fakes.objects.getObjectsRequests, 2, "no request is refused")
	})

	t.Run("interrupted transfer resumes at the last received byte", func(t *testing.T) {
		t.Parallel()

		fakes := newFakeServices()
		fakes.objects.chunkSize = 4
		fakes.objects.failAfter = 2
		fakes.objects.objects = map[string][]byte{
			"a": []byte("0123456789"), // 3 messages, the stream fails after 2
			"b": []byte("hello"),
		}
		fakes.objects.types = map[string]gitalypb.ObjectType{"b": gitalypb.ObjectType_BLOB}
		repo := dialFake(t, fakes.register).Repo("repo")

		objects, err := repo.GetObjects(t.Context(), []string{"a", "b"})
		require.NoError(t, err)
		require.Equal(t, []sdk.Object{
			{OID: "a", Type: sdk.ObjectBlob, Size: 10, Data: []byte("0123456789")},
			{OID: "b", Type: sdk.ObjectBlob, Size: 5, Data: []byte("hello")},
		}, objects)

		requests := fakes.objects.getObjectsRequests
		require.Len(t, requests, 2)
		require.Equal(t, "a", requests[1].GetResumeOid())
		require.Equal(t, int64(8), requests[1].GetResumeOffset())
		require.Equal(t, []string{"a", "b"}, requests[1].GetOids())
	})

	t.Run("empty and invalid IDs", func(t *testing.T) {
		t.Parallel()

		repo := dialFake(t, newFakeServices().register).Repo("repo")

		objects, err := repo.GetObjects(t.Context(), nil)
		require.NoError(t, err)
		require.Empty(t, objects)

		_, err = repo.GetObjects(t.Context(), []string{"a", ""})
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))
	})
}

func oids(objects []sdk.Object) []string {
	out := make([]string, 0, len(objects))
	for _, o := range objects {
		out = append(out, o.OID)
	}
	return out
}
