package repoutil

import (
	"context"
	"os"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/counter"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestRemove(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc        string
		createRepo  func(tb testing.TB, ctx context.Context, cfg config.Cfg) (*gitalypb.Repository, string)
		expectedErr error
	}{
		{
			desc: "success",
			createRepo: func(tb testing.TB, ctx context.Context, cfg config.Cfg) (*gitalypb.Repository, string) {
				return gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})
			},
		},
		{
			desc: "does not exist",
			createRepo: func(tb testing.TB, ctx context.Context, cfg config.Cfg) (*gitalypb.Repository, string) {
				repo := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: "/does/not/exist"}
				return repo, ""
			},
			expectedErr: structerr.NewNotFound("repository does not exist"),
		},
		{
			desc: "locked",
			createRepo: func(tb testing.TB, ctx context.Context, cfg config.Cfg) (*gitalypb.Repository, string) {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				// Simulate a concurrent RPC holding the repository lock.
				lockPath := repoPath + ".lock"
				require.NoError(t, os.WriteFile(lockPath, []byte{}, mode.File))
				tb.Cleanup(func() {
					require.NoError(t, os.RemoveAll(lockPath))
				})

				return repo, repoPath
			},
			expectedErr: structerr.NewFailedPrecondition("repository is already locked"),
		},
		{
			desc: "unfinished deletion doesn't fail subsequent deletions",
			createRepo: func(tb testing.TB, ctx context.Context, cfg config.Cfg) (*gitalypb.Repository, string) {
				repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				require.NoError(t,
					remove(
						ctx,
						testhelper.SharedLogger(t),
						config.NewLocator(cfg),
						transaction.NewTrackingManager(),
						repo,
						// Pass a no-op removeAll to leave the temporary directory in place. Previous unfinished
						// deletion for the same relative path should not cause any conflicts.
						func(string) error { return nil },
					),
				)

				return gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
					RelativePath:           repo.GetRelativePath(),
				})
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			logger := testhelper.SharedLogger(t)
			locator := config.NewLocator(cfg)
			txManager := transaction.NewTrackingManager()
			repoCounter := counter.NewRepositoryCounter(cfg.Storages)

			repo, repoPath := tc.createRepo(t, ctx, cfg)

			if repoPath != "" {
				require.DirExists(t, repoPath)
			}

			err := Remove(ctx, logger, locator, txManager, repoCounter, repo)

			if tc.expectedErr != nil {
				require.Equal(t, tc.expectedErr, err)
				return
			}

			require.NoError(t, err)

			if repoPath != "" {
				require.NoDirExists(t, repoPath)
			}
		})
	}
}
