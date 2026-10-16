package gate_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	reviewstore "github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestGateRefusesShadowHistoryNotImported checks the guard of the LOCK AND
// HISTORY RULE: a tracked repository whose file changelog has entries beyond
// the database's log sequence (written in shadow mode) is refused until its
// history is imported, and the passed check is remembered.
func TestGateRefusesShadowHistoryNotImported(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	g, cfg, core := newGateEnv(t, corestore.ModeAuthoritative, &fakeFreshener{})
	repo, repoPath := untrackedRepo(t, ctx, cfg)
	record := registerHead(t, ctx, core, repo)

	appendShadow := func(t *testing.T, ref string) []changelog.Entry {
		t.Helper()
		entries, err := changelog.Append(repoPath, changelog.Meta{Operation: "shadow", Actor: "alice"}, []changelog.Update{
			{Reference: ref, OldOID: gittest.DefaultObjectHash.ZeroOID.String(), NewOID: gittest.DefaultObjectHash.EmptyTreeOID.String()},
		})
		require.NoError(t, err)
		return entries
	}

	call := func(t *testing.T) (bool, error) {
		t.Helper()
		handled := false
		_, err := g.UnaryInterceptor()(ctx, &gitalypb.HasLocalBranchesRequest{Repository: repo},
			&grpc.UnaryServerInfo{FullMethod: accessorMethod}, func(context.Context, any) (any, error) {
				handled = true
				return &gitalypb.HasLocalBranchesResponse{}, nil
			})
		return handled, err
	}

	shadow := append(appendShadow(t, "refs/heads/main"), appendShadow(t, "refs/heads/feature")...)

	handled, err := call(t)
	require.False(t, handled)
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "error %v", err)
	require.Contains(t, err.Error(), "the shadow-mode history of this repository is not in the storage core yet: run graft storage import")

	// The stream interceptor refuses it too, on the first message.
	err = g.StreamInterceptor()(nil, &fakeStream{ctx: ctx, msg: &gitalypb.UpdateReferencesRequest{Repository: repo}},
		&grpc.StreamServerInfo{FullMethod: allowedStreamMutator}, func(srv any, stream grpc.ServerStream) error {
			return stream.RecvMsg(&gitalypb.UpdateReferencesRequest{})
		})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "error %v", err)

	// Only part of the history in the database is not enough.
	_, _, err = core.Meta.ImportHistory(ctx, record.ID, shadow[:1], nil)
	require.NoError(t, err)
	handled, err = call(t)
	require.False(t, handled)
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "error %v", err)

	_, _, err = core.Meta.ImportHistory(ctx, record.ID, shadow[1:], nil)
	require.NoError(t, err)
	handled, err = call(t)
	require.NoError(t, err)
	require.True(t, handled)

	// The passed check is remembered for the repository.
	appendShadow(t, "refs/heads/late")
	handled, err = call(t)
	require.NoError(t, err)
	require.True(t, handled)
}

