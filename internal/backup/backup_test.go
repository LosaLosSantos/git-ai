package backup_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/archive"
	"github.com/feder-cr/git-ai/internal/backup"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/counter"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/storagemgr/partition/migration"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestManager_Create(t *testing.T) {
	t.Parallel()

	const backupID = "abc123"

	cfg := testcfg.Build(t)

	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll)

	ctx := testhelper.Context(t)
	repoCounter := counter.NewRepositoryCounter(cfg.Storages)

	for _, managerTC := range []struct {
		desc  string
		setup func(t testing.TB, sink *backup.Sink, locator backup.Locator) *backup.Manager
	}{
		{
			desc: "RPC manager",
			setup: func(tb testing.TB, sink *backup.Sink, locator backup.Locator) *backup.Manager {
				pool := client.NewPool()
				tb.Cleanup(func() {
					testhelper.MustClose(tb, pool)
				})

				return backup.NewManager(sink, testhelper.SharedLogger(t), locator, pool)
			},
		},
		{
			desc: "Local manager",
			setup: func(tb testing.TB, sink *backup.Sink, locator backup.Locator) *backup.Manager {
				if testhelper.IsPraefectEnabled() {
					tb.Skip("local backup manager expects to operate on the local filesystem so cannot operate through praefect")
				}

				storageLocator := config.NewLocator(cfg)
				gitCmdFactory := gittest.NewCommandFactory(tb, cfg)
				catfileCache := catfile.NewCache(cfg)
				tb.Cleanup(catfileCache.Stop)
				txManager := transaction.NewTrackingManager()
				migrationStateManager := migration.NewStateManager(&[]migration.Migration{})

				return backup.NewManagerLocal(
					sink,
					testhelper.SharedLogger(t),
					locator,
					storageLocator,
					gitCmdFactory,
					catfileCache,
					txManager,
					repoCounter,
					migrationStateManager,
				)
			},
		},
	} {

		type setupData struct {
			repo             *gitalypb.Repository
			repoPath         string
			backupPathPrefix string
			expectedBackup   *backup.Backup
		}

		for _, tc := range []struct {
			desc               string
			setup              func(tb testing.TB, vanityRepo storage.Repository) setupData
			createsRefList     bool
			createsBundle      bool
			createsCustomHooks bool
			err                error
		}{
			{
				desc: "no hooks",
				setup: func(tb testing.TB, vanityRepo storage.Repository) setupData {
					repo, repoPath := gittest.CreateRepository(tb, ctx, cfg)
					gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

					return setupData{
						repo:     repo,
						repoPath: repoPath,
						expectedBackup: &backup.Backup{
							ID:            backupID,
							Repository:    vanityRepo,
							ObjectFormat:  gittest.DefaultObjectHash.Format,
							HeadReference: git.DefaultRef.String(),
							Steps: []backup.Step{
								{
									BundlePath:      joinBackupPath(t, "", "", vanityRepo, backupID, "001.bundle"),
									RefPath:         joinBackupPath(t, "", "", vanityRepo, backupID, "001.refs"),
									CustomHooksPath: joinBackupPath(t, "", "", vanityRepo, backupID, "001.custom_hooks.tar"),
								},
							},
						},
					}
				},
				createsRefList:     true,
				createsBundle:      true,
				createsCustomHooks: false,
			},
			{
				desc: "hooks",
				setup: func(tb testing.TB, vanityRepo storage.Repository) setupData {
					repo, repoPath := gittest.CreateRepository(tb, ctx, cfg)
					gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
					require.NoError(tb, os.Mkdir(filepath.Join(repoPath, "custom_hooks"), mode.Directory))
					require.NoError(tb, os.WriteFile(filepath.Join(repoPath, "custom_hooks/pre-commit.sample"), []byte("Some hooks"), mode.File))

					return setupData{
						repo:     repo,
						repoPath: repoPath,
						expectedBackup: &backup.Backup{
							ID:            backupID,
							Repository:    vanityRepo,
							ObjectFormat:  gittest.DefaultObjectHash.Format,
							HeadReference: git.DefaultRef.String(),
							Steps: []backup.Step{
								{
									BundlePath:      joinBackupPath(t, "", "", vanityRepo, backupID, "001.bundle"),
									RefPath:         joinBackupPath(t, "", "", vanityRepo, backupID, "001.refs"),
									CustomHooksPath: joinBackupPath(t, "", "", vanityRepo, backupID, "001.custom_hooks.tar"),
								},
							},
						},
					}
				},
				createsRefList:     true,
				createsBundle:      true,
				createsCustomHooks: true,
			},
			{
				desc: "empty repo",
				setup: func(tb testing.TB, vanityRepo storage.Repository) setupData {
					emptyRepo, repoPath := gittest.CreateRepository(tb, ctx, cfg)

					return setupData{
						repo:     emptyRepo,
						repoPath: repoPath,
						expectedBackup: &backup.Backup{
							ID:            backupID,
							Repository:    vanityRepo,
							Empty:         true,
							ObjectFormat:  gittest.DefaultObjectHash.Format,
							HeadReference: git.DefaultRef.String(),
							Steps: []backup.Step{
								{
									BundlePath:      joinBackupPath(t, "", "", vanityRepo, backupID, "001.bundle"),
									RefPath:         joinBackupPath(t, "", "", vanityRepo, backupID, "001.refs"),
									CustomHooksPath: joinBackupPath(t, "", "", vanityRepo, backupID, "001.custom_hooks.tar"),
								},
							},
						},
					}
				},
				createsRefList:     true,
				createsBundle:      false,
				createsCustomHooks: false,
			},
			{
				desc: "nonexistent repo",
				setup: func(tb testing.TB, vanityRepo storage.Repository) setupData {
					emptyRepo, repoPath := gittest.CreateRepository(tb, ctx, cfg)
					nonexistentRepo := proto.Clone(emptyRepo).(*gitalypb.Repository)
					nonexistentRepo.RelativePath = gittest.NewRepositoryName(t)

					return setupData{
						repo:     nonexistentRepo,
						repoPath: repoPath,
						expectedBackup: &backup.Backup{
							ID:          backupID,
							Repository:  vanityRepo,
							NonExistent: true,
							Empty:       true,
							Steps: []backup.Step{
								{
									BundlePath:      joinBackupPath(t, "", "", vanityRepo, backupID, "001.bundle"),
									RefPath:         joinBackupPath(t, "", "", vanityRepo, backupID, "001.refs"),
									CustomHooksPath: joinBackupPath(t, "", "", vanityRepo, backupID, "001.custom_hooks.tar"),
								},
							},
						},
					}
				},
				createsRefList:     false,
				createsBundle:      false,
				createsCustomHooks: false,
			},
			{
				desc: "with backup path prefix",
				setup: func(tb testing.TB, vanityRepo storage.Repository) setupData {
					repo, repoPath := gittest.CreateRepository(tb, ctx, cfg)
					gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
					require.NoError(tb, os.Mkdir(filepath.Join(repoPath, "custom_hooks"), mode.Directory))
					require.NoError(tb, os.WriteFile(filepath.Join(repoPath, "custom_hooks/pre-commit.sample"), []byte("Some hooks"), mode.File))

					return setupData{
						repo:             repo,
						repoPath:         repoPath,
						backupPathPrefix: "foo",
						expectedBackup: &backup.Backup{
							ID:            backupID,
							Repository:    vanityRepo,
							ObjectFormat:  gittest.DefaultObjectHash.Format,
							HeadReference: git.DefaultRef.String(),
							Steps: []backup.Step{
								{
									BundlePath:      joinBackupPath(t, "", "foo", vanityRepo, backupID, "001.bundle"),
									RefPath:         joinBackupPath(t, "", "foo", vanityRepo, backupID, "001.refs"),
									CustomHooksPath: joinBackupPath(t, "", "foo", vanityRepo, backupID, "001.custom_hooks.tar"),
								},
							},
						},
					}
				},
				createsRefList:     true,
				createsBundle:      true,
				createsCustomHooks: true,
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				backupRoot := testhelper.TempDir(t)
				vanityRepo := &gitalypb.Repository{
					RelativePath: "some/path.git",
					StorageName:  "some_storage",
				}

				data := tc.setup(t, vanityRepo)

				manifestPath := filepath.Join(backupRoot, data.backupPathPrefix, "manifests", vanityRepo.GetStorageName(), vanityRepo.GetRelativePath(), backupID+".toml")
				refsPath := joinBackupPath(t, backupRoot, data.backupPathPrefix, vanityRepo, backupID, "001.refs")
				bundlePath := joinBackupPath(t, backupRoot, data.backupPathPrefix, vanityRepo, backupID, "001.bundle")
				customHooksPath := joinBackupPath(t, backupRoot, data.backupPathPrefix, vanityRepo, backupID, "001.custom_hooks.tar")

				sink, err := backup.ResolveSink(ctx, backupRoot)
				require.NoError(t, err)
				defer testhelper.MustClose(t, sink)

				locator := backup.NewLocator(sink)

				fsBackup := managerTC.setup(t, sink, locator)
				err = fsBackup.Create(ctx, &backup.CreateRequest{
					Server:           storage.ServerInfo{Address: cfg.SocketPath, Token: cfg.Auth.Token},
					Repository:       data.repo,
					VanityRepository: vanityRepo,
					BackupID:         backupID,
					PathPrefix:       data.backupPathPrefix,
				})
				if tc.err == nil {
					require.NoError(t, err)
				} else {
					require.Equal(t, tc.err, err)
				}

				require.FileExists(t, manifestPath)

				if tc.createsBundle {
					require.FileExists(t, refsPath)
					require.FileExists(t, bundlePath)

					output := gittest.Exec(t, cfg, "-C", data.repoPath, "bundle", "verify", bundlePath)
					require.Contains(t, string(output), "The bundle records a complete history")

					expectedRefs := gittest.Exec(t, cfg, "-C", data.repoPath, "show-ref", "--head")
					actualRefs := testhelper.MustReadFile(t, refsPath)
					require.Equal(t, string(expectedRefs), string(actualRefs))
				} else {
					require.NoFileExists(t, bundlePath)
				}

				if tc.createsRefList {
					require.FileExists(t, refsPath)
				} else {
					require.NoFileExists(t, refsPath)
				}

				if tc.createsCustomHooks {
					require.FileExists(t, customHooksPath)
				} else {
					require.NoFileExists(t, customHooksPath)
				}

				backupData, err := locator.Find(ctx, vanityRepo, backupID, backup.WithPathPrefix(data.backupPathPrefix))
				if data.expectedBackup == nil {
					require.ErrorIs(t, err, backup.ErrDoesntExist)
				} else {
					require.NoError(t, err)
					require.Equal(t, data.expectedBackup, backupData)
				}
			})
		}
	}
}

