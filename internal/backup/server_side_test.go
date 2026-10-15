package backup_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/feder-cr/open_scion/internal/backup"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service/setup"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

func TestServerSideAdapter_Create(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	type setupData struct {
		repo             *gitalypb.Repository
		backupID         string
		backupPathPrefix string
		incremental      bool
	}

	for _, tc := range []struct {
		desc        string
		setup       func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData
		expectedErr error
	}{
		{
			desc: "success",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

				return setupData{
					repo:     repo,
					backupID: "abc123",
				}
			},
		},
		{
			desc: "success with path prefix",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

				return setupData{
					repo:             repo,
					backupID:         "abc123",
					backupPathPrefix: "foo",
				}
			},
		},
		{
			desc: "success - incremental",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

				return setupData{
					repo:        repo,
					backupID:    "abc123",
					incremental: true,
				}
			},
		},
		{
			desc: "missing backup ID",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

				return setupData{
					repo:     repo,
					backupID: "",
				}
			},
			expectedErr: structerr.NewInvalidArgument("server-side create: rpc error: code = InvalidArgument desc = empty BackupId"),
		},
		{
			desc: "success - repository with no branches",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, _ := gittest.CreateRepository(t, ctx, cfg)

				return setupData{
					repo:     repo,
					backupID: "abc123",
				}
			},
		},
		{
			desc: "success - repository does not exist",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo := &gitalypb.Repository{
					StorageName:  cfg.Storages[0].Name,
					RelativePath: gittest.NewRepositoryName(t),
				}

				return setupData{
					repo:     repo,
					backupID: "abc123",
				}
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			backupRoot := testhelper.TempDir(t)
			backupSink, err := backup.ResolveSink(ctx, backupRoot)
			require.NoError(t, err)

			backupLocator := backup.NewLocator(backupSink)

			cfg := testcfg.Build(t)
			cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
				testserver.WithBackupSink(backupSink),
				testserver.WithBackupLocator(backupLocator),
			)

			pool := client.NewPool()
			defer testhelper.MustClose(t, pool)

			adapter := backup.NewServerSideAdapter(pool)

			data := tc.setup(t, ctx, cfg)

			ctx := testhelper.MergeIncomingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))

			err = adapter.Create(ctx, &backup.CreateRequest{
				Repository:       data.repo,
				VanityRepository: data.repo,
				BackupID:         data.backupID,
				PathPrefix:       data.backupPathPrefix,
				Incremental:      data.incremental,
			})
			if tc.expectedErr != nil {
				testhelper.RequireGrpcError(t, tc.expectedErr, err)
				return
			}

			require.NoError(t, err)
		})
	}
}

