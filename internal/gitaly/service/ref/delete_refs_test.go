package ref

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	hookservice "github.com/feder-cr/git-ai/internal/gitaly/service/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/grpc/metadata"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/internal/transaction/txinfo"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDeleteRefs_successful(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)

	testCases := []struct {
		desc    string
		request *gitalypb.DeleteRefsRequest
	}{
		{
			desc: "delete all except refs with certain prefixes",
			request: &gitalypb.DeleteRefsRequest{
				ExceptWithPrefix: [][]byte{[]byte("refs/keep"), []byte("refs/also-keep"), []byte("refs/heads/")},
			},
		},
		{
			desc: "delete certain refs",
			request: &gitalypb.DeleteRefsRequest{
				Refs: [][]byte{
					[]byte("refs/delete/a"),
					[]byte("refs/also-delete/b"),
					[]byte("refs/delete/symbolic-a"),
					[]byte("refs/delete/symbolic-c"),
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.desc, func(t *testing.T) {
			repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

			for _, ref := range []string{
				"refs/heads/master",
				"refs/delete/a",
				"refs/also-delete/b",
				"refs/keep/c",
				"refs/also-keep/d",
			} {
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage(ref), gittest.WithReference(ref))
			}

			gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/delete/symbolic-a", "refs/delete/a")
			gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/delete/symbolic-c", "refs/keep/c")

			testCase.request.Repository = repo
			_, err := client.DeleteRefs(ctx, testCase.request)
			require.NoError(t, err)

			// Ensure that the internal refs are gone, but the others still exist
			refs := gittest.GetReferences(t, cfg, repoPath)

			refNames := make([]string, len(refs))
			for i, branch := range refs {
				refNames[i] = branch.Name.String()
			}

			require.NotContains(t, refNames, "refs/delete/a")
			require.NotContains(t, refNames, "refs/also-delete/b")
			require.NotContains(t, refNames, "refs/delete/symbolic-a")
			require.NotContains(t, refNames, "refs/delete/symbolic-c")
			require.Contains(t, refNames, "refs/keep/c")
			require.Contains(t, refNames, "refs/also-keep/d")
			require.Contains(t, refNames, "refs/heads/master")
		})
	}
}

func TestDeleteRefs_transaction(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	testcfg.BuildGitalyHooks(t, cfg)

	txManager := transaction.NewTrackingManager()

	addr := testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterRefServiceServer(srv, NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
		gitalypb.RegisterHookServiceServer(srv, hookservice.NewServer(deps))
	}, testserver.WithTransactionManager(txManager))
	cfg.SocketPath = addr

	client, conn := newRefServiceClient(t, addr)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	ctx, err := txinfo.InjectTransaction(ctx, 1, "node", true)
	require.NoError(t, err)
	ctx = metadata.IncomingToOutgoing(ctx)

	for _, tc := range []struct {
		desc          string
		request       *gitalypb.DeleteRefsRequest
		expectedVotes int
	}{
		{
			desc: "delete nothing",
			request: &gitalypb.DeleteRefsRequest{
				ExceptWithPrefix: [][]byte{[]byte("refs/")},
			},
			expectedVotes: 3,
		},
		{
			desc: "delete all refs",
			request: &gitalypb.DeleteRefsRequest{
				ExceptWithPrefix: [][]byte{[]byte("nonexisting/prefix/")},
			},
			expectedVotes: 3,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
			gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

			txManager.Reset()

			tc.request.Repository = repo

			response, err := client.DeleteRefs(ctx, tc.request)
			require.NoError(t, err)
			require.Empty(t, response.GetGitError())

			require.Equal(t, tc.expectedVotes, len(txManager.Votes()))
		})
	}
}

func TestDeleteRefs_invalidRefFormat(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)
	repo, _ := gittest.CreateRepository(t, ctx, cfg)

	request := &gitalypb.DeleteRefsRequest{
		Repository: repo,
		Refs:       [][]byte{[]byte(`refs invalid-ref-format`)},
	}

	response, err := client.DeleteRefs(ctx, request)

	require.Nil(t, response)
	detailedErr := structerr.NewInvalidArgument("invalid references").WithDetail(
		&gitalypb.DeleteRefsError{
			Error: &gitalypb.DeleteRefsError_InvalidFormat{
				InvalidFormat: &gitalypb.InvalidRefFormatError{
					Refs: request.GetRefs(),
				},
			},
		},
	)
	testhelper.RequireGrpcError(t, detailedErr, err)
}

