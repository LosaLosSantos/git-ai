package repository

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/config/auth"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/gitaly/transaction"
	"github.com/feder-cr/open_scion/internal/grpc/metadata"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/internal/transaction/txinfo"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestCreateRepository_missingAuth(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{Auth: auth.Config{Token: "some"}}))

	_, serverSocketPath := runRepositoryService(t, cfg)
	client := newRepositoryClient(t, config.Cfg{Auth: auth.Config{Token: ""}}, serverSocketPath)

	_, err := client.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{
		Repository: &gitalypb.Repository{
			StorageName:  cfg.Storages[0].Name,
			RelativePath: gittest.NewRepositoryName(t),
		},
	})
	testhelper.RequireGrpcError(t, structerr.NewUnauthenticated("authentication required"), err)
}

func TestCreateRepository_successful(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRepositoryService(t)

	repo := &gitalypb.Repository{
		StorageName:  cfg.Storages[0].Name,
		RelativePath: gittest.NewRepositoryName(t),
	}

	_, err := client.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{
		Repository: repo,
	})
	require.NoError(t, err)

	conn := gittest.DialService(t, ctx, cfg)
	require.True(t, gittest.RepositoryExists(t, ctx, conn, repo))

	repoDir := filepath.Join(cfg.Storages[0].Path, gittest.GetReplicaPath(t, ctx, cfg, repo))

	require.NoError(t, unix.Access(repoDir, unix.R_OK))
	require.NoError(t, unix.Access(repoDir, unix.W_OK))
	require.NoError(t, unix.Access(repoDir, unix.X_OK))

	for _, dir := range []string{repoDir, filepath.Join(repoDir, "refs")} {
		fi, err := os.Stat(dir)
		require.NoError(t, err)
		require.True(t, fi.IsDir(), "%q must be a directory", fi.Name())

		require.NoError(t, unix.Access(dir, unix.R_OK))
		require.NoError(t, unix.Access(dir, unix.W_OK))
		require.NoError(t, unix.Access(dir, unix.X_OK))
	}

	symRef := string(gittest.Exec(t, cfg, "-C", repoDir, "symbolic-ref", "HEAD"))
	require.Equal(t, symRef, fmt.Sprintf("%s\n", git.DefaultRef))
}

func TestCreateRepository_withDefaultBranch(t *testing.T) {
	t.Parallel()

	cfg, client := setupRepositoryService(t)
	ctx := testhelper.Context(t)
	refClient := gitalypb.NewRefServiceClient(gittest.DialService(t, ctx, cfg))

	for _, tc := range []struct {
		desc              string
		defaultBranch     string
		expected          string
		expectedErrString string
	}{
		{
			desc:          "valid default branch",
			defaultBranch: "develop",
			expected:      "refs/heads/develop",
		},
		{
			desc:          "empty branch name",
			defaultBranch: "",
			expected:      "refs/heads/main",
		},
		{
			desc:              "invalid branch name",
			defaultBranch:     "./.lock",
			expected:          "refs/heads/main",
			expectedErrString: `creating repository: exit status 128, stderr: "fatal: invalid initial branch name: './.lock'\n"`,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			repo := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: gittest.NewRepositoryName(t)}

			req := &gitalypb.CreateRepositoryRequest{Repository: repo, DefaultBranch: []byte(tc.defaultBranch)}
			_, err := client.CreateRepository(ctx, req)
			if tc.expectedErrString != "" {
				require.Contains(t, err.Error(), tc.expectedErrString)
			} else {
				require.NoError(t, err)

				resp, err := refClient.FindDefaultBranchName(ctx, &gitalypb.FindDefaultBranchNameRequest{
					Repository: repo,
					HeadOnly:   true,
				})
				require.NoError(t, err)
				symRef := string(resp.GetName())
				require.Equal(t, tc.expected, symRef)
			}
		})
	}
}

