package review

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/corestore/coretest"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// setupWithStorageCore runs a ReviewService whose storage core is in the given
// mode, on the SQLite database of the test.
func setupWithStorageCore(t *testing.T, mode corestore.Mode) (context.Context, config.Cfg, *corestore.Core, gitalypb.ReviewServiceClient) {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	core := coretest.Open(t, coretest.Options{Mode: mode})

	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterReviewServiceServer(srv, newServer(deps))
	}, testserver.WithGraftStorage(core))

	conn, err := client.New(ctx, cfg.SocketPath)
	require.NoError(t, err)
	t.Cleanup(func() { testhelper.MustClose(t, conn) })

	return ctx, cfg, core, gitalypb.NewReviewServiceClient(conn)
}

// setupAuthoritative runs a ReviewService whose storage core is in
// authoritative mode.
func setupAuthoritative(t *testing.T) (context.Context, config.Cfg, *corestore.Core, gitalypb.ReviewServiceClient) {
	t.Helper()
	return setupWithStorageCore(t, corestore.ModeAuthoritative)
}

// createLocalRepository creates a repository on disk only: the test server
// runs no RepositoryService.
func createLocalRepository(t *testing.T, ctx context.Context, cfg config.Cfg) (*gitalypb.Repository, string) {
	t.Helper()
	return gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
}

// importRepository imports the local repository into the storage core, as
// ImportRepository does. The local references must be written before the call:
// the import records every reference of the repository.
func importRepository(t *testing.T, cfg config.Cfg, core *corestore.Core, repo *gitalypb.Repository, repoPath string, opts coretest.ImportOptions) corestore.Repository {
	t.Helper()
	return coretest.Import(t, core, localrepo.NewTestRepo(t, cfg, repo), repo, repoPath, opts)
}

// listStoredReviews drains ListReviews of the repository.
func listStoredReviews(t *testing.T, ctx context.Context, client gitalypb.ReviewServiceClient, repo *gitalypb.Repository, includeClosed bool) []*gitalypb.Review {
	t.Helper()

	stream, err := client.ListReviews(ctx, &gitalypb.ListReviewsRequest{Repository: repo, IncludeClosed: includeClosed})
	require.NoError(t, err)

	var reviews []*gitalypb.Review
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		reviews = append(reviews, resp.GetReviews()...)
	}
	return reviews
}

func TestAuthoritativeReviewsLiveInDatabase(t *testing.T) {
	ctx, cfg, core, client := setupAuthoritative(t)

	repo, repoPath := createLocalRepository(t, ctx, cfg)
	head := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	tracked := importRepository(t, cfg, core, repo, repoPath, coretest.ImportOptions{})

	created, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository:   repo,
		TargetBranch: []byte("main"),
		HeadCommitId: head.String(),
	})
	require.NoError(t, err)
	require.Equal(t, "1", created.GetReview().GetId())

	stored, version, err := core.Meta.GetReview(ctx, tracked.ID, "1")
	require.NoError(t, err, "the review is in the database")
	require.Equal(t, int64(1), version)
	require.Equal(t, []byte("main"), stored.GetTargetBranch())
	require.False(t, stored.GetApproved(), "computed fields are not stored")
	require.Empty(t, stored.GetRequirements())

	require.NoDirExists(t, filepath.Join(repoPath, "graft", "reviews"), "no review files are written")

	got, err := client.GetReview(ctx, &gitalypb.GetReviewRequest{Repository: repo, Id: "1"})
	require.NoError(t, err)
	require.Equal(t, "1", got.GetReview().GetId())

	_, err = client.GetReview(ctx, &gitalypb.GetReviewRequest{Repository: repo, Id: "42"})
	require.Equal(t, codes.NotFound, status.Code(err))

	reviews := listStoredReviews(t, ctx, client, repo, false)
	require.Len(t, reviews, 1)
	require.Equal(t, "1", reviews[0].GetId())

	// Updates go through the version check of the database.
	comment, err := client.AddReviewComment(ctx, &gitalypb.AddReviewCommentRequest{
		Repository: repo,
		Id:         "1",
		Body:       []byte("looks good"),
	})
	require.NoError(t, err)
	require.Equal(t, "1", comment.GetComment().GetId())

	closed, err := client.CloseReview(ctx, &gitalypb.CloseReviewRequest{Repository: repo, Id: "1"})
	require.NoError(t, err)
	require.Equal(t, gitalypb.Review_STATE_CLOSED, closed.GetReview().GetState())

	stored, version, err = core.Meta.GetReview(ctx, tracked.ID, "1")
	require.NoError(t, err)
	require.Equal(t, int64(3), version, "one version per update")
	require.Equal(t, gitalypb.Review_STATE_CLOSED, stored.GetState())
	require.Len(t, stored.GetComments(), 1)

	require.Empty(t, listStoredReviews(t, ctx, client, repo, false), "closed reviews are not listed")
	require.Len(t, listStoredReviews(t, ctx, client, repo, true), 1)

	_, err = client.CloseReview(ctx, &gitalypb.CloseReviewRequest{Repository: repo, Id: "42"})
	require.Equal(t, codes.NotFound, status.Code(err))

	require.NoDirExists(t, filepath.Join(repoPath, "graft", "reviews"), "no review files are written")
}