func TestServerSideAdapter_Restore(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	type setupData struct {
		repo             *gitalypb.Repository
		repoPath         string
		backupID         string
		useLatest        bool
		backupPathPrefix string
		expectedChecksum *git.Checksum
	}

	for _, tc := range []struct {
		desc        string
		setup       func(t *testing.T, ctx context.Context, cfg config.Cfg, backupSink *backup.Sink, backupLocator backup.Locator) setupData
		expectedErr error
	}{
		{
			desc: "success",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, backupSink *backup.Sink, backupLocator backup.Locator) setupData {
				_, templateRepoPath := gittest.CreateRepository(t, ctx, cfg)
				oid := gittest.WriteCommit(t, cfg, templateRepoPath, gittest.WithBranch(git.DefaultBranch))
				gittest.WriteCommit(t, cfg, templateRepoPath, gittest.WithBranch("feature"), gittest.WithParents(oid))
				checksum := gittest.ChecksumRepo(t, cfg, templateRepoPath)

				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
				backupData := backupLocator.BeginFull(ctx, repo, "abc123")
				step := backupData.Steps[len(backupData.Steps)-1]

				w, err := backupSink.GetWriter(ctx, step.BundlePath)
				require.NoError(t, err)
				bundle := gittest.BundleRepo(t, cfg, templateRepoPath, "-")
				_, err = w.Write(bundle)
				require.NoError(t, err)
				require.NoError(t, w.Close())

				w, err = backupSink.GetWriter(ctx, step.RefPath)
				require.NoError(t, err)
				refs := gittest.Exec(t, cfg, "-C", templateRepoPath, "show-ref", "--head")
				_, err = w.Write(refs)
				require.NoError(t, err)
				require.NoError(t, w.Close())

				backupData.ObjectFormat = gittest.DefaultObjectHash.Format

				require.NoError(t, backupLocator.Commit(ctx, backupData))

				return setupData{
					repo:             repo,
					repoPath:         repoPath,
					backupID:         "abc123",
					expectedChecksum: checksum,
				}
			},
		},
		{
			desc: "success with path prefix",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, backupSink *backup.Sink, backupLocator backup.Locator) setupData {
				_, templateRepoPath := gittest.CreateRepository(t, ctx, cfg)
				oid := gittest.WriteCommit(t, cfg, templateRepoPath, gittest.WithBranch(git.DefaultBranch))
				gittest.WriteCommit(t, cfg, templateRepoPath, gittest.WithBranch("feature"), gittest.WithParents(oid))
				checksum := gittest.ChecksumRepo(t, cfg, templateRepoPath)

				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
				backupData := backupLocator.BeginFull(ctx, repo, "abc123", backup.WithPathPrefix("foo"))
				step := backupData.Steps[len(backupData.Steps)-1]

				w, err := backupSink.GetWriter(ctx, step.BundlePath)
				require.NoError(t, err)
				bundle := gittest.BundleRepo(t, cfg, templateRepoPath, "-")
				_, err = w.Write(bundle)
				require.NoError(t, err)
				require.NoError(t, w.Close())

				w, err = backupSink.GetWriter(ctx, step.RefPath)
				require.NoError(t, err)
				refs := gittest.Exec(t, cfg, "-C", templateRepoPath, "show-ref", "--head")
				_, err = w.Write(refs)
				require.NoError(t, err)
				require.NoError(t, w.Close())

				backupData.ObjectFormat = gittest.DefaultObjectHash.Format

				require.NoError(t, backupLocator.Commit(ctx, backupData, backup.WithPathPrefix("foo")))

				return setupData{
					repo:             repo,
					repoPath:         repoPath,
					backupID:         "abc123",
					backupPathPrefix: "foo",
					expectedChecksum: checksum,
				}
			},
		},
		{
			desc: "missing bundle",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, backupSink *backup.Sink, backupLocator backup.Locator) setupData {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					RelativePath: "@test/restore/latest/missing.git",
				})

				return setupData{
					repo:     repo,
					repoPath: repoPath,
					backupID: "",
				}
			},
			expectedErr: structerr.NewInternal("server-side restore: restore repository: manifest: find latest: doesn't exist"),
		},
		{
			desc: "UseLatest falls back to latest when backup ID not found",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg, backupSink *backup.Sink, backupLocator backup.Locator) setupData {
				_, templateRepoPath := gittest.CreateRepository(t, ctx, cfg)
				oid := gittest.WriteCommit(t, cfg, templateRepoPath, gittest.WithBranch(git.DefaultBranch))
				gittest.WriteCommit(t, cfg, templateRepoPath, gittest.WithBranch("feature"), gittest.WithParents(oid))
				checksum := gittest.ChecksumRepo(t, cfg, templateRepoPath)

				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
				bkp := backupLocator.BeginFull(ctx, repo, "existing123")
				step := bkp.Steps[len(bkp.Steps)-1]

				w, err := backupSink.GetWriter(ctx, step.BundlePath)
				require.NoError(t, err)
				bundle := gittest.BundleRepo(t, cfg, templateRepoPath, "-")
				_, err = w.Write(bundle)
				require.NoError(t, err)
				require.NoError(t, w.Close())

				w, err = backupSink.GetWriter(ctx, step.RefPath)
				require.NoError(t, err)
				refs := gittest.Exec(t, cfg, "-C", templateRepoPath, "show-ref", "--head")
				_, err = w.Write(refs)
				require.NoError(t, err)
				require.NoError(t, w.Close())

				bkp.ObjectFormat = gittest.DefaultObjectHash.Format

				require.NoError(t, backupLocator.Commit(ctx, bkp))

				return setupData{
					repo:             repo,
					repoPath:         repoPath,
					backupID:         "nonexistent456",
					useLatest:        true,
					expectedChecksum: checksum,
				}
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			backupRoot := testhelper.TempDir(t)
			backupSink, err := backup.ResolveSink(ctx, backupRoot)
			require.NoError(t, err)

			backupLocator := backup.NewLocator(backupSink)

			cfg := testcfg.Build(t)
			cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
				testserver.WithBackupSink(backupSink),
				testserver.WithBackupLocator(backupLocator),
			)

			pool := client.NewPool()
			defer testhelper.MustClose(t, pool)

			adapter := backup.NewServerSideAdapter(pool)

			data := tc.setup(t, ctx, cfg, backupSink, backupLocator)

			ctx := testhelper.MergeIncomingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))

			err = adapter.Restore(ctx, &backup.RestoreRequest{
				Repository:       data.repo,
				VanityRepository: data.repo,
				BackupID:         data.backupID,
				UseLatest:        data.useLatest,
				PathPrefix:       data.backupPathPrefix,
			})
			if tc.expectedErr != nil {
				testhelper.RequireGrpcError(t, tc.expectedErr, err)
				return
			}

			require.NoError(t, err)
		})
	}
}

