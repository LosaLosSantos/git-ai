package repository

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/housekeeping"
	housekeepingmgr "github.com/feder-cr/open_scion/internal/git/housekeeping/manager"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/git/stats"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/gitaly/storage/mode"
	"github.com/feder-cr/open_scion/internal/helper/text"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestOptimizeRepository(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRepositoryService(t)

	t.Run("gitconfig credentials get pruned", func(t *testing.T) {
		t.Parallel()

		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		gitconfigPath := filepath.Join(repoPath, "config")

		readConfig := func() []string {
			return strings.Split(text.ChompBytes(gittest.Exec(t, cfg, "config", "--file", gitconfigPath, "--list")), "\n")
		}

		configWithSecrets := readConfig()
		configWithStrippedSecrets := readConfig()

		// Set up a gitconfig with all sorts of credentials.
		for key, shouldBeStripped := range map[string]bool{
			"http.http://localhost:51744/60631c8695bf041a808759a05de53e36a73316aacb502824fabbb0c6055637c1.git.extraHeader": true,
			"http.http://localhost:51744/60631c8695bf041a808759a05de53e36a73316aacb502824fabbb0c6055637c2.git.extraHeader": true,
			"hTTp.http://localhost:51744/60631c8695bf041a808759a05de53e36a73316aacb502824fabbb0c6055637c5.git.ExtrAheaDeR": true,
			"http.http://extraheader/extraheader/extraheader.git.extraHeader":                                              true,
			// This line should not get stripped as Git wouldn't even know how to
			// interpret it due to the `https` prefix. Git only knows about `http`.
			"https.https://localhost:51744/60631c8695bf041a808759a05de53e36a73316aacb502824fabbb0c6055637c5.git.extraHeader": false,
			// This one should not get stripped as its prefix is wrong.
			"randomStart-http.http://localhost:51744/60631c8695bf041a808759a05de53e36a73316aacb502824fabbb0c6055637c3.git.extraHeader": false,
			// Same here, this one should not get stripped because its suffix is wrong.
			"http.http://localhost:51744/60631c8695bf041a808759a05de53e36a73316aacb502824fabbb0c6055637c4.git.extraHeader-randomEnd": false,
		} {
			value := "Authorization: Basic secret-password"
			line := fmt.Sprintf("%s=%s", strings.ToLower(key), value)

			gittest.Exec(t, cfg, "config", "--file", gitconfigPath, key, value)

			configWithSecrets = append(configWithSecrets, line)
			if !shouldBeStripped {
				configWithStrippedSecrets = append(configWithStrippedSecrets, line)
			}
		}
		require.Equal(t, configWithSecrets, readConfig())

		// Calling OptimizeRepository should cause us to strip any of the added creds from
		// the gitconfig.
		_, err := client.OptimizeRepository(ctx, &gitalypb.OptimizeRepositoryRequest{
			Repository: repo,
		})
		require.NoError(t, err)
		// The gitconfig should not contain any of the stripped gitconfig values anymore.
		require.Equal(t, configWithStrippedSecrets, readConfig())
	})

	t.Run("up-to-date packfile does not get repacked", func(t *testing.T) {
		t.Parallel()

		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		// Write a commit and force-repack the whole repository. This is to ensure that the
		// repository is in a state where it shouldn't need to be repacked.
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"))
		_, err := client.OptimizeRepository(ctx, &gitalypb.OptimizeRepositoryRequest{
			Repository: repo,
			Strategy:   gitalypb.OptimizeRepositoryRequest_STRATEGY_EAGER,
		})
		require.NoError(t, err)

		// We should have a single packfile now.
		packfiles, err := filepath.Glob(filepath.Join(repoPath, "objects", "pack", "pack-*.pack"))
		require.NoError(t, err)
		require.Len(t, packfiles, 1)

		// Now we do a second, lazy optimization of the repository. This time around we
		// should see that the repository was in a well-defined state already, so we should
		// not perform any optimization.
		_, err = client.OptimizeRepository(ctx, &gitalypb.OptimizeRepositoryRequest{
			Repository: repo,
		})
		require.NoError(t, err)

		// The packfile should not have changed.
		updatedPackfiles, err := filepath.Glob(filepath.Join(repoPath, "objects", "pack", "pack-*.pack"))
		require.NoError(t, err)
		require.Equal(t, packfiles, updatedPackfiles)
	})

	t.Run("empty ref directories get pruned after grace period", func(t *testing.T) {
		t.Parallel()

		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		// Git will leave behind empty refs directories at times. In order to not slow down
		// enumerating refs we want to make sure that they get cleaned up properly.
		emptyRefsDir := filepath.Join(repoPath, "refs", "merge-requests", "1")
		require.NoError(t, os.MkdirAll(emptyRefsDir, mode.Directory))

		// But we don't expect the first call to OptimizeRepository to do anything. This is
		// because we have a grace period so that we don't delete empty ref directories that
		// have just been created by a concurrently running Git process.
		_, err := client.OptimizeRepository(ctx, &gitalypb.OptimizeRepositoryRequest{
			Repository: repo,
		})
		require.NoError(t, err)
		require.DirExists(t, emptyRefsDir)

		// Change the modification time of the complete repository to be older than a day.
		require.NoError(t, filepath.WalkDir(repoPath, func(path string, _ fs.DirEntry, err error) error {
			require.NoError(t, err)
			oneDayAgo := time.Now().Add(-24 * time.Hour)
			require.NoError(t, os.Chtimes(path, oneDayAgo, oneDayAgo))
			return nil
		}))

		// Now the second call to OptimizeRepository should indeed clean up the empty refs
		// directories.
		_, err = client.OptimizeRepository(ctx, &gitalypb.OptimizeRepositoryRequest{
			Repository: repo,
		})
		require.NoError(t, err)
		// We shouldn't have removed the top-level "refs" directory.
		require.DirExists(t, filepath.Join(repoPath, "refs"))
		// But the other two directories should be gone.
		require.NoDirExists(t, filepath.Join(repoPath, "refs", "merge-requests"))
		require.NoDirExists(t, filepath.Join(repoPath, "refs", "merge-requests", "1"))
	})
}