func TestDeleteRefs_refLocked(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	oldValue := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"))
	newValue := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("new"))

	request := &gitalypb.DeleteRefsRequest{
		Repository: repoProto,
		Refs:       [][]byte{[]byte("refs/heads/master")},
	}

	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	updater, err := updateref.New(ctx, repo)
	require.NoError(t, err)
	defer testhelper.MustClose(t, updater)

	require.NoError(t, updater.Start())
	require.NoError(t, updater.Update(
		git.ReferenceName("refs/heads/master"),
		newValue,
		oldValue,
	))
	require.NoError(t, updater.Prepare())

	response, err := client.DeleteRefs(ctx, request)

	// For reftable there is only table level locking and hence no
	// reference value is provided.
	expectedRefs := gittest.FilesOrReftables([]byte("refs/heads/master"), nil)

	require.Nil(t, response)
	// This gets intercepted by the Aborted interceptor which replaces the error message
	// "cannot lock references"
	detailedErr := testhelper.ToInterceptedMetadata(
		structerr.NewAborted("The operation could not be completed. Please try again.").
			WithDetail(&gitalypb.DeleteRefsError{
				Error: &gitalypb.DeleteRefsError_ReferencesLocked{
					ReferencesLocked: &gitalypb.ReferencesLockedError{
						Refs: [][]byte{expectedRefs},
					},
				},
			}).
			WithMetadata(
				"error_details", "cannot lock references",
			),
	)
	testhelper.RequireGrpcError(t, detailedErr, err)
}

func TestDeleteRefs_packedRefsLocked(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"))
	// Pack the reference so that deleting it needs the packed-refs lock. For the reftable
	// backend this compacts the tables.
	gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")

	// Emulate a concurrent writer holding the lock on the packed references.
	lockFile := gittest.FilesOrReftables("packed-refs.lock", "reftable/tables.list.lock")
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, lockFile), nil, fs.ModePerm))

	response, err := client.DeleteRefs(ctx, &gitalypb.DeleteRefsRequest{
		Repository: repoProto,
		Refs:       [][]byte{[]byte("refs/heads/master")},
	})
	require.Nil(t, response)

	// The lock covers the whole packed-refs file (or table list), so no reference name is
	// reported. The reftable backend reports this through the generic locking error and thus
	// carries one empty name. The Aborted interceptor replaces the error message.
	detailedErr := testhelper.ToInterceptedMetadata(
		structerr.NewAborted("The operation could not be completed. Please try again.").
			WithDetail(&gitalypb.DeleteRefsError{
				Error: &gitalypb.DeleteRefsError_ReferencesLocked{
					ReferencesLocked: &gitalypb.ReferencesLockedError{
						Refs: gittest.FilesOrReftables[[][]byte](nil, [][]byte{nil}),
					},
				},
			}).
			WithMetadata(
				"error_details", "cannot lock references",
			),
	)
	testhelper.RequireGrpcError(t, detailedErr, err)
}

func TestDeleteRefs_validation(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)
	repo, _ := gittest.CreateRepository(t, ctx, cfg)

	testCases := []struct {
		desc        string
		request     *gitalypb.DeleteRefsRequest
		expectedErr error
	}{
		{
			desc:        "no repository provided",
			request:     &gitalypb.DeleteRefsRequest{Repository: nil},
			expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
		},
		{
			desc: "Invalid repository",
			request: &gitalypb.DeleteRefsRequest{
				Repository:       &gitalypb.Repository{StorageName: "fake", RelativePath: "path"},
				ExceptWithPrefix: [][]byte{[]byte("exclude-this")},
			},
			expectedErr: testhelper.ToInterceptedMetadata(structerr.NewInvalidArgument(
				"%w", storage.NewStorageNotFoundError("fake"),
			)),
		},
		{
			desc: "Repository is nil",
			request: &gitalypb.DeleteRefsRequest{
				Repository:       nil,
				ExceptWithPrefix: [][]byte{[]byte("exclude-this")},
			},
			expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
		},
		{
			desc: "No prefixes nor refs",
			request: &gitalypb.DeleteRefsRequest{
				Repository: repo,
			},
			expectedErr: status.Error(codes.InvalidArgument, "empty ExceptWithPrefix and Refs"),
		},
		{
			desc: "prefixes with refs",
			request: &gitalypb.DeleteRefsRequest{
				Repository:       repo,
				ExceptWithPrefix: [][]byte{[]byte("exclude-this")},
				Refs:             [][]byte{[]byte("delete-this")},
			},
			expectedErr: status.Error(codes.InvalidArgument, "ExceptWithPrefix and Refs are mutually exclusive"),
		},
		{
			desc: "Empty prefix",
			request: &gitalypb.DeleteRefsRequest{
				Repository:       repo,
				ExceptWithPrefix: [][]byte{[]byte("exclude-this"), {}},
			},
			expectedErr: status.Error(codes.InvalidArgument, "empty prefix for exclusion"),
		},
		{
			desc: "Empty ref",
			request: &gitalypb.DeleteRefsRequest{
				Repository: repo,
				Refs:       [][]byte{[]byte("delete-this"), {}},
			},
			expectedErr: status.Error(codes.InvalidArgument, "empty ref"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := client.DeleteRefs(ctx, tc.request)
			testhelper.RequireGrpcError(t, tc.expectedErr, err)
		})
	}
}