func TestServerSideAdapter_ReadLatestBackupID(t *testing.T) {
	t.Parallel()

	// Each storage in a test case is backed by one of these servers. The
	// storages are named so that they sort in the order they're listed.
	type serverKind int
	const (
		// withoutSink is a server with no backup sink. It fails the RPC with
		// FailedPrecondition.
		withoutSink serverKind = iota
		// unreachable is an address that no server listens on.
		unreachable
		// emptySink is a server whose backup sink has no backup ID markers.
		emptySink
		// markedSink is a server whose backup sink has a backup ID marker.
		markedSink
	)

	for _, tc := range []struct {
		desc        string
		servers     []serverKind
		expectedID  string
		expectedErr error
	}{
		{
			desc:       "skips a storage whose server has no backup sink",
			servers:    []serverKind{withoutSink, markedSink},
			expectedID: "the-backup",
		},
		{
			desc:       "skips a storage whose server is unreachable",
			servers:    []serverKind{unreachable, markedSink},
			expectedID: "the-backup",
		},
		{
			desc:        "stops at the first storage without backup ID markers",
			servers:     []serverKind{withoutSink, emptySink, markedSink},
			expectedErr: backup.ErrDoesntExist,
		},
		{
			desc:    "every storage fails",
			servers: []serverKind{withoutSink},
			expectedErr: structerr.NewFailedPrecondition(
				`server-side read latest backup id: storage "storage-0": read latest backup ID: server-side backups are not configured`,
			),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			ctx := testhelper.Context(t)

			servers := storage.GitalyServers{}
			for i, kind := range tc.servers {
				storageName := fmt.Sprintf("storage-%d", i)

				var opts []testserver.GitalyServerOpt
				switch kind {
				case unreachable:
					servers[storageName] = storage.ServerInfo{Address: "unix://" + testhelper.GetTemporaryGitalySocketFileName(t)}
					continue
				case emptySink, markedSink:
					backupSink, err := backup.ResolveSink(ctx, testhelper.TempDir(t))
					require.NoError(t, err)

					if kind == markedSink {
						require.NoError(t, backup.NewIDManager(backupSink).WriteBackupID(ctx, &backup.WriteBackupIDRequest{BackupID: "the-backup"}))
					}

					opts = append(opts, testserver.WithBackupSink(backupSink))
				}

				cfg := testcfg.Build(t, testcfg.WithStorages(storageName))
				cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll, opts...)
				servers[storageName] = storage.ServerInfo{Address: cfg.SocketPath, Token: cfg.Auth.Token}
			}

			serversJSON, err := json.Marshal(servers)
			require.NoError(t, err)
			ctx = testhelper.MergeIncomingMetadata(ctx, metadata.Pairs("gitaly-servers", base64.StdEncoding.EncodeToString(serversJSON)))

			pool := client.NewPool()
			defer testhelper.MustClose(t, pool)

			backupID, err := backup.NewServerSideAdapter(pool).ReadLatestBackupID(ctx, "")
			if tc.expectedErr != nil {
				if errors.Is(tc.expectedErr, backup.ErrDoesntExist) {
					require.ErrorIs(t, err, backup.ErrDoesntExist)
				} else {
					testhelper.RequireGrpcError(t, tc.expectedErr, err)
				}
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectedID, backupID)
		})
	}
}