type mockHousekeepingManager struct {
	housekeepingmgr.Manager
	strategyCh chan housekeeping.OptimizationStrategy
}

func (m mockHousekeepingManager) OptimizeRepository(_ context.Context, _ *localrepo.Repo, opts ...housekeepingmgr.OptimizeRepositoryOption) error {
	var cfg housekeepingmgr.OptimizeRepositoryConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	m.strategyCh <- cfg.StrategyConstructor(stats.RepositoryInfo{})
	return nil
}

func TestOptimizeRepository_strategy(t *testing.T) {
	t.Parallel()

	housekeepingManager := mockHousekeepingManager{
		strategyCh: make(chan housekeeping.OptimizationStrategy, 1),
	}

	ctx := testhelper.Context(t)
	cfg, client := setupRepositoryService(t, testserver.WithHousekeepingManager(housekeepingManager))

	repoProto, _ := gittest.CreateRepository(t, ctx, cfg)

	for _, tc := range []struct {
		desc         string
		request      *gitalypb.OptimizeRepositoryRequest
		expectedType housekeeping.OptimizationStrategy
	}{
		{
			desc: "no strategy",
			request: &gitalypb.OptimizeRepositoryRequest{
				Repository: repoProto,
			},
			expectedType: housekeeping.HeuristicalOptimizationStrategy{},
		},
		{
			desc: "heuristical strategy",
			request: &gitalypb.OptimizeRepositoryRequest{
				Repository: repoProto,
				Strategy:   gitalypb.OptimizeRepositoryRequest_STRATEGY_HEURISTICAL,
			},
			expectedType: housekeeping.HeuristicalOptimizationStrategy{},
		},
		{
			desc: "eager strategy",
			request: &gitalypb.OptimizeRepositoryRequest{
				Repository: repoProto,
				Strategy:   gitalypb.OptimizeRepositoryRequest_STRATEGY_EAGER,
			},
			expectedType: housekeeping.EagerOptimizationStrategy{},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			response, err := client.OptimizeRepository(ctx, tc.request)
			require.NoError(t, err)
			testhelper.ProtoEqual(t, &gitalypb.OptimizeRepositoryResponse{}, response)

			require.IsType(t, tc.expectedType, <-housekeepingManager.strategyCh)
		})
	}
}

