package repository

import (
	"os"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestRepositoryExists(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfgBuilder := testcfg.NewGitalyCfgBuilder(testcfg.WithStorages("default", "other", "broken"))
	cfg := cfgBuilder.Build(t)

	client, socketPath := runRepositoryService(t, cfg)
	cfg.SocketPath = socketPath

	require.NoError(t, os.RemoveAll(cfg.Storages[2].Path), "third storage needs to be invalid")

	repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{})

	queries := []struct {
		desc        string
		request     *gitalypb.RepositoryExistsRequest
		expectedErr error
		exists      bool
	}{
		{
			desc: "repository nil",
			request: &gitalypb.RepositoryExistsRequest{
				Repository: nil,
			},
			expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
		},
		{
			desc: "storage name empty",
			request: &gitalypb.RepositoryExistsRequest{
				Repository: &gitalypb.Repository{
					StorageName:  "",
					RelativePath: repo.GetRelativePath(),
				},
			},
			expectedErr: structerr.NewInvalidArgument("%w", storage.ErrStorageNotSet),
		},
		{
			desc: "relative path empty",
			request: &gitalypb.RepositoryExistsRequest{
				Repository: &gitalypb.Repository{
					StorageName:  repo.GetStorageName(),
					RelativePath: "",
				},
			},
			expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryPathNotSet),
		},
		{
			desc: "exists true",
			request: &gitalypb.RepositoryExistsRequest{
				Repository: &gitalypb.Repository{
					StorageName:  repo.GetStorageName(),
					RelativePath: repo.GetRelativePath(),
				},
			},
			exists: true,
		},
		{
			desc: "exists false, wrong storage",
			request: &gitalypb.RepositoryExistsRequest{
				Repository: &gitalypb.Repository{
					StorageName:  "other",
					RelativePath: repo.GetRelativePath(),
				},
			},
			exists: false,
		},
		{
			desc: "storage not configured",
			request: &gitalypb.RepositoryExistsRequest{
				Repository: &gitalypb.Repository{
					StorageName:  "unconfigured",
					RelativePath: "foobar.git",
				},
			},
			expectedErr: testhelper.GitalyOrPraefect(
				error(testhelper.ToInterceptedMetadata(structerr.NewInvalidArgument(
					"%w", storage.NewStorageNotFoundError("unconfigured"),
				))),
				// Praefect doesn't check for storage existence but just returns
				// that the repository doesn't exist.
				nil,
			),
		},
		{
			desc: "storage directory does not exist",
			request: &gitalypb.RepositoryExistsRequest{
				Repository: &gitalypb.Repository{
					StorageName:  "broken",
					RelativePath: "foobar.git",
				},
			},
			expectedErr: func() error {
				if testhelper.IsPraefectEnabled() {
					// Praefect checks repository existence from the database so it can't encounter an error where a storage is configured
					// but doesn't exist.
					return nil
				}

				return testhelper.WithInterceptedMetadata(
					structerr.NewNotFound("storage does not exist"),
					"storage_path", cfg.Storages[2].Path,
				)
			}(),
		},
	}

	for _, tc := range queries {
		t.Run(tc.desc, func(t *testing.T) {
			response, err := client.RepositoryExists(ctx, tc.request)
			testhelper.RequireGrpcError(t, tc.expectedErr, err)
			if err != nil {
				// Ignore the response message if there was an error
				return
			}

			require.Equal(t, tc.exists, response.GetExists())
		})
	}
}
