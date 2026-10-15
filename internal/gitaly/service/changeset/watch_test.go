package changeset

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The watch tests are not parallel: some of them change watchPollInterval.

// recvResult is the outcome of one Recv call.
type recvResult struct {
	changeset *gitalypb.Changeset
	err       error
}

// watchStream reads the messages of a WatchChangesets stream in the background.
type watchStream struct {
	t       *testing.T
	results chan recvResult
}

func startWatch(t *testing.T, ctx context.Context, client gitalypb.ChangesetServiceClient, req *gitalypb.WatchChangesetsRequest) *watchStream {
	t.Helper()

	stream, err := client.WatchChangesets(ctx, req)
	require.NoError(t, err)

	ws := &watchStream{t: t, results: make(chan recvResult, 100)}
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				ws.results <- recvResult{err: err}
				return
			}
			ws.results <- recvResult{changeset: resp.GetChangeset()}
		}
	}()

	return ws
}

// next returns the next changeset of the stream, failing the test if none
// arrives in time or the stream ends.
func (ws *watchStream) next() *gitalypb.Changeset {
	ws.t.Helper()

	select {
	case res := <-ws.results:
		require.NoError(ws.t, res.err)
		return res.changeset
	case <-time.After(10 * time.Second):
		ws.t.Fatal("timed out waiting for a changeset")
		return nil
	}
}

func TestWatchChangesets_validation(t *testing.T) {
	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))

	stream, err := env.changesets.WatchChangesets(env.ctx, &gitalypb.WatchChangesetsRequest{})
	require.NoError(t, err)

	_, err = stream.Recv()
	testhelper.RequireGrpcError(t, structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet), err)
}

func TestWatchChangesets_repositoryNotFound(t *testing.T) {
	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))

	stream, err := env.changesets.WatchChangesets(env.ctx, &gitalypb.WatchChangesetsRequest{
		Repository: &gitalypb.Repository{StorageName: env.cfg.Storages[0].Name, RelativePath: "missing.git"},
	})
	require.NoError(t, err)

	_, err = stream.Recv()
	require.Equal(t, codes.NotFound, status.Code(err), "error: %v", err)
}

func TestWatchChangesets_backlogThenLive(t *testing.T) {
	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))
	repo, repoPath := gittest.CreateRepository(t, ctx, env.cfg)

	_, err := changelog.Append(repoPath, changelog.Meta{Operation: "land", Actor: "agent-a"}, []changelog.Update{
		{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: "aaaa"},
		{Reference: "refs/heads/b", OldOID: zeroOID, NewOID: "bbbb"},
	})
	require.NoError(t, err)

	watchCtx, cancel := context.WithCancel(env.ctx)
	defer cancel()

	ws := startWatch(t, watchCtx, env.changesets, &gitalypb.WatchChangesetsRequest{Repository: repo})

	require.Equal(t, uint64(1), ws.next().GetSequence(), "the backlog comes first")
	require.Equal(t, uint64(2), ws.next().GetSequence())

	// Appended while the watch is open: delivered live.
	_, err = changelog.Append(repoPath, changelog.Meta{Operation: "land", Actor: "agent-a"}, []changelog.Update{
		{Reference: "refs/heads/c", OldOID: zeroOID, NewOID: "cccc"},
	})
	require.NoError(t, err)

	live := ws.next()
	require.Equal(t, uint64(3), live.GetSequence())
	require.Equal(t, []byte("refs/heads/c"), live.GetReference())
	require.Equal(t, "land", live.GetOperation())
	require.Equal(t, "agent-a", live.GetActor())
}

func TestWatchChangesets_afterSequenceSkipsBacklog(t *testing.T) {
	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))
	repo, repoPath := gittest.CreateRepository(t, ctx, env.cfg)

	appendDirectly(t, repoPath, 3)

	watchCtx, cancel := context.WithCancel(env.ctx)
	defer cancel()

	ws := startWatch(t, watchCtx, env.changesets, &gitalypb.WatchChangesetsRequest{Repository: repo, AfterSequence: 2})
	require.Equal(t, uint64(3), ws.next().GetSequence())

	appendDirectly(t, repoPath, 1)
	require.Equal(t, uint64(4), ws.next().GetSequence())
}