func TestOptimizeRepository_validation(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRepositoryService(t)
	repo, _ := gittest.CreateRepository(t, ctx, cfg)

	for _, tc := range []struct {
		desc        string
		request     *gitalypb.OptimizeRepositoryRequest
		expectedErr error
	}{
		{
			desc:        "empty repository",
			request:     &gitalypb.OptimizeRepositoryRequest{},
			expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
		},
		{
			desc: "invalid repository storage",
			request: &gitalypb.OptimizeRepositoryRequest{
				Repository: &gitalypb.Repository{
					StorageName:  "non-existent",
					RelativePath: repo.GetRelativePath(),
				},
			},
			expectedErr: testhelper.ToInterceptedMetadata(structerr.NewInvalidArgument(
				"%w", storage.NewStorageNotFoundError("non-existent"),
			)),
		},
		{
			desc: "invalid repository path",
			request: &gitalypb.OptimizeRepositoryRequest{
				Repository: &gitalypb.Repository{
					StorageName:  repo.GetStorageName(),
					RelativePath: "path/not/exist",
				},
			},
			expectedErr: testhelper.ToInterceptedMetadata(
				structerr.New("%w", storage.NewRepositoryNotFoundError(cfg.Storages[0].Name, "path/not/exist")),
			),
		},
		{
			desc: "invalid optimization strategy",
			request: &gitalypb.OptimizeRepositoryRequest{
				Repository: repo,
				Strategy:   12,
			},
			expectedErr: structerr.NewInvalidArgument("unsupported optimization strategy 12"),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := client.OptimizeRepository(ctx, tc.request)
			testhelper.RequireGrpcError(t, tc.expectedErr, err)
		})
	}
}

func TestOptimizeRepository_logStatistics(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	logger := testhelper.NewLogger(t)
	hook := testhelper.AddLoggerHook(logger)
	cfg, client := setupRepositoryService(t, testserver.WithLogger(logger))

	repoProto, _ := gittest.CreateRepository(t, ctx, cfg)
	_, err := client.OptimizeRepository(ctx, &gitalypb.OptimizeRepositoryRequest{
		Repository: repoProto,
	})
	require.NoError(t, err)

	requireRepositoryInfoLog(t, hook.AllEntries()...)
}

func TestOffloadingRepository_validation(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	for _, tc := range []struct {
		desc             string
		request          *gitalypb.OptimizeRepositoryRequest
		offloadingConfig config.Offloading
		expectedErr      error
	}{
		{
			desc: "offloading is disabled by default",
			request: &gitalypb.OptimizeRepositoryRequest{
				Strategy: gitalypb.OptimizeRepositoryRequest_STRATEGY_OFFLOADING,
			},
			expectedErr: structerr.NewUnimplemented("offloading feature not enabled"),
		},
		{
			desc: "offloading not enabled in config",
			offloadingConfig: config.Offloading{
				Enabled: false,
			},
			request: &gitalypb.OptimizeRepositoryRequest{
				Strategy: gitalypb.OptimizeRepositoryRequest_STRATEGY_OFFLOADING,
			},
			expectedErr: structerr.NewUnimplemented("offloading feature not enabled"),
		},
		{
			desc: "offloading missing sink URL",
			offloadingConfig: config.Offloading{
				Enabled:   true,
				CacheRoot: "/whatever_dir/",
			},
			request: &gitalypb.OptimizeRepositoryRequest{
				Strategy: gitalypb.OptimizeRepositoryRequest_STRATEGY_OFFLOADING,
			},
			expectedErr: structerr.NewInvalidArgument("offloading configuration missing sink URL"),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			cfg := testcfg.Build(t)
			testcfg.BuildGitalyHooks(t, cfg)
			testcfg.BuildGitalySSH(t, cfg)
			cfg.Offloading = tc.offloadingConfig
			client, serverSocketPath := runRepositoryService(t, cfg)
			cfg.SocketPath = serverSocketPath
			repo, _ := gittest.CreateRepository(t, ctx, cfg)

			tc.request.Repository = repo
			_, err := client.OptimizeRepository(ctx, tc.request)
			testhelper.RequireGrpcErrorContains(t, tc.expectedErr, err)
		})
	}
}

