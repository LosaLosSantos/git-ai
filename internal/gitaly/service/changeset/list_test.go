package changeset

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const zeroOID = "0000000000000000000000000000000000000000"

// listAll calls ListChangesets and returns every changeset together with the
// number of messages the stream used to send them.
func listAll(tb testing.TB, ctx context.Context, client gitalypb.ChangesetServiceClient, req *gitalypb.ListChangesetsRequest) ([]*gitalypb.Changeset, int, error) {
	tb.Helper()

	stream, err := client.ListChangesets(ctx, req)
	if err != nil {
		return nil, 0, err
	}

	var (
		changesets []*gitalypb.Changeset
		messages   int
	)
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return changesets, messages, nil
		}
		if err != nil {
			return changesets, messages, err
		}

		messages++
		changesets = append(changesets, resp.GetChangesets()...)
	}
}

// appendDirectly appends n changesets to the log of repoPath without going
// through a reference update.
func appendDirectly(tb testing.TB, repoPath string, n int) {
	tb.Helper()

	updates := make([]changelog.Update, 0, n)
	for i := 0; i < n; i++ {
		updates = append(updates, changelog.Update{
			Reference: fmt.Sprintf("refs/heads/branch-%d", i),
			OldOID:    zeroOID,
			NewOID:    string(gittest.DefaultObjectHash.EmptyTreeOID),
		})
	}

	_, err := changelog.Append(repoPath, changelog.Meta{Operation: "test", Actor: "tester"}, updates)
	require.NoError(tb, err)
}

func TestListChangesets_validation(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))

	for _, tc := range []struct {
		desc        string
		req         *gitalypb.ListChangesetsRequest
		expectedErr error
	}{
		{
			desc:        "repository not set",
			req:         &gitalypb.ListChangesetsRequest{},
			expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
		},
		{
			desc: "repository without relative path",
			req: &gitalypb.ListChangesetsRequest{
				Repository: &gitalypb.Repository{StorageName: env.cfg.Storages[0].Name},
			},
			expectedErr: structerr.NewInvalidArgument("repository path not set"),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			_, _, err := listAll(t, env.ctx, env.changesets, tc.req)
			testhelper.RequireGrpcError(t, tc.expectedErr, err)
		})
	}
}

func TestListChangesets_repositoryNotFound(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))

	_, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{
		Repository: &gitalypb.Repository{
			StorageName:  env.cfg.Storages[0].Name,
			RelativePath: "does-not-exist.git",
		},
	})
	require.Equal(t, codes.NotFound, status.Code(err), "error: %v", err)
}

func TestListChangesets_emptyLog(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))
	repo, _ := gittest.CreateRepository(t, ctx, env.cfg)

	changesets, messages, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: repo})
	require.NoError(t, err)
	require.Empty(t, changesets)
	require.Zero(t, messages)
}

func TestListChangesets_fieldsAndCursor(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))
	repo, repoPath := gittest.CreateRepository(t, ctx, env.cfg)

	_, err := changelog.Append(repoPath, changelog.Meta{Operation: "land", Actor: "agent-a", Rewritten: true}, []changelog.Update{
		{Reference: "refs/heads/main", OldOID: zeroOID, NewOID: "aaaa"},
		{Reference: "refs/tags/v1", OldOID: "aaaa", NewOID: zeroOID},
		{Reference: "refs/heads/other", OldOID: "bbbb", NewOID: "cccc"},
	})
	require.NoError(t, err)

	for _, tc := range []struct {
		desc      string
		after     uint64
		limit     uint32
		sequences []uint64
	}{
		{desc: "all", after: 0, limit: 0, sequences: []uint64{1, 2, 3}},
		{desc: "after the first", after: 1, limit: 0, sequences: []uint64{2, 3}},
		{desc: "limit", after: 0, limit: 2, sequences: []uint64{1, 2}},
		{desc: "after the last", after: 3, limit: 0, sequences: nil},
		{desc: "after beyond the end", after: 100, limit: 0, sequences: nil},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{
				Repository:    repo,
				AfterSequence: tc.after,
				Limit:         tc.limit,
			})
			require.NoError(t, err)

			var sequences []uint64
			for _, c := range changesets {
				sequences = append(sequences, c.GetSequence())
			}
			require.Equal(t, tc.sequences, sequences)
		})
	}

	changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: repo})
	require.NoError(t, err)
	require.Len(t, changesets, 3)

	first := changesets[0]
	require.Equal(t, []byte("refs/heads/main"), first.GetReference())
	require.Equal(t, zeroOID, first.GetOldOid())
	require.Equal(t, "aaaa", first.GetNewOid())
	require.Equal(t, "land", first.GetOperation())
	require.Equal(t, "agent-a", first.GetActor())
	require.True(t, first.GetRewritten())
	require.NotNil(t, first.GetRecordedAt())
	require.NotZero(t, first.GetRecordedAt().AsTime().UnixNano())

	require.True(t, changesets[2].GetRewritten(), "all entries of one append share the metadata")
	require.Equal(t, []byte("refs/heads/other"), changesets[2].GetReference())
}

func TestListChangesets_batchesAndLimit(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))
	repo, repoPath := gittest.CreateRepository(t, ctx, env.cfg)

	appendDirectly(t, repoPath, 2500)

	for _, tc := range []struct {
		desc          string
		after         uint64
		limit         uint32
		expectedCount int
		expectedMsgs  int
		firstSequence uint64
	}{
		{desc: "default limit is 1000 in one message", expectedCount: 1000, expectedMsgs: 1, firstSequence: 1},
		{desc: "limit spanning two messages", limit: 1500, expectedCount: 1500, expectedMsgs: 2, firstSequence: 1},
		{desc: "limit above the batch size", limit: 2500, expectedCount: 2500, expectedMsgs: 3, firstSequence: 1},
		{desc: "cursor in the middle", after: 2000, expectedCount: 500, expectedMsgs: 1, firstSequence: 2001},
		{desc: "cursor at the end", after: 2500, expectedCount: 0, expectedMsgs: 0},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			changesets, messages, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{
				Repository:    repo,
				AfterSequence: tc.after,
				Limit:         tc.limit,
			})
			require.NoError(t, err)
			require.Len(t, changesets, tc.expectedCount)
			require.Equal(t, tc.expectedMsgs, messages)

			for i, c := range changesets {
				require.Equal(t, tc.firstSequence+uint64(i), c.GetSequence())
			}
		})
	}
}

func TestListChangesets_repositoryWithoutLogIsEmpty(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, testcfg.Build(t))
	repo, _ := gittest.CreateRepository(t, ctx, env.cfg)

	changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: repo, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, changesets)
}
