package objectpool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestDelete(t *testing.T) {
	t.Parallel()

	type setupData struct {
		request      *gitalypb.DeleteObjectPoolRequest
		expectedErr  error
		expectExists bool
	}

	for _, tc := range []struct {
		desc  string
		setup func(t *testing.T, ctx context.Context, cfg config.Cfg, poolRepo *gitalypb.Repository) setupData
	}{
		{
			desc: "no pool in request fails",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, poolRepo *gitalypb.Repository) setupData {
				return setupData{
					request: &gitalypb.DeleteObjectPoolRequest{
						ObjectPool: nil,
					},
					expectedErr: testhelper.GitalyOrPraefect(
						structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
						structerr.NewInvalidArgument("no object pool repository"),
					),
					expectExists: true,
				}
			},
		},
		{
			desc: "deleting outside pools directory fails",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, poolRepo *gitalypb.Repository) setupData {
				return setupData{
					request: &gitalypb.DeleteObjectPoolRequest{ObjectPool: &gitalypb.ObjectPool{
						Repository: &gitalypb.Repository{
							StorageName:  poolRepo.GetStorageName(),
							RelativePath: ".",
						},
					}},
					expectedErr:  errInvalidPoolDir,
					expectExists: true,
				}
			},
		},
		{
			desc: "deleting pools directory fails",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, poolRepo *gitalypb.Repository) setupData {
				splitPath := strings.Split(poolRepo.GetRelativePath(), string(os.PathSeparator))

				return setupData{
					request: &gitalypb.DeleteObjectPoolRequest{ObjectPool: &gitalypb.ObjectPool{
						Repository: &gitalypb.Repository{
							StorageName:  poolRepo.GetStorageName(),
							RelativePath: splitPath[0],
						},
					}},
					expectedErr:  errInvalidPoolDir,
					expectExists: true,
				}
			},
		},
		{
			desc: "deleting first level subdirectory fails",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, poolRepo *gitalypb.Repository) setupData {
				splitPath := strings.Split(poolRepo.GetRelativePath(), string(os.PathSeparator))

				return setupData{
					request: &gitalypb.DeleteObjectPoolRequest{ObjectPool: &gitalypb.ObjectPool{
						Repository: &gitalypb.Repository{
							StorageName:  poolRepo.GetStorageName(),
							RelativePath: filepath.Join(splitPath[:2]...),
						},
					}},
					expectedErr:  errInvalidPoolDir,
					expectExists: true,
				}
			},
		},
		{
			desc: "deleting second level subdirectory fails",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, poolRepo *gitalypb.Repository) setupData {
				splitPath := strings.Split(poolRepo.GetRelativePath(), string(os.PathSeparator))

				return setupData{
					request: &gitalypb.DeleteObjectPoolRequest{ObjectPool: &gitalypb.ObjectPool{
						Repository: &gitalypb.Repository{
							StorageName:  poolRepo.GetStorageName(),
							RelativePath: filepath.Join(splitPath[:3]...),
						},
					}},
					expectedErr:  errInvalidPoolDir,
					expectExists: true,
				}
			},
		},
		{
			desc: "deleting pool subdirectory fails",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, poolRepo *gitalypb.Repository) setupData {
				validPoolPath := poolRepo.GetRelativePath()

				return setupData{
					request: &gitalypb.DeleteObjectPoolRequest{ObjectPool: &gitalypb.ObjectPool{
						Repository: &gitalypb.Repository{
							StorageName:  poolRepo.GetStorageName(),
							RelativePath: filepath.Join(validPoolPath, "objects"),
						},
					}},
					expectedErr:  errInvalidPoolDir,
					expectExists: true,
				}
			},
		},
		{
			desc: "path traversing fails",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, poolRepo *gitalypb.Repository) setupData {
				validPoolPath := poolRepo.GetRelativePath()

				return setupData{
					request: &gitalypb.DeleteObjectPoolRequest{ObjectPool: &gitalypb.ObjectPool{
						Repository: &gitalypb.Repository{
							StorageName:  poolRepo.GetStorageName(),
							RelativePath: validPoolPath + "/../../../../..",
						},
					}},
					expectedErr: testhelper.GitalyOrPraefect(
						testhelper.WithInterceptedMetadata(
							structerr.NewInvalidArgument("%w", storage.ErrRelativePathEscapesRoot),
							"relative_path", validPoolPath+"/../../../../..",
						),
						errInvalidPoolDir,
					),
					expectExists: true,
				}
			},
		},
		{
			desc: "deleting pool succeeds",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, poolRepo *gitalypb.Repository) setupData {
				validPoolPath := poolRepo.GetRelativePath()

				return setupData{
					request: &gitalypb.DeleteObjectPoolRequest{ObjectPool: &gitalypb.ObjectPool{
						Repository: &gitalypb.Repository{
							StorageName:  poolRepo.GetStorageName(),
							RelativePath: validPoolPath,
						},
					}},
				}
			},
		},
		{
			desc: "deleting non-existent pool succeeds",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, poolRepo *gitalypb.Repository) setupData {
				return setupData{
					request: &gitalypb.DeleteObjectPoolRequest{ObjectPool: &gitalypb.ObjectPool{
						Repository: &gitalypb.Repository{
							StorageName:  poolRepo.GetStorageName(),
							RelativePath: gittest.NewObjectPoolName(t),
						},
					}},
					expectExists: true,
				}
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg, repoProto, _, _, client := setup(t, ctx)
			poolProto, _, _ := createObjectPool(t, ctx, cfg, repoProto)
			data := tc.setup(t, ctx, cfg, poolProto.GetRepository())

			repositoryClient := gitalypb.NewRepositoryServiceClient(extractConn(client))

			_, err := client.DeleteObjectPool(ctx, data.request)
			testhelper.RequireGrpcError(t, data.expectedErr, err)

			response, err := repositoryClient.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{
				Repository: poolProto.GetRepository(),
			})
			require.NoError(t, err)
			require.Equal(t, data.expectExists, response.GetExists())
		})
	}
}