func TestWatchChangesets_receivesLiveUpdates(t *testing.T) {
	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))
	repo, repoPath := gittest.CreateRepository(t, ctx, env.cfg)

	c1 := gittest.WriteCommit(t, env.cfg, repoPath).String()

	watchCtx, cancel := context.WithCancel(env.ctx)
	defer cancel()

	ws := startWatch(t, watchCtx, env.changesets, &gitalypb.WatchChangesetsRequest{Repository: repo})

	_, err := env.operations.UserUpdateBranch(env.ctx, &gitalypb.UserUpdateBranchRequest{
		Repository: repo,
		BranchName: []byte("branch"),
		Oldrev:     []byte(gittest.DefaultObjectHash.ZeroOID),
		Newrev:     []byte(c1),
		User:       gittest.TestUser,
	})
	require.NoError(t, err)

	got := ws.next()
	require.Equal(t, uint64(1), got.GetSequence())
	require.Equal(t, []byte("refs/heads/branch"), got.GetReference())
	require.Equal(t, c1, got.GetNewOid())
	require.Equal(t, "update_branch", got.GetOperation())
	require.Equal(t, gittest.GlID, got.GetActor())
}

func TestWatchChangesets_pollFallbackSeesUnnotifiedAppends(t *testing.T) {
	// Not parallel: it changes watchPollInterval.
	previous := watchPollInterval
	watchPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { watchPollInterval = previous })

	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))
	repo, repoPath := gittest.CreateRepository(t, ctx, env.cfg)

	watchCtx, cancel := context.WithCancel(env.ctx)
	defer cancel()

	ws := startWatch(t, watchCtx, env.changesets, &gitalypb.WatchChangesetsRequest{Repository: repo})

	// Write the entry behind the changelog's back, so that no notification is sent.
	line := fmt.Sprintf(`{"sequence":1,"reference":"refs/heads/ext","old_oid":"%s","new_oid":"aaaa","operation":"external","actor":"other","recorded_at":"2026-01-01T00:00:00Z"}`+"\n", zeroOID)
	require.NoError(t, os.MkdirAll(repoPath+"/graft", 0o755))
	require.NoError(t, appendFile(changelog.Path(repoPath), line))

	got := ws.next()
	require.Equal(t, uint64(1), got.GetSequence())
	require.Equal(t, "external", got.GetOperation())
}

// appendFile appends content to the file at path, creating it if needed.
func appendFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}

	return f.Close()
}

// fakeWatchStream is a WatchChangesets server stream that hands the sent
// changesets to the test and whose context the test cancels.
type fakeWatchStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan *gitalypb.Changeset
}

func (s *fakeWatchStream) Context() context.Context { return s.ctx }

func (s *fakeWatchStream) Send(resp *gitalypb.WatchChangesetsResponse) error {
	select {
	case s.sent <- resp.GetChangeset():
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func TestWatchChangesets_handlerReturnsOnCancel(t *testing.T) {
	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))
	repo, repoPath := gittest.CreateRepository(t, ctx, env.cfg)

	appendDirectly(t, repoPath, 2)

	// The server handler is called directly, so that its return can be observed.
	// A cancelled gRPC client returns Canceled without waiting for the handler.
	watchCtx, cancel := context.WithCancel(env.ctx)
	defer cancel()

	stream := &fakeWatchStream{ctx: watchCtx, sent: make(chan *gitalypb.Changeset, 10)}
	done := make(chan error, 1)
	go func() {
		done <- env.direct.WatchChangesets(&gitalypb.WatchChangesetsRequest{Repository: repo}, stream)
	}()

	for want := uint64(1); want <= 2; want++ {
		select {
		case got := <-stream.sent:
			require.Equal(t, want, got.GetSequence())
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the backlog")
		}
	}

	// Append while the watch is live, so the handler is waiting on its wake-up
	// channels when the context is cancelled.
	appendDirectly(t, repoPath, 1)
	select {
	case got := <-stream.sent:
		require.Equal(t, uint64(3), got.GetSequence())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the live changeset")
	}

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a cancelled watch ends without an error")
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not return after the context was cancelled")
	}
}

// TestWatchChangesets_cancellationEndsStream checks the client side: the
// client gets Canceled once it cancels. TestWatchChangesets_handlerReturnsOnCancel
// checks that the server handler returns.
func TestWatchChangesets_cancellationEndsStream(t *testing.T) {
	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))
	repo, _ := gittest.CreateRepository(t, ctx, env.cfg)

	watchCtx, cancel := context.WithCancel(env.ctx)

	stream, err := env.changesets.WatchChangesets(watchCtx, &gitalypb.WatchChangesetsRequest{Repository: repo})
	require.NoError(t, err)

	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		done <- err
	}()

	select {
	case err := <-done:
		require.Equal(t, codes.Canceled, status.Code(err), "error: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled watch did not end")
	}
}
