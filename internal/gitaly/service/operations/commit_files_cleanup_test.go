package operations

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestUserCommitFilesReleasesProcessesAndFiles checks that a UserCommitFiles request, whether it
// fails, panics or succeeds, gives back its object reader and stops its tree writer, and leaves no
// temporary tree object file in the storage (PLAN9, WP96): once the cache evicts its processes,
// no cat-file process is left running.
func TestUserCommitFilesReleasesProcessesAndFiles(t *testing.T) {
	t.Parallel()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	var processCache *catfile.ProcessCache
	// panicNext makes the next request panic once it holds its object reader.
	var panicNext atomic.Bool
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		processCache = deps.GetCatfileCache().(*catfile.ProcessCache)
		operationServer := NewServer(deps)
		operationServer.testHook = func(point string) {
			if point == "UserCommitFiles: reader taken" && panicNext.CompareAndSwap(true, false) {
				panic("injected")
			}
		}
		gitalypb.RegisterOperationServiceServer(srv, operationServer)
		gitalypb.RegisterHookServiceServer(srv, hook.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	})
	client, conn := newOperationClient(t, cfg.SocketPath)
	t.Cleanup(func() { conn.Close() })
	ctx = testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"),
		gittest.WithTreeEntries(gittest.TreeEntry{Path: "README", Mode: "100644", Content: "readme\n"}))

	tempDir, err := config.NewLocator(cfg).TempDir(cfg.Storages[0].Name)
	require.NoError(t, err)

	for _, tc := range []struct {
		desc        string
		panics      bool
		startBranch string
		requests    []*gitalypb.UserCommitFilesRequest
		expectedErr string
	}{
		{
			desc:        "a panic while the request holds its object reader",
			panics:      true,
			requests:    []*gitalypb.UserCommitFilesRequest{createFileHeaderRequest("file"), actionContentRequest("content\n")},
			expectedErr: "injected",
		},
		{
			desc:        "a file to delete that does not exist",
			requests:    []*gitalypb.UserCommitFilesRequest{deleteFileHeaderRequest("missing.txt")},
			expectedErr: "A file with this name doesn't exist",
		},
		{
			desc:        "a file below a file",
			requests:    []*gitalypb.UserCommitFilesRequest{createFileHeaderRequest("README/file"), actionContentRequest("content\n")},
			expectedErr: "A file with this name already exists",
		},
		{
			desc:        "a start branch that does not exist",
			startBranch: "missing",
			requests:    []*gitalypb.UserCommitFilesRequest{createFileHeaderRequest("file"), actionContentRequest("content\n")},
			expectedErr: "resolve parent commit",
		},
		{
			desc:     "success",
			requests: []*gitalypb.UserCommitFilesRequest{createFileHeaderRequest("dir/file"), actionContentRequest("content\n")},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			panicNext.Store(tc.panics)
			stream, err := client.UserCommitFiles(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(headerRequest(repo, gittest.TestUser, "master", []byte("message"), tc.startBranch, "")))
			for _, request := range tc.requests {
				require.NoError(t, stream.Send(request))
			}
			_, err = stream.CloseAndRecv()
			if tc.expectedErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.expectedErr)
			}

			// Every process the request took was given back to the cache or closed: evicting
			// the cache stops them all.
			processCache.Evict()
			require.NoError(t, testutil.CollectAndCompare(processCache, strings.NewReader(`
# HELP gitaly_catfile_processes Gauge of active catfile processes
# TYPE gitaly_catfile_processes gauge
gitaly_catfile_processes 0
`), "gitaly_catfile_processes"))

			entries, err := os.ReadDir(tempDir)
			require.NoError(t, err)
			for _, entry := range entries {
				require.NotContains(t, entry.Name(), "tree-object-")
			}
		})
	}
}
