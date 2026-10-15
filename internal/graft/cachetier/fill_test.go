package cachetier

import (
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGetObjects_OwnerFailureDoesNotFailWaiters checks that a request waiting on
// another request's fill is not failed by that request's error. The owner asks
// for an object the upstream rejects, which fails its whole batch. The waiter
// asks only for a valid object, so it must retry and succeed.
func TestGetObjects_OwnerFailureDoesNotFailWaiters(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/poison.git")
	upstream := newFakeObjectService()

	content := []byte("popular object")
	popular := upstream.put(repo, gitalypb.ObjectType_BLOB, content, false)
	rejected := strings.Repeat("a", 64)
	upstream.reject = func(oid string) bool { return oid == rejected }
	upstream.gate = make(chan struct{})
	upstream.started = make(chan struct{}, 10)

	tier := newTier(t, upstream, Config{})

	ownerErr := make(chan error, 1)
	go func() {
		ownerErr <- objectsErr(ctx, tier.client, &gitalypb.GetObjectsRequest{
			Repository: repo,
			Oids:       []string{popular, rejected},
		})
	}()

	// The owner's fill holds the popular flight and blocks at the gate.
	<-upstream.started

	waiterErr := make(chan error, 1)
	go func() {
		waiterErr <- objectsErr(ctx, tier.client, &gitalypb.GetObjectsRequest{
			Repository: repo,
			Oids:       []string{popular},
		})
	}()

	require.Eventually(t, func() bool {
		return tier.server.Stats().Coalesced == 1
	}, 5*time.Second, time.Millisecond, "the waiter must join the owner's fill")

	close(upstream.gate)

	require.Equal(t, codes.InvalidArgument, status.Code(<-ownerErr), "the owner keeps its own error")
	require.NoError(t, <-waiterErr, "the waiter must not inherit the owner's error")

	objects, _ := getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{popular}})
	require.Equal(t, content, objects[popular].data)
}

// TestGetObjects_AlternateObjectDirectoriesBypassTheCache checks that a read through
// a quarantine view neither stores its objects nor is served from the cache.
// Otherwise the objects of a rejected push would reach plain readers.
func TestGetObjects_AlternateObjectDirectoriesBypassTheCache(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/quarantine.git")
	upstream := newFakeObjectService()

	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("quarantined secret"), false)
	tier := newTier(t, upstream, Config{})

	quarantine := &gitalypb.Repository{
		StorageName:                   repo.GetStorageName(),
		RelativePath:                  repo.GetRelativePath(),
		GitObjectDirectory:            "objects/incoming-test",
		GitAlternateObjectDirectories: []string{"objects"},
	}
	quarantineReq := &gitalypb.GetObjectsRequest{Repository: quarantine, Oids: []string{oid}}

	for i := 0; i < 2; i++ {
		objects, _ := getObjects(t, ctx, tier.client, quarantineReq)
		require.Equal(t, []byte("quarantined secret"), objects[oid].data)
	}
	require.Equal(t, int64(2), upstream.getObjectsCalls.Load(), "every quarantine read goes upstream")
	require.Zero(t, tier.server.Stats().MemoryEntries, "quarantine reads must not be stored")

	plainReq := &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}}
	objects, _ := getObjects(t, ctx, tier.client, plainReq)
	require.Equal(t, []byte("quarantined secret"), objects[oid].data)
	require.Equal(t, int64(3), upstream.getObjectsCalls.Load(), "the plain read must not be served from a quarantine read")
	require.Equal(t, 1, tier.server.Stats().MemoryEntries)
}
