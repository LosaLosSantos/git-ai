package derived

import (
	"bytes"
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// lastCommitPaths are the paths the LastCommitForPath equivalence test queries.
// "a*" is a pathspec glob, which is answered by git log rather than derived data.
var lastCommitPaths = []string{"a", "b", "renamed", "missing", ".", "a*", "a/", "dir/", "dir/x/", "dir/x", "dir//x", "./a"}

// TestLastCommitForPath_MatchesGitForRevisionsNamingSeveralCommits covers revisions
// that are ranges or select several commits. Git log answers them, and the derived
// data must not turn them into an empty response.
func TestLastCommitForPath_MatchesGitForRevisionsNamingSeveralCommits(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, enabled := setupServices(t, testcfg.Build(t))

	disabledCfg := testcfg.Build(t)
	disabledCfg.Storages = cfg.Storages
	disabledCfg.Graft.Derived.Disabled = true
	_, disabled := setupServices(t, disabledCfg)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	writeHistory(t, cfg, repoPath, mergeHistory())
	for _, revision := range []string{"root..merge", "root...side", "merge^@", "merge^!", "merge^-", "main^-"} {
		for _, path := range []string{"a", "b", "dir", "dir/x", "renamed", ".", "", "/"} {
			gitPath := path
			if gitPath == "" || gitPath == "/" {
				gitPath = "."
			}
			want := string(bytes.TrimSpace(gittest.Exec(t, cfg, "-C", repoPath, "log", "-1", "--format=%H", revision, "--", gitPath)))
			if revision == "root..merge" && path == "a" {
				// Guards against the test passing because both sides are empty.
				require.NotEmpty(t, want)
			}

			for _, svc := range []services{enabled, disabled} {
				response, err := svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
					Repository: repoProto,
					Revision:   []byte(revision),
					Path:       []byte(path),
				})
				require.NoError(t, err)
				require.Equal(t, want, response.GetCommit().GetId(), "last commit of %q at %q", path, revision)
			}
		}
	}
}

func TestLastCommitForPath_MatchesGitWithAndWithoutDerivedData(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, enabled := setupServices(t, testcfg.Build(t))

	// The second server has its own runtime directory but shares the storage, so that both
	// servers read the same repositories.
	disabledCfg := testcfg.Build(t)
	disabledCfg.Storages = cfg.Storages
	disabledCfg.Graft.Derived.Disabled = true
	_, disabled := setupServices(t, disabledCfg)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commits := mergeHistory()
	ids := writeHistory(t, cfg, repoPath, commits)

	for _, commit := range commits {
		for _, path := range lastCommitPaths {
			want := gitLastMod(t, cfg, repoPath, ids[commit.name], path)

			for _, svc := range []services{enabled, disabled} {
				response, err := svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
					Repository: repoProto,
					Revision:   []byte(commit.name),
					Path:       []byte(path),
				})
				require.NoError(t, err)

				require.Equal(t, want, response.GetCommit().GetId(), "last commit of %q at %s", path, commit.name)
			}
		}
	}
}

func TestLastCommitForPath_UsesStoredLastMod(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := writeHistory(t, cfg, repoPath, mergeHistory())

	// The store says that "b" last changed in "root" as of "merge", where git log says
	// "main". Nothing else may influence the answer.
	require.Equal(t, ids["main"].String(), gitLastMod(t, cfg, repoPath, ids["merge"], "b"))
	plantLastMod(t, repoPath, ids["merge"], "b", ids["root"])

	response, err := svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
		Repository: repoProto,
		Revision:   []byte("merge"),
		Path:       []byte("b"),
	})
	require.NoError(t, err)
	require.Equal(t, ids["root"].String(), response.GetCommit().GetId())
}

func TestLastCommitForPath_PathsMissingAtRevisionUseGitAndWriteNothing(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := writeHistory(t, cfg, repoPath, mergeHistory())

	// Any reader can send any path name. Paths that do not exist at the revision,
	// including deleted ones, are answered by git log and must not create store entries.
	for i := 0; i < 5; i++ {
		response, err := svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
			Repository: repoProto,
			Revision:   []byte("delete"),
			Path:       []byte(fmt.Sprintf("does-not-exist-%d", i)),
		})
		require.NoError(t, err)
		require.Nil(t, response.GetCommit())
	}

	// A deleted path is attributed to the deletion, as git log does.
	response, err := svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
		Repository: repoProto,
		Revision:   []byte("delete"),
		Path:       []byte("a"),
	})
	require.NoError(t, err)
	require.Equal(t, ids["rename"].String(), response.GetCommit().GetId())

	require.NoDirExists(t, filepath.Join(repoPath, "graft"))
}

func TestLastCommitForPath_LongHistoryStoresBoundedEntries(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	const length = 2000
	ids := writeHistory(t, cfg, repoPath, linearHistory(length, func(i int) string {
		if i == 0 {
			return "a"
		}
		return ""
	}))

	for _, revision := range []string{"c1999", "c1500", "c1000"} {
		response, err := svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
			Repository: repoProto,
			Revision:   []byte(revision),
			Path:       []byte("a"),
		})
		require.NoError(t, err)
		require.Equal(t, ids["c0"].String(), response.GetCommit().GetId())
	}

	// A walk over 2,000 commits stores a checkpoint every 64 commits, not one entry per
	// commit, and the later queries only add their own start.
	var files int
	require.NoError(t, filepath.WalkDir(filepath.Join(repoPath, "graft"), func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files++
		}
		return nil
	}))
	require.NotZero(t, files)
	require.LessOrEqual(t, files, length/64+2+2)
}

func TestLastCommitForPath_NotFoundIsEmptyResponse(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	writeHistory(t, cfg, repoPath, mergeHistory())

	for _, tc := range []struct {
		desc     string
		revision string
		path     string
	}{
		{desc: "missing path", revision: "delete", path: "never-existed"},
		{desc: "unknown revision", revision: "does-not-exist", path: "a"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			response, err := svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
				Repository: repoProto,
				Revision:   []byte(tc.revision),
				Path:       []byte(tc.path),
			})
			require.NoError(t, err)
			require.Nil(t, response.GetCommit())
		})
	}
}

func TestLastCommitForPath_InvalidRequests(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	writeHistory(t, cfg, repoPath, mergeHistory())

	_, err := svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
		Repository: repoProto,
		Revision:   []byte("--output=/tmp/x"),
		Path:       []byte("a"),
	})
	testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)

	_, err = svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
		Repository: repoProto,
		Revision:   []byte("delete"),
		Path:       []byte("../a"),
	})
	testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
}

func TestLastCommitForPath_DisabledWritesNothing(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Derived.Disabled = true
	cfg, svc := setupServices(t, cfg)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := writeHistory(t, cfg, repoPath, linearHistory(3, func(i int) string { return fmt.Sprintf("file%d", i) }))

	response, err := svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
		Repository: repoProto,
		Revision:   []byte("c2"),
		Path:       []byte("file1"),
	})
	require.NoError(t, err)
	require.Equal(t, ids["c1"].String(), response.GetCommit().GetId())

	require.NoDirExists(t, filepath.Join(repoPath, "graft"))
}
