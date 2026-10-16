package ref

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestRefExists(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commitID := gittest.WriteCommit(t, cfg, repoPath)
	for _, ref := range []git.ReferenceName{"refs/heads/master", "refs/tags/v1.0.0", "refs/heads/'test'", "refs/heads/ʕ•ᴥ•ʔ"} {
		gittest.WriteRef(t, cfg, repoPath, ref, commitID)
	}

	for _, tc := range []struct {
		desc             string
		request          *gitalypb.RefExistsRequest
		expectedResponse *gitalypb.RefExistsResponse
		expectedErr      error
	}{
		{
			desc: "repository not provided",
			request: &gitalypb.RefExistsRequest{
				Repository: nil,
			},
			expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
		},
		{
			desc: "invalid ref name",
			request: &gitalypb.RefExistsRequest{
				Repository: repo,
				Ref:        []byte("in valid"),
			},
			expectedErr: structerr.NewInvalidArgument("invalid refname"),
		},
		{
			desc: "master",
			request: &gitalypb.RefExistsRequest{
				Repository: repo,
				Ref:        []byte("refs/heads/master"),
			},
			expectedResponse: &gitalypb.RefExistsResponse{
				Value: true,
			},
		},
		{
			desc: "v1.0.0",
			request: &gitalypb.RefExistsRequest{
				Repository: repo,
				Ref:        []byte("refs/tags/v1.0.0"),
			},
			expectedResponse: &gitalypb.RefExistsResponse{
				Value: true,
			},
		},
		{
			desc: "quoted",
			request: &gitalypb.RefExistsRequest{
				Repository: repo,
				Ref:        []byte("refs/heads/'test'"),
			},
			expectedResponse: &gitalypb.RefExistsResponse{
				Value: true,
			},
		},
		{
			desc: "unicode exists",
			request: &gitalypb.RefExistsRequest{
				Repository: repo,
				Ref:        []byte("refs/heads/ʕ•ᴥ•ʔ"),
			},
			expectedResponse: &gitalypb.RefExistsResponse{
				Value: true,
			},
		},
		{
			desc: "unicode missing",
			request: &gitalypb.RefExistsRequest{
				Repository: repo,
				Ref:        []byte("refs/tags/अस्तित्वहीन"),
			},
			expectedResponse: &gitalypb.RefExistsResponse{
				Value: false,
			},
		},
		{
			desc: "spaces",
			request: &gitalypb.RefExistsRequest{
				Repository: repo,
				Ref:        []byte("refs/ /heads"),
			},
			expectedResponse: &gitalypb.RefExistsResponse{
				Value: false,
			},
		},
		{
			desc: "haxxors",
			request: &gitalypb.RefExistsRequest{
				Repository: repo,
				Ref:        []byte("refs/; rm -rf /tmp/*"),
			},
			expectedResponse: &gitalypb.RefExistsResponse{
				Value: false,
			},
		},
		{
			desc: "dashes",
			request: &gitalypb.RefExistsRequest{
				Repository: repo,
				Ref:        []byte("--"),
			},
			expectedErr: structerr.NewInvalidArgument("invalid refname"),
		},
		{
			desc: "blank",
			request: &gitalypb.RefExistsRequest{
				Repository: repo,
				Ref:        []byte(""),
			},
			expectedErr: structerr.NewInvalidArgument("invalid refname"),
		},
		{
			desc: "not tags or branches",
			request: &gitalypb.RefExistsRequest{
				Repository: repo,
				Ref:        []byte("refs/heads/remotes/origin"),
			},
			expectedResponse: &gitalypb.RefExistsResponse{
				Value: false,
			},
		},
		{
			desc: "wildcards",
			request: &gitalypb.RefExistsRequest{
				Repository: repo,
				Ref:        []byte("refs/heads/*"),
			},
			expectedResponse: &gitalypb.RefExistsResponse{
				Value: false,
			},
		},
		{
			desc: "invalid repos",
			request: &gitalypb.RefExistsRequest{
				Repository: &gitalypb.Repository{
					StorageName:  "invalid",
					RelativePath: "/etc/",
				},
				Ref: []byte("refs/heads/master"),
			},
			expectedErr: testhelper.ToInterceptedMetadata(structerr.NewInvalidArgument(
				"%w", storage.NewStorageNotFoundError("invalid"),
			)),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			response, err := client.RefExists(ctx, tc.request)
			testhelper.RequireGrpcError(t, tc.expectedErr, err)
			testhelper.ProtoEqual(t, tc.expectedResponse, response)
		})
	}

	expectedDanglingErr := func(ref git.ReferenceName, oid git.ObjectID) error {
		return testhelper.ToInterceptedMetadata(
			structerr.NewFailedPrecondition(
				"reference %q points to missing object %s", ref, oid,
			).
				WithDetail(&gitalypb.RefExistsError{
					Error: &gitalypb.RefExistsError_UnresolvableReference{
						UnresolvableReference: &gitalypb.ReferenceUnresolvableError{
							ReferenceName: []byte(ref),
							Oid:           oid.String(),
						},
					},
				}).
				WithMetadata("reference", string(ref)),
		)
	}

	setupDanglingRepo := func(t *testing.T, pack bool) (*gitalypb.Repository, git.ReferenceName, git.ObjectID) {
		danglingRepo, danglingRepoPath := gittest.CreateRepository(t, ctx, cfg)
		// Ensure the repo is valid/non-empty.
		gittest.WriteCommit(t, cfg, danglingRepoPath, gittest.WithBranch("main"))

		commitID := gittest.WriteCommit(t, cfg, danglingRepoPath, gittest.WithMessage("dangling"))
		ref := git.ReferenceName("refs/keep-around/" + commitID.String())
		gittest.WriteRef(t, cfg, danglingRepoPath, ref, commitID)
		if pack {
			gittest.Exec(t, cfg, "-C", danglingRepoPath, "pack-refs", "--all")
		}
		// Remove the object so the reference dangles.
		hex := commitID.String()
		require.NoError(t, os.Remove(filepath.Join(danglingRepoPath, "objects", hex[0:2], hex[2:])))
		return danglingRepo, ref, commitID
	}

	t.Run("dangling reference", func(t *testing.T) {
		t.Parallel()
		repo, ref, oid := setupDanglingRepo(t, false)
		response, err := client.RefExists(ctx, &gitalypb.RefExistsRequest{Repository: repo, Ref: []byte(ref)})
		testhelper.RequireGrpcError(t, expectedDanglingErr(ref, oid), err)
		testhelper.ProtoEqual(t, (*gitalypb.RefExistsResponse)(nil), response)
	})

	t.Run("packed dangling reference", func(t *testing.T) {
		t.Parallel()
		repo, ref, oid := setupDanglingRepo(t, true)
		response, err := client.RefExists(ctx, &gitalypb.RefExistsRequest{Repository: repo, Ref: []byte(ref)})
		testhelper.RequireGrpcError(t, expectedDanglingErr(ref, oid), err)
		testhelper.ProtoEqual(t, (*gitalypb.RefExistsResponse)(nil), response)
	})
}