func TestAuthoritativeImportedReviewsAreServed(t *testing.T) {
	ctx, cfg, core, client := setupAuthoritative(t)

	repo, repoPath := createLocalRepository(t, ctx, cfg)
	head := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	// Reviews imported with the repository keep their IDs; the next review
	// gets the following one.
	importRepository(t, cfg, core, repo, repoPath, coretest.ImportOptions{
		Reviews: []*gitalypb.Review{
			{Id: "7", TargetBranch: []byte("main"), HeadCommitId: head.String()},
		},
	})

	got, err := client.GetReview(ctx, &gitalypb.GetReviewRequest{Repository: repo, Id: "7"})
	require.NoError(t, err)
	require.Equal(t, head.String(), got.GetReview().GetHeadCommitId())

	created, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository:   repo,
		TargetBranch: []byte("main"),
		HeadCommitId: head.String(),
	})
	require.NoError(t, err)
	require.Equal(t, "8", created.GetReview().GetId())

	reviews := listStoredReviews(t, ctx, client, repo, false)
	require.Len(t, reviews, 2)
	require.Equal(t, "8", reviews[0].GetId())
	require.Equal(t, "7", reviews[1].GetId())
}

func TestAuthoritativeUntrackedRepositoryIsRejected(t *testing.T) {
	ctx, cfg, _, client := setupAuthoritative(t)

	// The repository is never imported into the storage core.
	repo, repoPath := createLocalRepository(t, ctx, cfg)
	head := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	_, err := client.GetReview(ctx, &gitalypb.GetReviewRequest{Repository: repo, Id: "1"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	_, err = client.CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository:   repo,
		TargetBranch: []byte("main"),
		HeadCommitId: head.String(),
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.NoDirExists(t, filepath.Join(repoPath, "graft", "reviews"))
}

func TestShadowReviewsStayInFiles(t *testing.T) {
	ctx, cfg, core, client := setupWithStorageCore(t, corestore.ModeShadow)

	repo, repoPath := createLocalRepository(t, ctx, cfg)
	head := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	tracked := importRepository(t, cfg, core, repo, repoPath, coretest.ImportOptions{})

	created, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository:   repo,
		TargetBranch: []byte("main"),
		HeadCommitId: head.String(),
	})
	require.NoError(t, err)
	require.Equal(t, "1", created.GetReview().GetId())

	require.FileExists(t, filepath.Join(repoPath, "graft", "reviews", "1.json"), "the files are the source of truth")

	_, _, err = core.Meta.GetReview(ctx, tracked.ID, "1")
	require.ErrorIs(t, err, corestore.ErrReviewNotFound, "shadow mode does not write reviews to the database")
}
