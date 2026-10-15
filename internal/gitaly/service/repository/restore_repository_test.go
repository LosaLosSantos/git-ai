package repository

import (
	"context"
	"os"
	"testing"

	"github.com/feder-cr/open_scion/internal/backup"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestRestoreRepository(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	type setupData struct {
		cfg              config.Cfg
		client           gitalypb.RepositoryServiceClient
		repo             *gitalypb.Repository
		repoPath         string
		backupID         string
		backupPathPrefix string
		expectedChecksum *git.Checksum
	}

	for _, tc := range []struct {
		desc        string
		setup       func(t *testing.T, ctx context.Context, backupSink *backup.Sink, backupLocator backup.Locator) setupData
		expectedErr error
	}{
		{
			desc: "restore backup ID",
			setup: func(t *testing.T, ctx context.Context, backupSink *backup.Sink, backupLocator backup.Locator) setupData {
				cfg, client := setupRepositoryService(t,
					testserver.WithBackupSink(backupSink),
					testserver.WithBackupLocator(backupLocator),
				)

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
					cfg:              cfg,
					client:           client,
					repo:             repo,
					repoPath:         repoPath,
					backupID:         "abc123",
					expectedChecksum: checksum,
				}
			},
		},
		{
			desc: "restore latest",
			setup: func(t *testing.T, ctx context.Context, backupSink *backup.Sink, backupLocator backup.Locator) setupData {
				cfg, client := setupRepositoryService(t,
					testserver.WithBackupSink(backupSink),
					testserver.WithBackupLocator(backupLocator),
				)

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
					cfg:              cfg,
					client:           client,
					repo:             repo,
					repoPath:         repoPath,
					backupID:         "",
					expectedChecksum: checksum,
				}
			},
		},
		{
			desc: "restore latest with backup path prefix",
			setup: func(t *testing.T, ctx context.Context, backupSink *backup.Sink, backupLocator backup.Locator) setupData {
				cfg, client := setupRepositoryService(t,
					testserver.WithBackupSink(backupSink),
					testserver.WithBackupLocator(backupLocator),
				)

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
					cfg:              cfg,
					client:           client,
					repo:             repo,
					repoPath:         repoPath,
					backupID:         "",
					backupPathPrefix: "foo",
					expectedChecksum: checksum,
				}
			},
		},
		{
			desc: "restore latest missing",
			setup: func(t *testing.T, ctx context.Context, backupSink *backup.Sink, backupLocator backup.Locator) setupData {
				cfg, client := setupRepositoryService(t,
					testserver.WithBackupSink(backupSink),
					testserver.WithBackupLocator(backupLocator),
				)
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					RelativePath: "@test/restore/latest/missing.git",
				})

				return setupData{
					cfg:      cfg,
					client:   client,
					repo:     repo,
					repoPath: repoPath,
					backupID: "",
				}
			},
			expectedErr: structerr.NewInternal("restore repository: manifest: find latest: doesn't exist"),
		},
		{
			desc: "missing repository",
			setup: func(t *testing.T, ctx context.Context, backupSink *backup.Sink, backupLocator backup.Locator) setupData {
				cfg, client := setupRepositoryService(t,
					testserver.WithBackupSink(backupSink),
					testserver.WithBackupLocator(backupLocator),
				)

				return setupData{
					cfg:      cfg,
					client:   client,
					repo:     nil,
					backupID: "abc123",
				}
			},
			expectedErr: func() error {
				errorMessage := "restore repository: repository: repository not set"
				if testhelper.IsPraefectEnabled() {
					errorMessage = "repository not set"
				}

				return structerr.NewInvalidArgument("%s", errorMessage)
			}(),
		},
		{
			desc: "missing backup sink",
			setup: func(t *testing.T, ctx context.Context, backupSink *backup.Sink, backupLocator backup.Locator) setupData {
				cfg, client := setupRepositoryService(t,
					testserver.WithBackupLocator(backupLocator),
				)

				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

				return setupData{
					cfg:      cfg,
					client:   client,
					repo:     repo,
					repoPath: repoPath,
					backupID: "abc123",
				}
			},
			expectedErr: structerr.NewFailedPrecondition("restore repository: server-side backups are not configured"),
		},
		{
			desc: "missing backup locator",
			setup: func(t *testing.T, ctx context.Context, backupSink *backup.Sink, backupLocator backup.Locator) setupData {
				cfg, client := setupRepositoryService(t,
					testserver.WithBackupSink(backupSink),
				)

				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

				return setupData{
					cfg:      cfg,
					client:   client,
					repo:     repo,
					repoPath: repoPath,
					backupID: "abc123",
				}
			},
			expectedErr: structerr.NewFailedPrecondition("restore repository: server-side backups are not configured"),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			backupRoot := testhelper.TempDir(t)
			backupSink, err := backup.ResolveSink(ctx, backupRoot)
			require.NoError(t, err)

			backupLocator := backup.NewLocator(backupSink)

			data := tc.setup(t, ctx, backupSink, backupLocator)

			response, err := data.client.RestoreRepository(ctx, &gitalypb.RestoreRepositoryRequest{
				Repository:       data.repo,
				VanityRepository: data.repo,
				BackupId:         data.backupID,
				PathPrefix:       data.backupPathPrefix,
			})
			if tc.expectedErr != nil {
				testhelper.RequireGrpcError(t, tc.expectedErr, err)
				return
			}

			require.NoError(t, err)
			testhelper.ProtoEqual(t, &gitalypb.RestoreRepositoryResponse{}, response)

			checksum := gittest.ChecksumRepo(t, data.cfg, data.repoPath)
			require.Equal(t, data.expectedChecksum.String(), checksum.String())
		})
	}
}

