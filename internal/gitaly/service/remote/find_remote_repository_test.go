package remote

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestFindRemoteRepository(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRemoteService(t, ctx)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)

	type setupData struct {
		request          *gitalypb.FindRemoteRepositoryRequest
		expectedErr      error
		expectedResponse *gitalypb.FindRemoteRepositoryResponse
	}

	for _, tc := range []struct {
		desc  string
		setup func(t *testing.T) setupData
	}{
		{
			desc: "empty remote",
			setup: func(t *testing.T) setupData {
				return setupData{
					request: &gitalypb.FindRemoteRepositoryRequest{
						Remote:      "",
						StorageName: cfg.Storages[0].Name,
					},
					expectedErr: structerr.NewInvalidArgument("empty remote can't be checked."),
				}
			},
		},
		{
			desc: "nonexistent remote repository",
			setup: func(t *testing.T) setupData {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(404)
				}))
				t.Cleanup(server.Close)

				return setupData{
					request: &gitalypb.FindRemoteRepositoryRequest{
						Remote:      server.URL + "/does-not-exist.git",
						StorageName: cfg.Storages[0].Name,
					},
					expectedResponse: &gitalypb.FindRemoteRepositoryResponse{},
				}
			},
		},
		{
			desc: "successful",
			setup: func(t *testing.T) setupData {
				_, remoteRepoPath := gittest.CreateRepository(t, ctx, cfg)
				gittest.WriteCommit(t, cfg, remoteRepoPath, gittest.WithBranch("main"))

				port := gittest.HTTPServer(t, ctx, gitCmdFactory, remoteRepoPath, nil)

				return setupData{
					request: &gitalypb.FindRemoteRepositoryRequest{
						Remote:      fmt.Sprintf("http://127.0.0.1:%d/%s", port, filepath.Base(remoteRepoPath)),
						StorageName: cfg.Storages[0].Name,
					},
					expectedResponse: &gitalypb.FindRemoteRepositoryResponse{
						Exists: true,
					},
				}
			},
		},
		{
			desc: "successful with resolved address",
			setup: func(t *testing.T) setupData {
				_, remoteRepoPath := gittest.CreateRepository(t, ctx, cfg)
				gittest.WriteCommit(t, cfg, remoteRepoPath, gittest.WithBranch("main"))

				port := gittest.HTTPServer(t, ctx, gitCmdFactory, remoteRepoPath, nil)

				return setupData{
					request: &gitalypb.FindRemoteRepositoryRequest{
						Remote:          fmt.Sprintf("http://localhost:%d/%s", port, filepath.Base(remoteRepoPath)),
						StorageName:     cfg.Storages[0].Name,
						ResolvedAddress: "127.0.0.1",
					},
					expectedResponse: &gitalypb.FindRemoteRepositoryResponse{
						Exists: true,
					},
				}
			},
		},
		{
			desc: "invalid resolved address",
			setup: func(t *testing.T) setupData {
				return setupData{
					request: &gitalypb.FindRemoteRepositoryRequest{
						Remote:          "https://gitlab.com/foo/bar.git",
						StorageName:     cfg.Storages[0].Name,
						ResolvedAddress: "foo/bar",
					},
					expectedErr: structerr.NewInvalidArgument("couldn't get curloptResolve config: resolved address has invalid IPv4/IPv6 address"),
				}
			},
		},
		{
			desc: "other references ending in HEAD",
			setup: func(t *testing.T) setupData {
				_, remoteRepoPath := gittest.CreateRepository(t, ctx, cfg)
				commitID := gittest.WriteCommit(t, cfg, remoteRepoPath, gittest.WithBranch("main"))
				// git-ls-remote(1) matches HEAD against the tail of each reference name, so it
				// lists these references next to HEAD.
				gittest.WriteRef(t, cfg, remoteRepoPath, "refs/heads/feature/HEAD", commitID)
				gittest.WriteRef(t, cfg, remoteRepoPath, "refs/remotes/origin/HEAD", commitID)

				port := gittest.HTTPServer(t, ctx, gitCmdFactory, remoteRepoPath, nil)

				return setupData{
					request: &gitalypb.FindRemoteRepositoryRequest{
						Remote:      fmt.Sprintf("http://127.0.0.1:%d/%s", port, filepath.Base(remoteRepoPath)),
						StorageName: cfg.Storages[0].Name,
					},
					expectedResponse: &gitalypb.FindRemoteRepositoryResponse{
						Exists: true,
					},
				}
			},
		},
		{
			desc: "unborn HEAD with other references ending in HEAD",
			setup: func(t *testing.T) setupData {
				_, remoteRepoPath := gittest.CreateRepository(t, ctx, cfg)
				// HEAD points to the default branch, which is never written, so git-ls-remote(1)
				// lists these references and no line for HEAD itself.
				commitID := gittest.WriteCommit(t, cfg, remoteRepoPath, gittest.WithBranch("feature/HEAD"))
				gittest.WriteRef(t, cfg, remoteRepoPath, "refs/remotes/origin/HEAD", commitID)

				port := gittest.HTTPServer(t, ctx, gitCmdFactory, remoteRepoPath, nil)

				return setupData{
					request: &gitalypb.FindRemoteRepositoryRequest{
						Remote:      fmt.Sprintf("http://127.0.0.1:%d/%s", port, filepath.Base(remoteRepoPath)),
						StorageName: cfg.Storages[0].Name,
					},
					expectedResponse: &gitalypb.FindRemoteRepositoryResponse{},
				}
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			setup := tc.setup(t)

			response, err := client.FindRemoteRepository(ctx, setup.request)
			testhelper.RequireGrpcError(t, setup.expectedErr, err)
			testhelper.ProtoEqual(t, setup.expectedResponse, response)
		})
	}
}

func TestRemoteHeadExists(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc     string
		output   string
		expected bool
	}{
		{
			desc:     "SHA1 object ID",
			output:   git.ObjectHashSHA1.EmptyTreeOID.String() + "\tHEAD\n",
			expected: true,
		},
		{
			desc:     "SHA256 object ID",
			output:   git.ObjectHashSHA256.EmptyTreeOID.String() + "\tHEAD\n",
			expected: true,
		},
		{
			desc: "HEAD followed by other references ending in HEAD",
			output: git.ObjectHashSHA1.EmptyTreeOID.String() + "\tHEAD\n" +
				git.ObjectHashSHA1.EmptyTreeOID.String() + "\trefs/heads/feature/HEAD\n" +
				git.ObjectHashSHA1.EmptyTreeOID.String() + "\trefs/remotes/origin/HEAD\n",
			expected: true,
		},
		{
			desc: "HEAD after other references ending in HEAD",
			output: git.ObjectHashSHA1.EmptyTreeOID.String() + "\trefs/remotes/origin/HEAD\n" +
				git.ObjectHashSHA1.EmptyTreeOID.String() + "\tHEAD\n",
			expected: true,
		},
		{
			desc:   "only other references ending in HEAD",
			output: git.ObjectHashSHA1.EmptyTreeOID.String() + "\trefs/remotes/origin/HEAD\n",
		},
		{
			desc:   "object ID of unknown length",
			output: "4b825dc\tHEAD\n",
		},
		{
			desc:   "empty output",
			output: "",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.expected, remoteHeadExists([]byte(tc.output)))
		})
	}
}