func TestCreateRepository_withObjectFormat(t *testing.T) {
	t.Parallel()

	cfg, client := setupRepositoryService(t)
	ctx := testhelper.Context(t)

	for _, tc := range []struct {
		desc               string
		objectFormat       gitalypb.ObjectFormat
		expectedResponse   *gitalypb.CreateRepositoryResponse
		expectedObjectHash git.ObjectHash
		expectedErr        error
	}{
		{
			desc:               "unspecified object format",
			objectFormat:       gitalypb.ObjectFormat_OBJECT_FORMAT_UNSPECIFIED,
			expectedResponse:   &gitalypb.CreateRepositoryResponse{},
			expectedObjectHash: git.ObjectHashSHA1,
		},
		{
			desc:               "SHA1",
			objectFormat:       gitalypb.ObjectFormat_OBJECT_FORMAT_SHA1,
			expectedResponse:   &gitalypb.CreateRepositoryResponse{},
			expectedObjectHash: git.ObjectHashSHA1,
		},
		{
			desc:               "SHA256",
			objectFormat:       gitalypb.ObjectFormat_OBJECT_FORMAT_SHA256,
			expectedResponse:   &gitalypb.CreateRepositoryResponse{},
			expectedObjectHash: git.ObjectHashSHA256,
		},
		{
			desc:         "invalid object format",
			objectFormat: 3,
			expectedErr:  structerr.NewInvalidArgument("unknown object format: \"3\""),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			repoProto := &gitalypb.Repository{
				StorageName:  cfg.Storages[0].Name,
				RelativePath: gittest.NewRepositoryName(t),
			}

			response, err := client.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{
				Repository:   repoProto,
				ObjectFormat: tc.objectFormat,
			})
			testhelper.RequireGrpcError(t, tc.expectedErr, err)
			testhelper.ProtoEqual(t, tc.expectedResponse, response)

			if err != nil {
				return
			}

			resp, err := client.ObjectFormat(ctx, &gitalypb.ObjectFormatRequest{
				Repository: repoProto,
			})
			require.NoError(t, err)

			// If the repository was created we can check whether the object format of
			// the created repository matches our expectations.
			objectHash, err := git.ObjectHashByProto(resp.GetFormat())
			require.NoError(t, err)
			require.Equal(t, tc.expectedObjectHash.Format, objectHash.Format)
		})
	}
}

func TestCreateRepository_mvccBackend(t *testing.T) {
	t.Parallel()

	gittest.SkipWithoutMVCCGit(t)

	// When GIT_DEFAULT_REF_FORMAT is set, git-init honors it directly and the command factory
	// skips its own ref-format selection, so the MVCC backend cannot be chosen. This test only
	// makes sense when the reference format is not dictated by the environment.
	testhelper.SkipWithReftable(t, "MVCC backend selection is overridden by GIT_DEFAULT_REF_FORMAT")

	cfg, client := setupRepositoryService(t)

	for _, tc := range []struct {
		desc            string
		mvccEnabled     bool
		expectedBackend git.ReferenceBackend
	}{
		{
			desc:            "MVCC backend disabled",
			mvccEnabled:     false,
			expectedBackend: git.ReferenceBackendFiles,
		},
		{
			desc:            "MVCC backend enabled",
			mvccEnabled:     true,
			expectedBackend: git.ReferenceBackendMVCC,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			// Use OutgoingCtxWithFeatureFlag (not ContextWithFeatureFlag) so the flag value
			// replaces the default injected by testhelper.Context on the outgoing metadata
			// that this gRPC client call transmits to the server.
			ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.NewRepoMVCCBackend, tc.mvccEnabled)

			repo := &gitalypb.Repository{
				StorageName:  cfg.Storages[0].Name,
				RelativePath: gittest.NewRepositoryName(t),
			}

			_, err := client.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{
				Repository: repo,
				// Match the object format the test is running with so that the gittest
				// helpers below, which use the default object hash, operate on a
				// compatible repository.
				ObjectFormat: gittest.DefaultObjectHash.ProtoFormat,
			})
			require.NoError(t, err)

			repoDir := filepath.Join(cfg.Storages[0].Path, gittest.GetReplicaPath(t, ctx, cfg, repo))

			backend, err := gitcmd.DetectReferenceBackend(ctx, repoDir)
			require.NoError(t, err)
			require.Equal(t, tc.expectedBackend.Name, backend.Name)

			// Validate the repository is well-configured after creation by exercising a
			// write and a read against it: writing a commit and resolving it back must work
			// end to end for the created reference backend. gittest detects the backend from
			// the repository on disk and selects the matching Git binary, so this also
			// covers the MVCC backend being served by the MVCC-enabled Git binary.
			commitID := gittest.WriteCommit(t, cfg, repoDir, gittest.WithBranch("main"))
			require.Equal(t, commitID, gittest.ResolveRevision(t, cfg, repoDir, "refs/heads/main"))
		})
	}
}

