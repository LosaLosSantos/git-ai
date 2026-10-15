package derived

import (
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	commitservice "github.com/feder-cr/open_scion/internal/gitaly/service/commit"
	"github.com/feder-cr/open_scion/internal/gitaly/service/repository"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestEmptyRootCommit covers a repository whose first commit has the empty tree,
// as `git commit --allow-empty` creates it. Git never reports such a commit as
// changing the root directory.
func TestEmptyRootCommit(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, enabled := setupServices(t, testcfg.Build(t))

	disabledCfg := testcfg.Build(t)
	disabledCfg.Storages = cfg.Storages
	disabledCfg.Graft.Derived.Disabled = true
	_, disabled := setupServices(t, disabledCfg)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	root := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("rootonly"), gittest.WithMessage("root"))
	child := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithParents(root),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a\n"}))

	for _, revision := range []string{"rootonly", "main"} {
		for _, path := range []string{"", "/", ".", "a"} {
			gitPath := path
			if gitPath == "" || gitPath == "/" {
				gitPath = "."
			}
			want := gitLastMod(t, cfg, repoPath, gittest.ResolveRevision(t, cfg, repoPath, revision), gitPath)
			if revision == "main" {
				require.Equal(t, child.String(), want)
			} else {
				require.Empty(t, want)
			}

			for _, svc := range []services{enabled, disabled} {
				response, err := svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
					Repository: repoProto,
					Revision:   []byte(revision),
					Path:       []byte(path),
				})
				require.NoError(t, err)
				require.Equal(t, want, response.GetCommit().GetId(), "last commit of %q at %s", path, revision)
			}

			wantHistory := gitFirstParentLog(t, cfg, repoPath, gittest.ResolveRevision(t, cfg, repoPath, revision), gitPath)
			got := flatten(readPathHistory(t, ctx, enabled.derived, &gitalypb.PathHistoryRequest{
				Repository: repoProto,
				Revision:   []byte(revision),
				Path:       []byte(path),
				Limit:      100,
			}))
			require.Equal(t, wantHistory, got, "path history of %q at %s", path, revision)
		}
	}

	require.Equal(t, []string{child.String()}, gitFirstParentLog(t, cfg, repoPath, child, "."))
}

// TestLastCommitForPath_UnknownRevisionDoesNotWarn makes sure that a revision that
// does not exist, which is common client traffic, falls back to git log without
// being logged as a failure of the derived data.
func TestLastCommitForPath_UnknownRevisionDoesNotWarn(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	logger := testhelper.NewLogger(t)
	hook := testhelper.AddLoggerHook(logger)

	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterCommitServiceServer(srv, commitservice.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	}, testserver.WithLogger(logger))

	conn, err := client.New(ctx, cfg.SocketPath)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	commitClient := gitalypb.NewCommitServiceClient(conn)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a\n"}))

	hook.Reset()
	response, err := commitClient.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
		Repository: repoProto,
		Revision:   []byte("does-not-exist"),
		Path:       []byte("a"),
	})
	require.NoError(t, err)
	require.Nil(t, response.GetCommit())

	for _, entry := range hook.AllEntries() {
		require.False(t, strings.HasPrefix(entry.Message, "derived:"), "unexpected log entry %q", entry.Message)
	}
}