func TestManager_Create_incremental(t *testing.T) {
	t.Parallel()

	const backupID = "abc123"

	cfg := testcfg.Build(t)

	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll)
	ctx := testhelper.Context(t)
	repoCounter := counter.NewRepositoryCounter(cfg.Storages)

	for _, managerTC := range []struct {
		desc  string
		setup func(t testing.TB, sink *backup.Sink, locator backup.Locator) *backup.Manager
	}{
		{
			desc: "RPC manager",
			setup: func(tb testing.TB, sink *backup.Sink, locator backup.Locator) *backup.Manager {
				pool := client.NewPool()
				tb.Cleanup(func() {
					testhelper.MustClose(tb, pool)
				})

				return backup.NewManager(sink, testhelper.SharedLogger(t), locator, pool)
			},
		},
		{
			desc: "Local manager",
			setup: func(tb testing.TB, sink *backup.Sink, locator backup.Locator) *backup.Manager {
				if testhelper.IsPraefectEnabled() {
					tb.Skip("local backup manager expects to operate on the local filesystem so cannot operate through praefect")
				}

				storageLocator := config.NewLocator(cfg)
				gitCmdFactory := gittest.NewCommandFactory(tb, cfg)
				catfileCache := catfile.NewCache(cfg)
				tb.Cleanup(catfileCache.Stop)
				txManager := transaction.NewTrackingManager()
				migrationStateManager := migration.NewStateManager(&[]migration.Migration{})

				return backup.NewManagerLocal(sink, testhelper.SharedLogger(t), locator, storageLocator, gitCmdFactory, catfileCache, txManager, repoCounter, migrationStateManager)
			},
		},
	} {
		for _, tc := range []struct {
			desc              string
			setup             func(t testing.TB, backupRoot string) (*gitalypb.Repository, string, string)
			expectedIncrement string
			expectedErr       error
		}{
			{
				desc: "no previous backup",
				setup: func(tb testing.TB, backupRoot string) (*gitalypb.Repository, string, string) {
					repo, repoPath := gittest.CreateRepository(tb, ctx, cfg)
					gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
					return repo, repoPath, ""
				},
				expectedIncrement: "001",
			},
			{
				desc: "previous backup, no updates",
				setup: func(tb testing.TB, backupRoot string) (*gitalypb.Repository, string, string) {
					repo, repoPath := gittest.CreateRepository(tb, ctx, cfg)
					gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

					testhelper.WriteFiles(tb, backupRoot, map[string]any{
						filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), "abc123.toml"): fmt.Sprintf(`
object_format = '%[1]s'

[[steps]]
bundle_path = '%[2]s/001.bundle'
ref_path = '%[2]s/001.refs'
custom_hooks_path = '%[2]s/001.custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, repo.GetRelativePath()),
						filepath.Join(repo.GetRelativePath(), "001.bundle"): gittest.Exec(tb, cfg, "-C", repoPath, "bundle", "create", "-", "--all"),
						filepath.Join(repo.GetRelativePath(), "001.refs"):   gittest.Exec(tb, cfg, "-C", repoPath, "show-ref", "--head"),
					})

					return repo, repoPath, ""
				},
				expectedIncrement: "002",
			},
			{
				desc: "previous backup, updates",
				setup: func(tb testing.TB, backupRoot string) (*gitalypb.Repository, string, string) {
					repo, repoPath := gittest.CreateRepository(tb, ctx, cfg)
					commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

					testhelper.WriteFiles(tb, backupRoot, map[string]any{
						filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), "abc123.toml"): fmt.Sprintf(`
object_format = '%[1]s'

[[steps]]
bundle_path = '%[2]s/001.bundle'
ref_path = '%[2]s/001.refs'
custom_hooks_path = '%[2]s/001.custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, repo.GetRelativePath()),
						filepath.Join(repo.GetRelativePath(), "001.bundle"): gittest.Exec(tb, cfg, "-C", repoPath, "bundle", "create", "-", "--all"),
						filepath.Join(repo.GetRelativePath(), "001.refs"):   gittest.Exec(tb, cfg, "-C", repoPath, "show-ref", "--head"),
					})

					gittest.WriteCommit(tb, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithParents(commitID))

					return repo, repoPath, ""
				},
				expectedIncrement: "002",
			},
			{
				desc: "previous backup, updates and with backup path prefix",
				setup: func(tb testing.TB, backupRoot string) (*gitalypb.Repository, string, string) {
					repo, repoPath := gittest.CreateRepository(tb, ctx, cfg)
					commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

					testhelper.WriteFiles(tb, backupRoot, map[string]any{
						filepath.Join("foo", "manifests", repo.GetStorageName(), repo.GetRelativePath(), "abc123.toml"): fmt.Sprintf(`
object_format = '%[1]s'

[[steps]]
bundle_path = '%[2]s/001.bundle'
ref_path = '%[2]s/001.refs'
custom_hooks_path = '%[2]s/001.custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, repo.GetRelativePath()),
						filepath.Join(repo.GetRelativePath(), "001.bundle"): gittest.Exec(tb, cfg, "-C", repoPath, "bundle", "create", "-", "--all"),
						filepath.Join(repo.GetRelativePath(), "001.refs"):   gittest.Exec(tb, cfg, "-C", repoPath, "show-ref", "--head"),
					})

					gittest.WriteCommit(tb, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithParents(commitID))

					return repo, repoPath, "foo"
				},
				expectedIncrement: "002",
			},
			{
				// Regression test for an incremental backup failing when the
				// previous backup recorded the repository as non-existent (it
				// existed in the database but had no commits). Such a backup
				// writes a manifest with non_existent = true but never writes a
				// ref blob. Once the repository receives commits, the next
				// incremental must fall back to a full backup instead of
				// failing on the missing previous ref.
				desc: "previous backup non-existent, repository now initialised",
				setup: func(tb testing.TB, backupRoot string) (*gitalypb.Repository, string, string) {
					repo, repoPath := gittest.CreateRepository(tb, ctx, cfg)
					gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

					// Only the manifest is written, mirroring a backup of an
					// uninitialised repository. Crucially, no 001.refs/001.bundle
					// blobs exist for the previous step.
					testhelper.WriteFiles(tb, backupRoot, map[string]any{
						filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), "abc123.toml"): fmt.Sprintf(`
object_format = '%[1]s'
empty = true
non_existent = true

[[steps]]
bundle_path = '%[2]s/001.bundle'
ref_path = '%[2]s/001.refs'
custom_hooks_path = '%[2]s/001.custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, repo.GetRelativePath()),
					})

					return repo, repoPath, ""
				},
				expectedIncrement: "001",
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				backupRoot := testhelper.TempDir(t)
				repo, repoPath, backupPathPrefix := tc.setup(t, backupRoot)

				refsPath := joinBackupPath(t, backupRoot, backupPathPrefix, repo, backupID, tc.expectedIncrement+".refs")
				bundlePath := joinBackupPath(t, backupRoot, backupPathPrefix, repo, backupID, tc.expectedIncrement+".bundle")

				sink, err := backup.ResolveSink(ctx, backupRoot)
				require.NoError(t, err)
				defer testhelper.MustClose(t, sink)

				locator := backup.NewLocator(sink)

				fsBackup := managerTC.setup(t, sink, locator)
				err = fsBackup.Create(ctx, &backup.CreateRequest{
					Server:      storage.ServerInfo{Address: cfg.SocketPath, Token: cfg.Auth.Token},
					Repository:  repo,
					Incremental: true,
					BackupID:    backupID,
					PathPrefix:  backupPathPrefix,
				})
				if tc.expectedErr == nil {
					require.NoError(t, err)
				} else {
					require.Equal(t, tc.expectedErr, err)
					return
				}

				require.FileExists(t, refsPath)
				require.FileExists(t, bundlePath)

				expectedRefs := gittest.Exec(t, cfg, "-C", repoPath, "show-ref", "--head")
				actualRefs := testhelper.MustReadFile(t, refsPath)
				require.Equal(t, string(expectedRefs), string(actualRefs))
			})
		}
	}
}