func TestCreateRepository_referenceBackend(t *testing.T) {
	t.Parallel()

	// When GIT_DEFAULT_REF_FORMAT is set, git-init honors it directly and the command factory
	// skips its own ref-format selection, so the requested backend cannot be chosen. This test
	// only makes sense when the reference format is not dictated by the environment.
	testhelper.SkipWithReftable(t, "backend selection is overridden by GIT_DEFAULT_REF_FORMAT")

	cfg, client := setupRepositoryService(t)

	for _, tc := range []struct {
		desc            string
		requiresMVCCGit bool
		refBackend      gitalypb.ReferenceBackend
		expectedBackend git.ReferenceBackend
	}{
		{
			desc:            "unspecified leaves the choice to the node",
			refBackend:      gitalypb.ReferenceBackend_REFERENCE_BACKEND_UNSPECIFIED,
			expectedBackend: git.ReferenceBackendFiles,
		},
		{
			desc:            "files",
			refBackend:      gitalypb.ReferenceBackend_REFERENCE_BACKEND_FILES,
			expectedBackend: git.ReferenceBackendFiles,
		},
		{
			desc:            "reftable",
			refBackend:      gitalypb.ReferenceBackend_REFERENCE_BACKEND_REFTABLE,
			expectedBackend: git.ReferenceBackendReftables,
		},
		{
			desc:            "mvcc",
			requiresMVCCGit: true,
			refBackend:      gitalypb.ReferenceBackend_REFERENCE_BACKEND_MVCC,
			expectedBackend: git.ReferenceBackendMVCC,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			if tc.requiresMVCCGit {
				gittest.SkipWithoutMVCCGit(t)
			}

			ctx := testhelper.Context(t)

			repo := &gitalypb.Repository{
				StorageName:  cfg.Storages[0].Name,
				RelativePath: gittest.NewRepositoryName(t),
			}

			_, err := client.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{
				Repository: repo,
				// Match the object format the test is running with so that the gittest
				// helpers below, which use the default object hash, operate on a
				// compatible repository.
				ObjectFormat:     gittest.DefaultObjectHash.ProtoFormat,
				ReferenceBackend: tc.refBackend,
			})
			require.NoError(t, err)

			repoDir := filepath.Join(cfg.Storages[0].Path, gittest.GetReplicaPath(t, ctx, cfg, repo))

			backend, err := gitcmd.DetectReferenceBackend(ctx, repoDir)
			require.NoError(t, err)
			require.Equal(t, tc.expectedBackend.Name, backend.Name)

			// It must also be usable, so write and read a ref back.
			commitID := gittest.WriteCommit(t, cfg, repoDir, gittest.WithBranch("main"))
			require.Equal(t, commitID, gittest.ResolveRevision(t, cfg, repoDir, "refs/heads/main"))
		})
	}
}

func TestCreateRepository_invalidArguments(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRepositoryService(t)

	preexistingRepo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		// This creates the first repository on the server. As this test can run with
		// Praefect in front of it, we'll use the next replica path Praefect will assign in
		// order to ensure this repository creation conflicts even with Praefect in front of
		// it.
		RelativePath: storage.DeriveReplicaPath(1),
	})

	for _, tc := range []struct {
		desc        string
		repo        *gitalypb.Repository
		expectedErr error
	}{
		{
			desc:        "missing repository",
			repo:        nil,
			expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
		},
		{
			desc: "invalid storage",
			repo: &gitalypb.Repository{
				StorageName:  "does not exist",
				RelativePath: "foobar.git",
			},
			expectedErr: testhelper.ToInterceptedMetadata(structerr.NewInvalidArgument(
				"%w", storage.NewStorageNotFoundError("does not exist"),
			)),
		},
		{
			desc: "preexisting repository",
			repo: preexistingRepo,
			expectedErr: structerr.NewAlreadyExists("%s", testhelper.GitalyOrPraefect(
				"creating repository: repository exists already",
				"route repository creation: reserve repository id: repository already exists",
			)),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := client.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{Repository: tc.repo})
			require.Error(t, err)
			testhelper.RequireGrpcError(t, tc.expectedErr, err)
		})
	}
}

func TestCreateRepository_transactional(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	txManager := transaction.NewTrackingManager()
	cfg, client := setupRepositoryService(t, testserver.WithTransactionManager(txManager))

	ctx, err := txinfo.InjectTransaction(ctx, 1, "node", true)
	require.NoError(t, err)
	ctx = metadata.IncomingToOutgoing(ctx)

	repo := &gitalypb.Repository{
		StorageName:  cfg.Storages[0].Name,
		RelativePath: "repo.git",
	}
	_, err = client.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{Repository: repo})
	require.NoError(t, err)

	require.True(t, gittest.RepositoryExists(t, ctx, gittest.DialService(t, ctx, cfg), repo))
	require.Equal(t, 3, len(txManager.Votes()), "expected transactional vote")
}