// TestRestoreRepository_referenceBackend covers the reference_backend field of the request.
func TestRestoreRepository_referenceBackend(t *testing.T) {
	t.Parallel()

	testhelper.SkipWithReftable(t, "MVCC backend selection is overridden by GIT_DEFAULT_REF_FORMAT")
	testhelper.SkipWithPraefect(t, "MVCC repositories cannot be registered in Praefect's database")

	ctx := testhelper.Context(t)

	for _, tc := range []struct {
		desc            string
		keepRepository  bool
		refBackend      gitalypb.ReferenceBackend
		expectedBackend git.ReferenceBackend
	}{
		{
			desc:            "unspecified restores on the node's backend",
			refBackend:      gitalypb.ReferenceBackend_REFERENCE_BACKEND_UNSPECIFIED,
			expectedBackend: gittest.DefaultReferenceBackend,
		},
		{
			desc:            "requested backend is used",
			refBackend:      gitalypb.ReferenceBackend_REFERENCE_BACKEND_MVCC,
			expectedBackend: git.ReferenceBackendMVCC,
		},
		{
			// The repository is on MVCC and the node would pick files, so honouring
			// either would run Git under a binary that cannot read it.
			desc:            "existing repository keeps its backend",
			keepRepository:  true,
			refBackend:      gitalypb.ReferenceBackend_REFERENCE_BACKEND_FILES,
			expectedBackend: git.ReferenceBackendMVCC,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			backupRoot := testhelper.TempDir(t)
			backupSink, err := backup.ResolveSink(ctx, backupRoot)
			require.NoError(t, err)
			defer testhelper.MustClose(t, backupSink)

			backupLocator := backup.NewLocator(backupSink)

			cfg, client := setupRepositoryService(t,
				testserver.WithBackupSink(backupSink),
				testserver.WithBackupLocator(backupLocator),
			)

			// Bundle from MVCC so that restoring on the node's backend is visible.
			_, templateRepoPath := gittest.CreateMVCCRepository(t, ctx, cfg)
			oid := gittest.WriteCommit(t, cfg, templateRepoPath, gittest.WithBranch(git.DefaultBranch))
			gittest.WriteCommit(t, cfg, templateRepoPath, gittest.WithBranch("feature"), gittest.WithParents(oid))
			expectedChecksum := gittest.ChecksumRepo(t, cfg, templateRepoPath)

			var repo *gitalypb.Repository
			var repoPath string
			if tc.keepRepository {
				repo, repoPath = gittest.CreateMVCCRepository(t, ctx, cfg)
			} else {
				// Absent, so the restore is what creates it.
				repo, repoPath = gittest.CreateRepository(t, ctx, cfg)
				require.NoError(t, os.RemoveAll(repoPath))
			}

			backupData := backupLocator.BeginFull(ctx, repo, "abc123")
			step := backupData.Steps[len(backupData.Steps)-1]

			w, err := backupSink.GetWriter(ctx, step.BundlePath)
			require.NoError(t, err)
			_, err = w.Write(gittest.BundleRepo(t, cfg, templateRepoPath, "-"))
			require.NoError(t, err)
			require.NoError(t, w.Close())

			w, err = backupSink.GetWriter(ctx, step.RefPath)
			require.NoError(t, err)
			_, err = w.Write(gittest.Exec(t, cfg, "-C", templateRepoPath, "show-ref", "--head"))
			require.NoError(t, err)
			require.NoError(t, w.Close())

			backupData.ObjectFormat = gittest.DefaultObjectHash.Format
			backupData.HeadReference = git.DefaultRef.String()
			require.NoError(t, backupLocator.Commit(ctx, backupData))

			response, err := client.RestoreRepository(ctx, &gitalypb.RestoreRepositoryRequest{
				Repository:       repo,
				VanityRepository: repo,
				BackupId:         "abc123",
				ReferenceBackend: tc.refBackend,
			})
			require.NoError(t, err)
			testhelper.ProtoEqual(t, &gitalypb.RestoreRepositoryResponse{}, response)

			backend, err := gitcmd.DetectReferenceBackend(ctx, repoPath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedBackend.Name, backend.Name)

			require.Equal(t, expectedChecksum.String(), gittest.ChecksumRepo(t, cfg, repoPath).String())
		})
	}
}