func TestManager_Restore_latest(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	testcfg.BuildGitalyHooks(t, cfg)
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll)
	repoCounter := counter.NewRepositoryCounter(cfg.Storages)

	for _, managerTC := range []struct {
		desc  string
		setup func(t testing.TB, sink *backup.Sink, locator backup.Locator, logger log.Logger) *backup.Manager
	}{
		{
			desc: "RPC manager",
			setup: func(tb testing.TB, sink *backup.Sink, locator backup.Locator, logger log.Logger) *backup.Manager {
				pool := client.NewPool()
				tb.Cleanup(func() {
					testhelper.MustClose(tb, pool)
				})

				return backup.NewManager(sink, logger, locator, pool)
			},
		},
		{
			desc: "Local manager",
			setup: func(tb testing.TB, sink *backup.Sink, locator backup.Locator, logger log.Logger) *backup.Manager {
				if testhelper.IsPraefectEnabled() {
					tb.Skip("local backup manager expects to operate on the local filesystem so cannot operate through praefect")
				}

				storageLocator := config.NewLocator(cfg)
				gitCmdFactory := gittest.NewCommandFactory(tb, cfg)
				catfileCache := catfile.NewCache(cfg)
				tb.Cleanup(catfileCache.Stop)
				txManager := transaction.NewTrackingManager()
				migrationStateManager := migration.NewStateManager(&[]migration.Migration{})

				return backup.NewManagerLocal(sink, logger, locator, storageLocator, gitCmdFactory, catfileCache, txManager, repoCounter, migrationStateManager)
			},
		},
	} {
		t.Run(managerTC.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)

			cc, err := client.New(ctx, cfg.SocketPath)
			require.NoError(t, err)
			defer testhelper.MustClose(t, cc)

			repoClient := gitalypb.NewRepositoryServiceClient(cc)

			backupRoot := testhelper.TempDir(t)

			for _, tc := range []struct {
				desc          string
				setup         func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string)
				alwaysCreate  bool
				expectExists  bool
				expectedPaths []string
				expectedErrAs error
			}{
				{
					desc: "non-optimised",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						_, repoPath, _ := createAndSeedRepository(t, ctx, cfg)
						repoChecksum, repoBundle, repoRefs := createBackupArtifacts(t, cfg, repoPath)

						// Restoring into an empty repo, so ref reset won't work.
						repo, _ := gittest.CreateRepository(t, ctx, cfg)

						relativePath := stripRelativePath(tb, repo)
						testhelper.WriteFiles(tb, backupRoot, map[string]any{
							filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), "abc123.toml"): fmt.Sprintf(`
object_format = '%[1]s'
head_reference = '%[3]s'

[[steps]]
bundle_path = '%[2]s.bundle'
ref_path = '%[2]s.refs'
custom_hooks_path = '%[2]s/custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, relativePath, git.DefaultRef.String()),
							relativePath + ".bundle": repoBundle,
							relativePath + ".refs":   repoRefs,
						})

						require.NoError(tb, os.MkdirAll(filepath.Join(backupRoot, relativePath), mode.Directory))

						return repo, repoChecksum, ""
					},
					expectExists: true,
				},
				{
					desc: "existing repo, without hooks",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						repo, repoPath, head := createAndSeedRepository(t, ctx, cfg)
						repoChecksum, repoBundle, repoRefs := createBackupArtifacts(t, cfg, repoPath)
						gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(head), gittest.WithBranch("main"))

						relativePath := stripRelativePath(tb, repo)
						testhelper.WriteFiles(tb, backupRoot, map[string]any{
							filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), "abc123.toml"): fmt.Sprintf(`
object_format = '%[1]s'
head_reference = '%[3]s'

[[steps]]
bundle_path = '%[2]s.bundle'
ref_path = '%[2]s.refs'
custom_hooks_path = '%[2]s/custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, relativePath, git.DefaultRef.String()),
							relativePath + ".bundle": repoBundle,
							relativePath + ".refs":   repoRefs,
						})

						return repo, repoChecksum, ""
					},
					expectExists: true,
				},
				{
					desc: "existing repo, without backup path prefix",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						repo, repoPath, head := createAndSeedRepository(t, ctx, cfg)
						repoChecksum, repoBundle, repoRefs := createBackupArtifacts(t, cfg, repoPath)
						gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(head), gittest.WithBranch("main"))

						relativePath := stripRelativePath(tb, repo)
						testhelper.WriteFiles(tb, backupRoot, map[string]any{
							filepath.Join("foo", "manifests", repo.GetStorageName(), repo.GetRelativePath(), "abc123.toml"): fmt.Sprintf(`
object_format = '%[1]s'
head_reference = '%[3]s'

[[steps]]
bundle_path = '%[2]s.bundle'
ref_path = '%[2]s.refs'
custom_hooks_path = '%[2]s/custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, relativePath, git.DefaultRef.String()),
							relativePath + ".bundle": repoBundle,
							relativePath + ".refs":   repoRefs,
						})

						return repo, repoChecksum, "foo"
					},
					expectExists: true,
				},
				{
					desc: "existing repo, with hooks",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						repo, repoPath, head := createAndSeedRepository(t, ctx, cfg)
						repoChecksum, repoBundle, repoRefs := createBackupArtifacts(t, cfg, repoPath)
						gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(head), gittest.WithBranch("main"))

						relativePath := stripRelativePath(tb, repo)
						customHooksPath := filepath.Join(backupRoot, relativePath, "custom_hooks.tar")
						testhelper.WriteFiles(tb, backupRoot, map[string]any{
							filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), "abc123.toml"): fmt.Sprintf(`
object_format = '%[1]s'
head_reference = '%[3]s'

[[steps]]
bundle_path = '%[2]s.bundle'
ref_path = '%[2]s.refs'
custom_hooks_path = '%[2]s/custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, relativePath, git.DefaultRef.String()),
							relativePath + ".bundle": repoBundle,
							relativePath + ".refs":   repoRefs,
						})

						require.NoError(tb, os.MkdirAll(filepath.Join(backupRoot, relativePath), mode.Directory))
						testhelper.CopyFile(tb, mustCreateCustomHooksArchive(t, ctx), customHooksPath)

						return repo, repoChecksum, ""
					},
					expectedPaths: []string{
						"custom_hooks/pre-commit.sample",
						"custom_hooks/prepare-commit-msg.sample",
						"custom_hooks/pre-push.sample",
					},
					expectExists: true,
				},
				{
					desc: "missing backup",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						repo, _ := gittest.CreateRepository(t, ctx, cfg)
						return repo, nil, ""
					},
					expectedErrAs: backup.ErrSkipped,
				},
				{
					desc: "empty backup",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						repo, _ := gittest.CreateRepository(t, ctx, cfg)

						relativePath := stripRelativePath(tb, repo)
						testhelper.WriteFiles(tb, backupRoot, map[string]any{
							filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), "abc123.toml"): fmt.Sprintf(`
object_format = '%[1]s'
empty = true
non_existent= false
head_reference = '%[3]s'

[[steps]]
ref_path = '%[2]s.refs'
custom_hooks_path = '%[2]s/custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, relativePath, git.DefaultRef.String()),
							relativePath + ".refs": "",
						})

						return repo, new(git.Checksum), ""
					},
					expectExists: true,
				},
				{
					desc: "empty repository with custom hooks",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						repo, _ := gittest.CreateRepository(tb, ctx, cfg)
						const backupID = "abc123"

						customHooksArchive := mustCreateCustomHooksArchive(t, ctx)

						relativePath := stripRelativePath(tb, repo)
						testhelper.WriteFiles(tb, backupRoot, map[string]any{
							filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), "abc123.toml"): fmt.Sprintf(`
				empty = true
				non_existent = false
				object_format = %q
				head_reference = 'refs/heads/main'

				[[steps]]
				bundle_path = '%s/%s/001.bundle'
				ref_path = '%s/%s/001.refs'
				custom_hooks_path = '%s/%s/001.custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, relativePath, backupID, relativePath, backupID, relativePath, backupID),
							filepath.Join(relativePath, backupID, "001.refs"):             "",
							filepath.Join(relativePath, backupID, "001.custom_hooks.tar"): testhelper.MustReadFile(tb, customHooksArchive),
						})

						return repo, new(git.Checksum), ""
					},
					expectExists: true,
					expectedPaths: []string{
						"custom_hooks/pre-commit.sample",
						"custom_hooks/prepare-commit-msg.sample",
						"custom_hooks/pre-push.sample",
					},
				},
				{
					desc: "many incrementals",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						const backupID = "abc123"

						repo, repoPath, head := createAndSeedRepository(t, ctx, cfg)
						relativePath := stripRelativePath(tb, repo)

						master1 := gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("main"),
							gittest.WithParents(head),
						)
						other := gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("other"),
							gittest.WithParents(head),
						)
						gittest.Exec(tb, cfg, "-C", repoPath, "symbolic-ref", "HEAD", "refs/heads/main")
						bundle1 := gittest.Exec(tb, cfg, "-C", repoPath, "bundle", "create", "-",
							"HEAD",
							"refs/heads/main",
							"refs/heads/other",
						)
						refs1 := gittest.Exec(t, cfg, "-C", repoPath, "show-ref", "--head")
						customHooksPath1 := mustCreateCustomHooksArchive(t, ctx)

						master2 := gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("main"),
							gittest.WithParents(master1),
						)
						bundle2 := gittest.Exec(tb, cfg, "-C", repoPath, "bundle", "create", "-",
							"HEAD",
							"^"+master1.String(),
							"^"+other.String(),
							"refs/heads/main",
							"refs/heads/other",
						)
						refs2 := gittest.Exec(t, cfg, "-C", repoPath, "show-ref", "--head")
						customHooksPath2 := mustCreateCustomHooksArchive(t, ctx,
							"another-hook-1.sample",
							"another-hook-2.sample")

						checksum := gittest.ChecksumRepo(t, cfg, repoPath)

						// Create some more commits that will be reverted by the restore.
						gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("main"),
							gittest.WithParents(master2),
						)
						gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("other-2"),
							gittest.WithParents(master2),
						)

						testhelper.WriteFiles(tb, backupRoot, map[string]any{
							filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), "abc123.toml"): fmt.Sprintf(`
object_format = '%[1]s'
head_reference = '%[4]s'

[[steps]]
bundle_path = '%[2]s/%[3]s/001.bundle'
ref_path = '%[2]s/%[3]s/001.refs'
custom_hooks_path = '%[2]s/%[3]s/001.custom_hooks.tar'

[[steps]]
bundle_path = '%[2]s/%[3]s/002.bundle'
ref_path = '%[2]s/%[3]s/002.refs'
custom_hooks_path = '%[2]s/%[3]s/002.custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, relativePath, backupID, git.DefaultRef.String()),
							filepath.Join(relativePath, backupID, "001.bundle"): bundle1,
							filepath.Join(relativePath, backupID, "002.bundle"): bundle2,
							filepath.Join(relativePath, backupID, "001.refs"):   refs1,
							filepath.Join(relativePath, backupID, "002.refs"):   refs2,
						})

						testhelper.CopyFile(tb, customHooksPath1, filepath.Join(backupRoot, relativePath, backupID, "001.custom_hooks.tar"))
						testhelper.CopyFile(tb, customHooksPath2, filepath.Join(backupRoot, relativePath, backupID, "002.custom_hooks.tar"))

						return repo, checksum, ""
					},
					expectExists: true,
					expectedPaths: []string{
						"custom_hooks/pre-commit.sample",
						"custom_hooks/prepare-commit-msg.sample",
						"custom_hooks/pre-push.sample",
						"custom_hooks/another-hook-1.sample",
						"custom_hooks/another-hook-2.sample",
					},
				},
			} {
				t.Run(tc.desc, func(t *testing.T) {
					repo, expectedChecksum, backupPathPrefix := tc.setup(t)

					sink, err := backup.ResolveSink(ctx, backupRoot)
					require.NoError(t, err)
					defer testhelper.MustClose(t, sink)

					locator := backup.NewLocator(sink)

					logger := testhelper.NewLogger(t)

					fsBackup := managerTC.setup(t, sink, locator, logger)
					err = fsBackup.Restore(ctx, &backup.RestoreRequest{
						Server:           storage.ServerInfo{Address: cfg.SocketPath, Token: cfg.Auth.Token},
						Repository:       repo,
						VanityRepository: repo,
						BackupID:         "",
						UseLatest:        true,
						PathPrefix:       backupPathPrefix,
					})
					if tc.expectedErrAs != nil {
						require.ErrorAs(t, err, &tc.expectedErrAs)
					} else {
						require.NoError(t, err)
					}

					exists, err := repoClient.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{
						Repository: repo,
					})
					require.NoError(t, err)
					require.Equal(t, tc.expectExists, exists.GetExists(), "repository exists")

					if expectedChecksum != nil {
						checksum, err := repoClient.CalculateChecksum(ctx, &gitalypb.CalculateChecksumRequest{
							Repository: repo,
						})
						require.NoError(t, err)

						require.Equal(t, expectedChecksum.String(), checksum.GetChecksum())
					}

					if len(tc.expectedPaths) > 0 {
						// Restore has to use the rewritten path as the relative path due to the test creating
						// the repository through Praefect. In order to get to the correct disk paths, we need
						// to get the replica path of the rewritten repository.
						repoPath := filepath.Join(cfg.Storages[0].Path, gittest.GetReplicaPath(t, ctx, cfg, repo))
						for _, p := range tc.expectedPaths {
							require.FileExists(t, filepath.Join(repoPath, p))
						}
					}
				})
			}
		})
	}
}

func TestManager_Restore_specific(t *testing.T) {
	t.Parallel()

	const backupID = "abc123"

	cfg := testcfg.Build(t)
	testcfg.BuildGitalyHooks(t, cfg)
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll)
	repoCounter := counter.NewRepositoryCounter(cfg.Storages)

	for _, managerTC := range []struct {
		desc  string
		setup func(t testing.TB, sink *backup.Sink, locator backup.Locator, logger log.Logger) *backup.Manager
	}{
		{
			desc: "RPC manager",
			setup: func(tb testing.TB, sink *backup.Sink, locator backup.Locator, logger log.Logger) *backup.Manager {
				pool := client.NewPool()
				tb.Cleanup(func() {
					testhelper.MustClose(tb, pool)
				})

				return backup.NewManager(sink, logger, locator, pool)
			},
		},
		{
			desc: "Local manager",
			setup: func(tb testing.TB, sink *backup.Sink, locator backup.Locator, logger log.Logger) *backup.Manager {
				if testhelper.IsPraefectEnabled() {
					tb.Skip("local backup manager expects to operate on the local filesystem so cannot operate through praefect")
				}

				storageLocator := config.NewLocator(cfg)
				gitCmdFactory := gittest.NewCommandFactory(tb, cfg)
				catfileCache := catfile.NewCache(cfg)
				tb.Cleanup(catfileCache.Stop)
				txManager := transaction.NewTrackingManager()
				migrationStateManager := migration.NewStateManager(&[]migration.Migration{})

				return backup.NewManagerLocal(sink, logger, locator, storageLocator, gitCmdFactory, catfileCache, txManager, repoCounter, migrationStateManager)
			},
		},
	} {
		t.Run(managerTC.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)

			cc, err := client.New(ctx, cfg.SocketPath)
			require.NoError(t, err)
			defer testhelper.MustClose(t, cc)

			repoClient := gitalypb.NewRepositoryServiceClient(cc)

			backupRoot := testhelper.TempDir(t)

			for _, tc := range []struct {
				desc                  string
				setup                 func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string)
				alwaysCreate          bool
				expectExists          bool
				expectedPaths         []string
				expectedErrAs         error
				expectedHeadReference git.ReferenceName
			}{
				{
					desc: "missing backup",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						repo, _ := gittest.CreateRepository(tb, ctx, cfg)

						return repo, nil, ""
					},
					expectedErrAs: backup.ErrSkipped,
					expectExists:  true,
				},
				{
					desc: "single incremental",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						repo, repoPath, _ := createAndSeedRepository(t, ctx, cfg)
						repoChecksum, repoBundle, repoRefs := createBackupArtifacts(t, cfg, repoPath)

						relativePath := stripRelativePath(tb, repo)
						testhelper.WriteFiles(tb, backupRoot, map[string]any{
							filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), backupID+".toml"): fmt.Sprintf(`
object_format = '%[1]s'
head_reference = '%[4]s'

[[steps]]
bundle_path = '%[2]s/%[3]s/001.bundle'
ref_path = '%[2]s/%[3]s/001.refs'
custom_hooks_path = '%[2]s/%[3]s/001.custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, relativePath, backupID, git.DefaultRef.String()),
							filepath.Join(relativePath, backupID, "001.bundle"): repoBundle,
							filepath.Join(relativePath, backupID, "001.refs"):   repoRefs,
						})

						return repo, repoChecksum, ""
					},
					expectExists: true,
				},
				{
					desc: "many incrementals",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						repo, repoPath, head := createAndSeedRepository(t, ctx, cfg)

						master1 := gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("main"),
							gittest.WithParents(head),
						)
						other := gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("other"),
							gittest.WithParents(head),
						)
						gittest.Exec(tb, cfg, "-C", repoPath, "symbolic-ref", "HEAD", "refs/heads/main")
						bundle1 := gittest.Exec(tb, cfg, "-C", repoPath, "bundle", "create", "-",
							"HEAD",
							"refs/heads/main",
							"refs/heads/other",
						)
						refs1 := gittest.Exec(tb, cfg, "-C", repoPath, "show-ref", "--head")
						customHooksPath1 := mustCreateCustomHooksArchive(t, ctx)

						master2 := gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("main"),
							gittest.WithParents(master1),
						)
						bundle2 := gittest.Exec(tb, cfg, "-C", repoPath, "bundle", "create", "-",
							"HEAD",
							"^"+master1.String(),
							"^"+other.String(),
							"refs/heads/main",
							"refs/heads/other",
						)
						refs2 := gittest.Exec(tb, cfg, "-C", repoPath, "show-ref", "--head")
						customHooksPath2 := mustCreateCustomHooksArchive(t, ctx,
							"another-hook-1.sample",
							"another-hook-2.sample")

						checksum := gittest.ChecksumRepo(t, cfg, repoPath)

						// Create some more commits that will be reverted by the restore.
						gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("main"),
							gittest.WithParents(master2),
						)
						gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("other-2"),
							gittest.WithParents(master2),
						)

						relativePath := stripRelativePath(tb, repo)
						testhelper.WriteFiles(tb, backupRoot, map[string]any{
							filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), backupID+".toml"): fmt.Sprintf(`
object_format = '%[1]s'
head_reference = '%[4]s'

[[steps]]
bundle_path = '%[2]s/%[3]s/001.bundle'
ref_path = '%[2]s/%[3]s/001.refs'
custom_hooks_path = '%[2]s/%[3]s/001.custom_hooks.tar'

[[steps]]
bundle_path = '%[2]s/%[3]s/002.bundle'
ref_path = '%[2]s/%[3]s/002.refs'
custom_hooks_path = '%[2]s/%[3]s/002.custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, relativePath, backupID, git.DefaultRef.String()),
							filepath.Join(relativePath, backupID, "001.bundle"): bundle1,
							filepath.Join(relativePath, backupID, "002.bundle"): bundle2,
							filepath.Join(relativePath, backupID, "001.refs"):   refs1,
							filepath.Join(relativePath, backupID, "002.refs"):   refs2,
						})

						testhelper.CopyFile(tb, customHooksPath1, filepath.Join(backupRoot, relativePath, backupID, "001.custom_hooks.tar"))
						testhelper.CopyFile(tb, customHooksPath2, filepath.Join(backupRoot, relativePath, backupID, "002.custom_hooks.tar"))

						return repo, checksum, ""
					},
					expectExists: true,
					expectedPaths: []string{
						"custom_hooks/pre-commit.sample",
						"custom_hooks/prepare-commit-msg.sample",
						"custom_hooks/pre-push.sample",
						"custom_hooks/another-hook-1.sample",
						"custom_hooks/another-hook-2.sample",
					},
				},
				{
					desc: "many incrementals with backup path prefix",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						repo, repoPath, head := createAndSeedRepository(t, ctx, cfg)

						master1 := gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("main"),
							gittest.WithParents(head),
						)
						other := gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("other"),
							gittest.WithParents(head),
						)
						gittest.Exec(tb, cfg, "-C", repoPath, "symbolic-ref", "HEAD", "refs/heads/main")
						bundle1 := gittest.Exec(tb, cfg, "-C", repoPath, "bundle", "create", "-",
							"HEAD",
							"refs/heads/main",
							"refs/heads/other",
						)
						refs1 := gittest.Exec(tb, cfg, "-C", repoPath, "show-ref", "--head")
						customHooksPath1 := mustCreateCustomHooksArchive(t, ctx)

						master2 := gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("main"),
							gittest.WithParents(master1),
						)
						bundle2 := gittest.Exec(tb, cfg, "-C", repoPath, "bundle", "create", "-",
							"HEAD",
							"^"+master1.String(),
							"^"+other.String(),
							"refs/heads/main",
							"refs/heads/other",
						)
						refs2 := gittest.Exec(tb, cfg, "-C", repoPath, "show-ref", "--head")
						customHooksPath2 := mustCreateCustomHooksArchive(t, ctx,
							"another-hook-1.sample",
							"another-hook-2.sample")

						checksum := gittest.ChecksumRepo(t, cfg, repoPath)

						// Create some more commits that will be reverted by the restore.
						gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("main"),
							gittest.WithParents(master2),
						)
						gittest.WriteCommit(tb, cfg, repoPath,
							gittest.WithBranch("other-2"),
							gittest.WithParents(master2),
						)

						relativePath := stripRelativePath(tb, repo)
						testhelper.WriteFiles(tb, backupRoot, map[string]any{
							filepath.Join("foo", "manifests", repo.GetStorageName(), repo.GetRelativePath(), backupID+".toml"): fmt.Sprintf(`
object_format = '%[1]s'
head_reference = '%[4]s'

[[steps]]
bundle_path = '%[2]s/%[3]s/001.bundle'
ref_path = '%[2]s/%[3]s/001.refs'
custom_hooks_path = '%[2]s/%[3]s/001.custom_hooks.tar'

[[steps]]
bundle_path = '%[2]s/%[3]s/002.bundle'
ref_path = '%[2]s/%[3]s/002.refs'
custom_hooks_path = '%[2]s/%[3]s/002.custom_hooks.tar'
							`, gittest.DefaultObjectHash.Format, relativePath, backupID, git.DefaultRef.String()),
							filepath.Join(relativePath, backupID, "001.bundle"): bundle1,
							filepath.Join(relativePath, backupID, "002.bundle"): bundle2,
							filepath.Join(relativePath, backupID, "001.refs"):   refs1,
							filepath.Join(relativePath, backupID, "002.refs"):   refs2,
						})

						testhelper.CopyFile(tb, customHooksPath1, filepath.Join(backupRoot, relativePath, backupID, "001.custom_hooks.tar"))
						testhelper.CopyFile(tb, customHooksPath2, filepath.Join(backupRoot, relativePath, backupID, "002.custom_hooks.tar"))

						return repo, checksum, "foo"
					},
					expectExists: true,
					expectedPaths: []string{
						"custom_hooks/pre-commit.sample",
						"custom_hooks/prepare-commit-msg.sample",
						"custom_hooks/pre-push.sample",
						"custom_hooks/another-hook-1.sample",
						"custom_hooks/another-hook-2.sample",
					},
				},
				{
					desc: "empty backup",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						repo, _ := gittest.CreateRepository(tb, ctx, cfg)

						testhelper.WriteFiles(tb, backupRoot, map[string]any{
							filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), backupID+".toml"): fmt.Sprintf(
								`
empty = true
non_existent = false
object_format = %q
head_reference = 'refs/heads/banana'

[[steps]]
bundle_path = 'repo.bundle'
ref_path = 'repo.refs'
custom_hooks_path = 'custom_hooks.tar'
`, gittest.DefaultObjectHash.Format),
							"repo.refs": "",
						})

						return repo, new(git.Checksum), ""
					},
					expectExists:          true,
					expectedHeadReference: "refs/heads/banana",
				},
				{
					desc: "head reference",
					setup: func(tb testing.TB) (*gitalypb.Repository, *git.Checksum, string) {
						repo, repoPath, head := createAndSeedRepository(t, ctx, cfg)
						_, repoBundle, repoRefs := createBackupArtifacts(t, cfg, repoPath)

						testhelper.WriteFiles(tb, backupRoot, map[string]any{
							filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), backupID+".toml"): fmt.Sprintf(
								`empty = false
non_existent = false
object_format = %q
head_reference = 'refs/heads/banana'

[[steps]]
bundle_path = 'repo.bundle'
ref_path = 'repo.refs'
custom_hooks_path = 'custom_hooks.tar'
`, gittest.DefaultObjectHash.Format),
							"repo.bundle": repoBundle,
							"repo.refs":   repoRefs,
						})

						checksum := gittest.ChecksumRepo(tb, cfg, repoPath)
						// Negate off the default branch since the manifest is
						// explicitly setting a different unborn branch that
						// will not be part of the checksum.
						checksum.Add(git.NewReference("HEAD", head))

						return repo, checksum, ""
					},
					expectExists:          true,
					expectedHeadReference: "refs/heads/banana",
				},
			} {
				t.Run(tc.desc, func(t *testing.T) {
					repo, expectedChecksum, backupPathPrefix := tc.setup(t)

					sink, err := backup.ResolveSink(ctx, backupRoot)
					require.NoError(t, err)
					defer testhelper.MustClose(t, sink)

					locator := backup.NewLocator(sink)

					logger := testhelper.NewLogger(t)

					fsBackup := managerTC.setup(t, sink, locator, logger)
					err = fsBackup.Restore(ctx, &backup.RestoreRequest{
						Server:           storage.ServerInfo{Address: cfg.SocketPath, Token: cfg.Auth.Token},
						Repository:       repo,
						VanityRepository: repo,
						AlwaysCreate:     tc.alwaysCreate,
						BackupID:         backupID,
						PathPrefix:       backupPathPrefix,
					})
					if tc.expectedErrAs != nil {
						require.ErrorAs(t, err, &tc.expectedErrAs)
					} else {
						require.NoError(t, err)
					}

					exists, err := repoClient.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{
						Repository: repo,
					})
					require.NoError(t, err)
					require.Equal(t, tc.expectExists, exists.GetExists(), "repository exists")

					if expectedChecksum != nil {
						checksum, err := repoClient.CalculateChecksum(ctx, &gitalypb.CalculateChecksumRequest{
							Repository: repo,
						})
						require.NoError(t, err)

						require.Equal(t, expectedChecksum.String(), checksum.GetChecksum())
					}

					if len(tc.expectedPaths) > 0 {
						// Restore has to use the rewritten path as the relative path due to the test creating
						// the repository through Praefect. In order to get to the correct disk paths, we need
						// to get the replica path of the rewritten repository.
						repoPath := filepath.Join(cfg.Storages[0].Path, gittest.GetReplicaPath(t, ctx, cfg, repo))

						for _, p := range tc.expectedPaths {
							require.FileExists(t, filepath.Join(repoPath, p))
						}
					}

					if len(tc.expectedHeadReference) > 0 {
						repoPath := filepath.Join(cfg.Storages[0].Path, gittest.GetReplicaPath(t, ctx, cfg, repo))

						ref := gittest.GetSymbolicRef(t, cfg, repoPath, "HEAD")
						require.Equal(t, tc.expectedHeadReference, git.ReferenceName(ref.Target))
					}
				})
			}
		})
	}
}

func TestManager_Restore_useLatestFallback(t *testing.T) {
	t.Parallel()

	const existingBackupID = "abc123"
	const missingBackupID = "def456"

	cfg := testcfg.Build(t)
	testcfg.BuildGitalyHooks(t, cfg)
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll)
	repoCounter := counter.NewRepositoryCounter(cfg.Storages)

	for _, managerTC := range []struct {
		desc  string
		setup func(t testing.TB, sink *backup.Sink, locator backup.Locator, logger log.Logger) *backup.Manager
	}{
		{
			desc: "RPC manager",
			setup: func(tb testing.TB, sink *backup.Sink, locator backup.Locator, logger log.Logger) *backup.Manager {
				pool := client.NewPool()
				tb.Cleanup(func() {
					testhelper.MustClose(tb, pool)
				})

				return backup.NewManager(sink, logger, locator, pool)
			},
		},
		{
			desc: "Local manager",
			setup: func(tb testing.TB, sink *backup.Sink, locator backup.Locator, logger log.Logger) *backup.Manager {
				if testhelper.IsPraefectEnabled() {
					tb.Skip("local backup manager expects to operate on the local filesystem so cannot operate through praefect")
				}

				storageLocator := config.NewLocator(cfg)
				gitCmdFactory := gittest.NewCommandFactory(tb, cfg)
				catfileCache := catfile.NewCache(cfg)
				tb.Cleanup(catfileCache.Stop)
				txManager := transaction.NewTrackingManager()
				migrationStateManager := migration.NewStateManager(&[]migration.Migration{})

				return backup.NewManagerLocal(sink, logger, locator, storageLocator, gitCmdFactory, catfileCache, txManager, repoCounter, migrationStateManager)
			},
		},
	} {
		t.Run(managerTC.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)

			for _, tc := range []struct {
				desc          string
				backupID      string
				useLatest     bool
				expectedErrAs error
			}{
				{
					desc:      "UseLatest true falls back to latest available backup",
					backupID:  missingBackupID,
					useLatest: true,
				},
				{
					desc:          "UseLatest false returns error for missing backup ID",
					backupID:      missingBackupID,
					useLatest:     false,
					expectedErrAs: backup.ErrDoesntExist,
				},
			} {
				t.Run(tc.desc, func(t *testing.T) {
					backupRoot := testhelper.TempDir(t)

					repo, repoPath, _ := createAndSeedRepository(t, ctx, cfg)
					repoChecksum, repoBundle, repoRefs := createBackupArtifacts(t, cfg, repoPath)

					relativePath := stripRelativePath(t, repo)
					testhelper.WriteFiles(t, backupRoot, map[string]any{
						filepath.Join("manifests", repo.GetStorageName(), repo.GetRelativePath(), existingBackupID+".toml"): fmt.Sprintf(`
object_format = '%[1]s'

[[steps]]
bundle_path = '%[2]s/%[3]s/001.bundle'
ref_path = '%[2]s/%[3]s/001.refs'
custom_hooks_path = '%[2]s/%[3]s/001.custom_hooks.tar'
`, gittest.DefaultObjectHash.Format, relativePath, existingBackupID),
						filepath.Join(relativePath, existingBackupID, "001.bundle"): repoBundle,
						filepath.Join(relativePath, existingBackupID, "001.refs"):   repoRefs,
					})

					sink, err := backup.ResolveSink(ctx, backupRoot)
					require.NoError(t, err)
					defer testhelper.MustClose(t, sink)

					locator := backup.NewLocator(sink)
					logger := testhelper.NewLogger(t)

					mgr := managerTC.setup(t, sink, locator, logger)
					err = mgr.Restore(ctx, &backup.RestoreRequest{
						Server:           storage.ServerInfo{Address: cfg.SocketPath, Token: cfg.Auth.Token},
						Repository:       repo,
						VanityRepository: repo,
						BackupID:         tc.backupID,
						UseLatest:        tc.useLatest,
					})
					if tc.expectedErrAs != nil {
						require.ErrorAs(t, err, &tc.expectedErrAs)
						return
					}

					require.NoError(t, err)

					cc, err := client.New(ctx, cfg.SocketPath)
					require.NoError(t, err)
					defer testhelper.MustClose(t, cc)

					repoClient := gitalypb.NewRepositoryServiceClient(cc)
					checksum, err := repoClient.CalculateChecksum(ctx, &gitalypb.CalculateChecksumRequest{
						Repository: repo,
					})
					require.NoError(t, err)
					require.Equal(t, repoChecksum.String(), checksum.GetChecksum())
				})
			}
		})
	}
}

func TestManager_CreateRestore_contextServerInfo(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	testcfg.BuildGitalyHooks(t, cfg)
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll)

	ctx := testhelper.Context(t)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	gittest.WriteTag(t, cfg, repoPath, "v1.0.0", commitID.Revision())

	backupRoot := testhelper.TempDir(t)

	pool := client.NewPool()
	defer testhelper.MustClose(t, pool)

	sink, err := backup.ResolveSink(ctx, backupRoot)
	require.NoError(t, err)
	defer testhelper.MustClose(t, sink)

	locator := backup.NewLocator(sink)
	fsBackup := backup.NewManager(sink, testhelper.SharedLogger(t), locator, pool)

	ctx = testhelper.MergeIncomingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))

	require.NoError(t, fsBackup.Create(ctx, &backup.CreateRequest{
		Repository: repo,
		BackupID:   "abc123",
	}))
	require.NoError(t, fsBackup.Restore(ctx, &backup.RestoreRequest{
		Repository: repo,
	}))
}

func TestValidatePathPrefix(t *testing.T) {
	tests := []struct {
		name      string
		prefix    string
		expectErr bool
		errMsg    string
	}{
		{
			name:      "empty prefix is valid",
			prefix:    "",
			expectErr: false,
		},
		{
			name:      "single directory",
			prefix:    "2026",
			expectErr: false,
		},
		{
			name:      "nested relative path",
			prefix:    "backups/monthly/2024-03",
			expectErr: false,
		},
		{
			name:      "absolute path with leading slash",
			prefix:    "/tmp/backups",
			expectErr: true,
			errMsg:    "must be a relative path",
		},
		{
			name:      "directory traversal with ..",
			prefix:    "backups/../../../etc",
			expectErr: true,
			errMsg:    "must be a clean relative path without traversal",
		},
		{
			name:      "directory traversal at start",
			prefix:    "../backups",
			expectErr: true,
			errMsg:    "must be a clean relative path without traversal",
		},
		{
			name:      "directory traversal in middle",
			prefix:    "backups/../other",
			expectErr: true,
			errMsg:    "must be a clean relative path without traversal",
		},
		{
			name:      "dot path",
			prefix:    ".",
			expectErr: true,
			errMsg:    "must be a clean relative path without traversal",
		},
		{
			name:      "path with special characters",
			prefix:    "2024-03_11.Feb",
			expectErr: false,
		},
		{
			name:      "trailing slash",
			prefix:    "backups/",
			expectErr: true,
			errMsg:    "must be a clean relative path without traversal",
		},
		{
			name:      "double slash",
			prefix:    "backups//2024",
			expectErr: true,
			errMsg:    "must be a clean relative path without traversal",
		},
		{
			name:      "dot-dot only",
			prefix:    "..",
			expectErr: true,
			errMsg:    "must be a clean relative path without traversal",
		},
		{
			name:      "triple dots component",
			prefix:    "backups/.../feb",
			expectErr: true,
			errMsg:    "must be a clean relative path without traversal",
		},
		{
			name:      "dot component in middle",
			prefix:    "backups/./2024",
			expectErr: true,
			errMsg:    "must be a clean relative path without traversal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := backup.ValidatePathPrefix(tt.prefix)
			if tt.expectErr {
				require.Error(t, err)
				require.ErrorIs(t, err, backup.ErrInvalidPathPrefix)
				if tt.errMsg != "" {
					require.Contains(t, err.Error(), tt.errMsg)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func joinBackupPath(tb testing.TB, backupRoot string, backupPathPrefix string, repo storage.Repository, elements ...string) string {
	return filepath.Join(append([]string{
		backupRoot,
		backupPathPrefix,
		repo.GetStorageName(),
		repo.GetRelativePath(),
	}, elements...)...)
}

func stripRelativePath(tb testing.TB, repo storage.Repository) string {
	return strings.TrimSuffix(repo.GetRelativePath(), ".git")
}

func mustCreateCustomHooksArchive(t *testing.T, ctx context.Context, additionalHooks ...string) string {
	t.Helper()

	tmpDir := testhelper.TempDir(t)

	hooksDirPath := filepath.Join(tmpDir, "custom_hooks")
	require.NoError(t, os.Mkdir(hooksDirPath, os.ModePerm))

	require.NoError(t, os.WriteFile(filepath.Join(hooksDirPath, "pre-commit.sample"), []byte("foo"), os.ModePerm))
	require.NoError(t, os.WriteFile(filepath.Join(hooksDirPath, "prepare-commit-msg.sample"), []byte("bar"), os.ModePerm))
	require.NoError(t, os.WriteFile(filepath.Join(hooksDirPath, "pre-push.sample"), []byte("baz"), os.ModePerm))

	for _, hookName := range additionalHooks {
		require.NoError(t, os.WriteFile(filepath.Join(hooksDirPath, hookName), []byte("additional hook content"), os.ModePerm))
	}

	archivePath := filepath.Join(tmpDir, "custom_hooks.tar")
	file, err := os.Create(archivePath)
	require.NoError(t, err)
	defer testhelper.MustClose(t, file)

	require.NoError(t, archive.WriteTarball(ctx, testhelper.SharedLogger(t), file, tmpDir, "custom_hooks"))

	return archivePath
}

func createAndSeedRepository(t *testing.T, ctx context.Context, cfg config.Cfg) (repo *gitalypb.Repository, repoPath string, head git.ObjectID) {
	repo, repoPath = gittest.CreateRepository(t, ctx, cfg)
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	gittest.WriteTag(t, cfg, repoPath, "v1.0.0", commitID.Revision())

	return repo, repoPath, commitID
}

func createBackupArtifacts(t *testing.T, cfg config.Cfg, repoPath string) (repoChecksum *git.Checksum, repoBundle []byte, repoRefs []byte) {
	repoChecksum = gittest.ChecksumRepo(t, cfg, repoPath)
	repoBundle = gittest.BundleRepo(t, cfg, repoPath, "-")
	repoRefs = gittest.Exec(t, cfg, "-C", repoPath, "show-ref", "--head")

	return repoChecksum, repoBundle, repoRefs
}

func TestManager_WriteBackupID(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	backupRoot := testhelper.TempDir(t)
	sink, err := backup.ResolveSink(ctx, backupRoot)
	require.NoError(t, err)
	defer testhelper.MustClose(t, sink)

	mgr := backup.NewIDManager(sink)

	// Without path prefix
	require.NoError(t, mgr.WriteBackupID(ctx, &backup.WriteBackupIDRequest{BackupID: "my-backup"}))
	require.FileExists(t, filepath.Join(backupRoot, "backup_ids", "my-backup"))

	// With path prefix
	require.NoError(t, mgr.WriteBackupID(ctx, &backup.WriteBackupIDRequest{BackupID: "my-backup", PathPrefix: "foo"}))
	require.FileExists(t, filepath.Join(backupRoot, "foo", "backup_ids", "my-backup"))
}

func TestManager_ReadLatestBackupID(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	t.Run("no markers", func(t *testing.T) {
		t.Parallel()

		backupRoot := testhelper.TempDir(t)
		sink, err := backup.ResolveSink(ctx, backupRoot)
		require.NoError(t, err)
		defer testhelper.MustClose(t, sink)

		mgr := backup.NewIDManager(sink)

		_, err = mgr.ReadLatestBackupID(ctx, "")
		require.ErrorIs(t, err, backup.ErrDoesntExist)
	})

	t.Run("single marker", func(t *testing.T) {
		t.Parallel()

		backupRoot := testhelper.TempDir(t)
		testhelper.WriteFiles(t, backupRoot, map[string]any{
			"backup_ids/my-backup": "",
		})

		sink, err := backup.ResolveSink(ctx, backupRoot)
		require.NoError(t, err)
		defer testhelper.MustClose(t, sink)

		mgr := backup.NewIDManager(sink)

		id, err := mgr.ReadLatestBackupID(ctx, "")
		require.NoError(t, err)
		require.Equal(t, "my-backup", id)
	})

	t.Run("multiple markers picks latest by modification time", func(t *testing.T) {
		t.Parallel()

		backupRoot := testhelper.TempDir(t)

		// Write markers separately so they get different modification times.
		// The last one written should be picked as latest.
		testhelper.WriteFiles(t, backupRoot, map[string]any{
			"backup_ids/backup-1": "",
		})
		testhelper.WriteFiles(t, backupRoot, map[string]any{
			"backup_ids/backup-2": "",
		})
		testhelper.WriteFiles(t, backupRoot, map[string]any{
			"backup_ids/backup-3": "",
		})

		sink, err := backup.ResolveSink(ctx, backupRoot)
		require.NoError(t, err)
		defer testhelper.MustClose(t, sink)

		mgr := backup.NewIDManager(sink)

		id, err := mgr.ReadLatestBackupID(ctx, "")
		require.NoError(t, err)
		require.Equal(t, "backup-3", id)
	})

	t.Run("multiple markers with path prefix", func(t *testing.T) {
		t.Parallel()

		backupRoot := testhelper.TempDir(t)

		// Write markers separately so they get different modification times.
		// The last one written should be picked as latest.
		testhelper.WriteFiles(t, backupRoot, map[string]any{
			"foo/backup_ids/backup-1": "",
		})
		testhelper.WriteFiles(t, backupRoot, map[string]any{
			"foo/backup_ids/backup-2": "",
		})
		testhelper.WriteFiles(t, backupRoot, map[string]any{
			"foo/backup_ids/backup-3": "",
		})

		sink, err := backup.ResolveSink(ctx, backupRoot)
		require.NoError(t, err)
		defer testhelper.MustClose(t, sink)

		mgr := backup.NewIDManager(sink)

		id, err := mgr.ReadLatestBackupID(ctx, "foo")
		require.NoError(t, err)
		require.Equal(t, "backup-3", id)
	})
}

// TestManager_Restore_referenceBackend covers which backend a restore recreates a repository on.
// The backup's own backend is deliberately not carried over.
func TestManager_Restore_referenceBackend(t *testing.T) {
	t.Parallel()

	testhelper.SkipWithReftable(t, "MVCC backend selection is overridden by GIT_DEFAULT_REF_FORMAT")
	testhelper.SkipWithPraefect(t, "MVCC repositories cannot be registered in Praefect's database")

	cfg := testcfg.Build(t)
	testcfg.BuildGitalyHooks(t, cfg)
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll)

	ctx := testhelper.Context(t)
	repoCounter := counter.NewRepositoryCounter(cfg.Storages)

	for _, managerTC := range []struct {
		desc       string
		prepareCtx func(context.Context) context.Context
		setup      func(tb testing.TB, sink *backup.Sink, locator backup.Locator) *backup.Manager
	}{
		{
			desc:       "RPC manager",
			prepareCtx: func(ctx context.Context) context.Context { return ctx },
			setup: func(tb testing.TB, sink *backup.Sink, locator backup.Locator) *backup.Manager {
				pool := client.NewPool()
				tb.Cleanup(func() { testhelper.MustClose(tb, pool) })

				return backup.NewManager(sink, testhelper.SharedLogger(tb), locator, pool)
			},
		},
		{
			desc: "local manager",
			// Nothing selects the MVCC binary for an in-process manager. In production
			// the MVCC middleware does it, so stand in for it where the test reads an
			// MVCC repository.
			prepareCtx: func(ctx context.Context) context.Context {
				return gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC)
			},
			setup: func(tb testing.TB, sink *backup.Sink, locator backup.Locator) *backup.Manager {
				catfileCache := catfile.NewCache(cfg)
				tb.Cleanup(catfileCache.Stop)

				return backup.NewManagerLocal(
					sink,
					testhelper.SharedLogger(tb),
					locator,
					config.NewLocator(cfg),
					gittest.NewCommandFactory(tb, cfg),
					catfileCache,
					transaction.NewTrackingManager(),
					repoCounter,
					migration.NewStateManager(&[]migration.Migration{}),
				)
			},
		},
	} {
		t.Run(managerTC.desc, func(t *testing.T) {
			for _, tc := range []struct {
				desc            string
				keepRepository  bool
				refBackend      git.ReferenceBackend
				expectedBackend git.ReferenceBackend
			}{
				{
					desc:            "unset restores on the node's backend",
					expectedBackend: gittest.DefaultReferenceBackend,
				},
				{
					desc:            "requested backend is used",
					refBackend:      git.ReferenceBackendMVCC,
					expectedBackend: git.ReferenceBackendMVCC,
				},
				{
					// Honouring the request here would run Git under a binary that
					// cannot read the repository.
					desc:            "existing repository keeps its backend",
					keepRepository:  true,
					refBackend:      git.ReferenceBackendFiles,
					expectedBackend: git.ReferenceBackendMVCC,
				},
			} {
				t.Run(tc.desc, func(t *testing.T) {
					// Back up from MVCC so that restoring on the node's backend is a
					// visible change.
					repo, repoPath := gittest.CreateMVCCRepository(t, ctx, cfg)
					commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
					gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("feature"), gittest.WithParents(commitID))
					checksum := gittest.ChecksumRepo(t, cfg, repoPath)

					sink, err := backup.ResolveSink(ctx, testhelper.TempDir(t))
					require.NoError(t, err)
					defer testhelper.MustClose(t, sink)

					fsBackup := managerTC.setup(t, sink, backup.NewLocator(sink))

					ctx := testhelper.MergeIncomingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))

					require.NoError(t, fsBackup.Create(managerTC.prepareCtx(ctx), &backup.CreateRequest{
						Repository: repo,
						BackupID:   "abc123",
					}))

					restoreCtx := ctx
					if tc.keepRepository {
						restoreCtx = managerTC.prepareCtx(ctx)
					} else {
						// With the repository gone, only the request says which
						// backend to use.
						require.NoError(t, os.RemoveAll(repoPath))
					}

					require.NoError(t, fsBackup.Restore(restoreCtx, &backup.RestoreRequest{
						Repository:       repo,
						BackupID:         "abc123",
						ReferenceBackend: tc.refBackend,
					}))

					backend, err := gitcmd.DetectReferenceBackend(ctx, repoPath)
					require.NoError(t, err)
					require.Equal(t, tc.expectedBackend.Name, backend.Name)

					// Whichever backend it came back on, the contents must match.
					require.Equal(t, checksum, gittest.ChecksumRepo(t, cfg, repoPath))
				})
			}
		})
	}
}