func TestRehydratingRepository_validation(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	for _, tc := range []struct {
		desc             string
		request          *gitalypb.OptimizeRepositoryRequest
		offloadingConfig config.Offloading
		expectedErr      error

		// offloadRepo, when set to true, configures the repository with remote.offload.url,
		// simulating a state where the repository has already been offloaded.
		offloadRepo bool
		// offloadURL is the value assigned to remote.offload.url when offloadRepo is true.
		offloadURL string
	}{
		{
			// This test cases fails on the "repository is not offloaded" check.
			// It is meant to verify that rehydration is still callable when offloading is disabled
			// in the config.
			desc: "rehydrate is still callable when offloading config is empty",
			request: &gitalypb.OptimizeRepositoryRequest{
				Strategy: gitalypb.OptimizeRepositoryRequest_STRATEGY_REHYDRATION,
			},
			expectedErr: structerr.NewInvalidArgument("offloading configuration missing sink URL"),
		},
		{
			// This test cases fails on the "repository is not offloaded" check.
			// It is meant to verify that rehydration is still callable when offloading is disabled
			// in the config.
			desc: "rehydrate is still callable when offloading feature is not enabled in config",
			offloadingConfig: config.Offloading{
				Enabled: false,
			},
			request: &gitalypb.OptimizeRepositoryRequest{
				Strategy: gitalypb.OptimizeRepositoryRequest_STRATEGY_REHYDRATION,
			},
			expectedErr: structerr.NewInvalidArgument("offloading configuration missing sink URL"),
		},
		{
			desc:        "rehydrate when missing sink URL",
			offloadRepo: true,
			offloadURL:  "fake:/s3/my_server/bucket/some/prefix",
			offloadingConfig: config.Offloading{
				Enabled:   true,
				CacheRoot: "/whatever_dir/",
			},
			request: &gitalypb.OptimizeRepositoryRequest{
				Strategy: gitalypb.OptimizeRepositoryRequest_STRATEGY_REHYDRATION,
			},
			expectedErr: structerr.NewInvalidArgument("offloading configuration missing sink URL"),
		},
		{
			desc: "rehydrate when repository is not offloaded",
			offloadingConfig: config.Offloading{
				Enabled:    true,
				CacheRoot:  "/whatever_dir/",
				GoCloudURL: "file:://tmp",
			},
			request: &gitalypb.OptimizeRepositoryRequest{
				Strategy: gitalypb.OptimizeRepositoryRequest_STRATEGY_REHYDRATION,
			},
			expectedErr: structerr.NewFailedPrecondition("repository is not offloaded"),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			cfg := testcfg.Build(t)
			testcfg.BuildGitalyHooks(t, cfg)
			testcfg.BuildGitalySSH(t, cfg)
			cfg.Offloading = tc.offloadingConfig
			client, serverSocketPath := runRepositoryService(t, cfg)
			cfg.SocketPath = serverSocketPath
			repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
			if tc.offloadRepo {
				gittest.Exec(t, cfg, "-C", repoPath, "config", "remote.offload.url", tc.offloadURL)
			}

			tc.request.Repository = repo
			_, err := client.OptimizeRepository(ctx, tc.request)
			testhelper.RequireGrpcErrorContains(t, tc.expectedErr, err)
		})
	}
}
