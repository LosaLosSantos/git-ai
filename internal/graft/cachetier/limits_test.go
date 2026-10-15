package cachetier

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGetObjects_MissesOverTheRequestBudgetAreStreamedNotCached checks that a
// request whose misses exceed its byte budget does not buffer them. It streams
// through the upstream instead, so the tier never holds the objects in memory.
func TestGetObjects_MissesOverTheRequestBudgetAreStreamedNotCached(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/budget.git")
	upstream := newFakeObjectService()

	content := bytes.Repeat([]byte("large object "), 40)
	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, content, false)
	tier := newTier(t, upstream, Config{MaxRequestBytes: 64})

	req := &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}}
	for i := 0; i < 2; i++ {
		objects, _ := getObjects(t, ctx, tier.client, req)
		require.Equal(t, content, objects[oid].data)
	}

	// Each read first fills and overflows, then streams through: two upstream
	// calls per read. Nothing is cached, so the second read repeats the work.
	require.Equal(t, int64(4), upstream.getObjectsCalls.Load())
	require.Zero(t, tier.server.Stats().MemoryEntries, "objects over the request budget must not be cached")
}

// TestGetObjects_CancelledWaiterReportsCancellation checks that a request which
// is cancelled while it waits for another request's fill reports Canceled. It
// must not report Unavailable, which clients retry.
func TestGetObjects_CancelledWaiterReportsCancellation(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/waiter.git")
	upstream := newFakeObjectService()

	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("slow object"), false)
	upstream.gate = make(chan struct{})
	upstream.started = make(chan struct{}, 10)
	tier := newTier(t, upstream, Config{})

	req := &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}}

	ownerErr := make(chan error, 1)
	go func() {
		ownerErr <- objectsErr(ctx, tier.client, req)
	}()
	<-upstream.started

	waiterCtx, cancel := context.WithCancel(ctx)
	waiterErr := make(chan error, 1)
	go func() {
		_, err := tier.server.loadObjects(waiterCtx, repo, []string{oid})
		waiterErr <- err
	}()

	require.Eventually(t, func() bool {
		return tier.server.Stats().Coalesced == 1
	}, 5*time.Second, time.Millisecond, "the waiter must join the owner's fill")

	cancel()
	require.Equal(t, codes.Canceled, status.Code(<-waiterErr))

	close(upstream.gate)
	require.NoError(t, <-ownerErr)
}

// TestDiskStore_EvictionSkipsTemporaryFiles checks that eviction leaves the
// temporary file of a concurrent write alone. Removing it would make that
// write's rename fail, so the object would not be persisted.
func TestDiskStore_EvictionSkipsTemporaryFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	disk, err := newDiskStore(root, 1000)
	require.NoError(t, err)

	dir := filepath.Join(root, "in-progress")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	inProgress := filepath.Join(dir, tempFilePrefix+"123")
	require.NoError(t, os.WriteFile(inProgress, make([]byte, 2000), 0o644))
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(inProgress, old, old))

	require.NoError(t, disk.evict())

	_, err = os.Stat(inProgress)
	require.NoError(t, err, "eviction must not delete a concurrent write's temporary file")
}
