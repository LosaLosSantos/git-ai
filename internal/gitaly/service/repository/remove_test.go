package repository

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestRemoveRepository(t *testing.T) {
	t.Parallel()

	cfg, client := setupRepositoryService(t)
	ctx := testhelper.Context(t)

	repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		RelativePath: gittest.NewRepositoryName(t),
	})

	createResponse, err := client.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo})
	require.NoError(t, err)
	require.True(t, createResponse.GetExists())

	_, err = client.RemoveRepository(ctx, &gitalypb.RemoveRepositoryRequest{
		Repository: repo,
	})
	require.NoError(t, err)

	resp, err := client.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo})
	require.NoError(t, err)
	require.False(t, resp.GetExists())
}

func TestRemoveRepository_doesNotExist(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	cfg, client := setupRepositoryService(t)

	_, err := client.RemoveRepository(ctx, &gitalypb.RemoveRepositoryRequest{
		Repository: &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: "/does/not/exist"},
	})

	testhelper.RequireGrpcError(t,
		testhelper.GitalyOrPraefect[error](
			testhelper.WithInterceptedMetadataItems(
				structerr.NewNotFound("repository not found"),
				structerr.MetadataItem{Key: "relative_path", Value: "OVERRIDDEN_BY_TESTHELPER"},
				structerr.MetadataItem{Key: "storage_name", Value: cfg.Storages[0].Name},
			),
			structerr.NewNotFound("repository does not exist"),
		),
		err,
	)
}

func TestRemoveRepository_validate(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	_, client := setupRepositoryService(t)
	_, err := client.RemoveRepository(ctx, &gitalypb.RemoveRepositoryRequest{Repository: nil})
	testhelper.RequireGrpcError(t, structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet), err)
}

func TestRemoveRepository_locking(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	// Praefect does not acquire a lock on repository deletion so disable the test case for Praefect.
	cfg, client := setupRepositoryService(t, testserver.WithDisablePraefect())
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

	// Simulate a concurrent RPC holding the repository lock.
	lockPath := repoPath + ".lock"
	require.NoError(t, os.WriteFile(lockPath, []byte{}, mode.File))
	defer func() { require.NoError(t, os.RemoveAll(lockPath)) }()

	_, err := client.RemoveRepository(ctx, &gitalypb.RemoveRepositoryRequest{Repository: repo})
	testhelper.RequireGrpcError(t, structerr.NewFailedPrecondition("repository is already locked"), err)
}

func TestRemoveRepository_mvcc(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	// When GIT_DEFAULT_REF_FORMAT is set, git honors it directly, so an MVCC repository
	// cannot be created distinctly.
	testhelper.SkipWithReftable(t, "MVCC backend selection is overridden by GIT_DEFAULT_REF_FORMAT")
	// An MVCC repository must be created out-of-band, so with Praefect it is never registered
	// in the database and the RPC fails before it reaches the MVCC middleware.
	testhelper.SkipWithPraefect(t, "MVCC repositories cannot be registered in Praefect's database")

	for _, tc := range []struct {
		desc  string
		setup func(t *testing.T, repoPath string)
	}{
		{
			desc:  "with a published manifest",
			setup: func(t *testing.T, repoPath string) {},
		},
		{
			// Removing the manifest pointer makes cache warming fail, so the deletion
			// only succeeds if it never enters the MVCC cache lifecycle.
			desc: "with a missing manifest pointer",
			setup: func(t *testing.T, repoPath string) {
				require.NoError(t, os.RemoveAll(filepath.Join(repoPath, "mvcc")))
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			cfg, client := setupRepositoryService(t)

			repo, repoPath := gittest.CreateMVCCRepository(t, ctx, cfg)
			gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
			tc.setup(t, repoPath)

			_, err := client.RemoveRepository(ctx, &gitalypb.RemoveRepositoryRequest{
				Repository: repo,
			})
			require.NoError(t, err)

			require.NoDirExists(t, repoPath)
		})
	}
}