// TestGateRefusesShadowReviewsNotImported checks the review half of the guard:
// a review created or changed in the file review store (shadow mode) that
// ImportHistory would still write makes the gate refuse the repository, also
// when the file changelog is empty (no reference moved), until it is imported.
func TestGateRefusesShadowReviewsNotImported(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	g, cfg, core := newGateEnv(t, corestore.ModeAuthoritative, &fakeFreshener{})

	call := func(t *testing.T, repo *gitalypb.Repository) (bool, error) {
		t.Helper()
		handled := false
		_, err := g.UnaryInterceptor()(ctx, &gitalypb.HasLocalBranchesRequest{Repository: repo},
			&grpc.UnaryServerInfo{FullMethod: accessorMethod}, func(context.Context, any) (any, error) {
				handled = true
				return &gitalypb.HasLocalBranchesResponse{}, nil
			})
		return handled, err
	}

	requireRefused := func(t *testing.T, repo *gitalypb.Repository, reviewID string) {
		t.Helper()
		handled, err := call(t, repo)
		require.False(t, handled)
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "error %v", err)
		require.Contains(t, err.Error(), "the shadow-mode history of this repository is not in the storage core yet: run graft storage import")
		require.Equal(t, reviewID, structerr.ExtractMetadata(err)["review_id"])
	}

	requireServed := func(t *testing.T, repo *gitalypb.Repository) {
		t.Helper()
		handled, err := call(t, repo)
		require.NoError(t, err)
		require.True(t, handled)
	}

	t.Run("review created in shadow mode", func(t *testing.T) {
		repo, repoPath := untrackedRepo(t, ctx, cfg)
		record := registerHead(t, ctx, core, repo)

		created, err := reviewstore.Open(repoPath).Create(&gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("shadow")})
		require.NoError(t, err)

		requireRefused(t, repo, created.GetId())

		_, imported, err := core.Meta.ImportHistory(ctx, record.ID, nil, []*gitalypb.Review{created})
		require.NoError(t, err)
		require.Equal(t, 1, imported)
		requireServed(t, repo)
	})

	t.Run("imported review changed in shadow mode", func(t *testing.T) {
		repo, repoPath := untrackedRepo(t, ctx, cfg)
		record := registerHead(t, ctx, core, repo)

		file, err := reviewstore.Open(repoPath).Create(&gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("changed")})
		require.NoError(t, err)

		// The database has the version of an earlier import: same review, older
		// update time.
		atImport := proto.Clone(file).(*gitalypb.Review)
		atImport.Title = []byte("as imported")
		atImport.UpdatedAt = timestamppb.New(file.GetUpdatedAt().AsTime().Add(-time.Hour))
		_, imported, err := core.Meta.ImportHistory(ctx, record.ID, nil, []*gitalypb.Review{atImport})
		require.NoError(t, err)
		require.Equal(t, 1, imported)

		requireRefused(t, repo, file.GetId())

		_, imported, err = core.Meta.ImportHistory(ctx, record.ID, nil, []*gitalypb.Review{file})
		require.NoError(t, err)
		require.Equal(t, 1, imported)
		requireServed(t, repo)

		stored, _, err := core.Meta.GetReview(ctx, record.ID, file.GetId())
		require.NoError(t, err)
		require.Equal(t, "changed", string(stored.GetTitle()))
	})

	t.Run("reviews already imported", func(t *testing.T) {
		repo, repoPath := untrackedRepo(t, ctx, cfg)
		record := registerHead(t, ctx, core, repo)

		file, err := reviewstore.Open(repoPath).Create(&gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("imported")})
		require.NoError(t, err)
		_, _, err = core.Meta.ImportHistory(ctx, record.ID, nil, []*gitalypb.Review{file})
		require.NoError(t, err)

		requireServed(t, repo)
	})
}

// TestGateServesRepositoriesWithoutShadowHistory checks that the guard does not
// refuse a tracked repository whose file changelog is empty, or whose entries
// are all in the database.
func TestGateServesRepositoriesWithoutShadowHistory(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	g, cfg, core := newGateEnv(t, corestore.ModeAuthoritative, &fakeFreshener{})

	for _, withHistory := range []bool{false, true} {
		repo, repoPath := untrackedRepo(t, ctx, cfg)
		if withHistory {
			entries, err := changelog.Append(repoPath, changelog.Meta{Operation: "import"}, []changelog.Update{
				{Reference: "refs/heads/main", OldOID: gittest.DefaultObjectHash.ZeroOID.String(), NewOID: gittest.DefaultObjectHash.EmptyTreeOID.String()},
			})
			require.NoError(t, err)
			_, _, err = core.Meta.RegisterRepository(ctx, corestore.KeyOf(repo), gittest.DefaultObjectHash.Format, corestore.InitialState{
				Changeset: &corestore.ChangesetInput{
					Updates:   []corestore.RefUpdate{{Name: "HEAD", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")}},
					Operation: "create_repository",
					NodeID:    core.NodeID,
				},
				Changelog: entries,
			})
			require.NoError(t, err)
		} else {
			registerHead(t, ctx, core, repo)
		}

		handled := false
		_, err := g.UnaryInterceptor()(ctx, &gitalypb.HasLocalBranchesRequest{Repository: repo},
			&grpc.UnaryServerInfo{FullMethod: accessorMethod}, func(context.Context, any) (any, error) {
				handled = true
				return &gitalypb.HasLocalBranchesResponse{}, nil
			})
		require.NoError(t, err, "with history: %v", withHistory)
		require.True(t, handled)
	}
}
