package derived

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// disabledServices starts a second server that shares the storage of cfg but has
// the derived data disabled, so that its answers come from Git alone.
func disabledServices(t *testing.T, cfg config.Cfg) services {
	t.Helper()

	disabledCfg := testcfg.Build(t)
	disabledCfg.Storages = cfg.Storages
	disabledCfg.Graft.Derived.Disabled = true
	_, disabled := setupServices(t, disabledCfg)
	return disabled
}

func TestLastCommitForPath_MatchesGitForDirectoriesAndNonCanonicalPaths(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, enabled := setupServices(t, testcfg.Build(t))
	disabled := disabledServices(t, cfg)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commits := directoryHistory()
	ids := writeHistory(t, cfg, repoPath, commits)

	// Git normalizes paths with a trailing slash, "." or ".." components before it
	// matches them. "a/." names the directory "a/" for example, which never matches
	// the file "a", so the answer must be empty rather than the last commit of "a".
	paths := []string{
		"a", "a/", "a/.", "./a/.", "a/x/..", "a/./", "a/x/../a",
		"dir", "dir/", "dir/.", "dir/x", "dir/y", "dir/x/.", "dir/x/..", "dir//x", "./dir", "dir/..",
		".", "missing", "missing/.",
	}

	nonEmpty := 0
	for _, commit := range commits {
		for _, path := range paths {
			want := gitLastMod(t, cfg, repoPath, ids[commit.name], path)
			if want != "" {
				nonEmpty++
			}

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

	// Guards against the test passing because git and derived data both answer empty.
	require.Greater(t, nonEmpty, len(commits))
}

func TestLastCommitForPath_NULPathIsInternalLikeGit(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, enabled := setupServices(t, testcfg.Build(t))
	disabled := disabledServices(t, cfg)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	writeHistory(t, cfg, repoPath, mergeHistory())

	for _, svc := range []services{enabled, disabled} {
		_, err := svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
			Repository: repoProto,
			Revision:   []byte("root"),
			Path:       []byte("a\x00b"),
		})
		testhelper.RequireGrpcCode(t, err, codes.Internal)
	}
}

func TestDerivedRPCs_NonCanonicalPathsAreInvalidArgument(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	writeHistory(t, cfg, repoPath, mergeHistory())

	// "a" is a file at root, so these name a file with a trailing slash or a "." or
	// ".." component. The derived RPCs look up literal paths and reject them.
	for _, path := range []string{"a/", "a/.", "a/x/..", "./a", "a//b", "b/."} {
		t.Run(path, func(t *testing.T) {
			_, err := drainPathHistory(ctx, svc.derived, &gitalypb.PathHistoryRequest{
				Repository: repoProto,
				Revision:   []byte("root"),
				Path:       []byte(path),
			})
			testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)

			_, err = drainBlame(ctx, svc.derived, &gitalypb.BlameRequest{
				Repository: repoProto,
				Revision:   []byte("root"),
				Path:       []byte(path),
			})
			testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
		})
	}
}

func TestDerivedRPCs_RevisionsNamingSeveralCommitsAreInvalidArgument(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	writeHistory(t, cfg, repoPath, mergeHistory())

	for _, revision := range []string{"^merge", "root..merge", "root...side", "merge^@", "merge^!", "merge^-"} {
		t.Run(revision, func(t *testing.T) {
			_, err := drainPathHistory(ctx, svc.derived, &gitalypb.PathHistoryRequest{
				Repository: repoProto,
				Revision:   []byte(revision),
				Path:       []byte("a"),
			})
			testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)

			_, err = drainBlame(ctx, svc.derived, &gitalypb.BlameRequest{
				Repository: repoProto,
				Revision:   []byte(revision),
				Path:       []byte("a"),
			})
			testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
		})
	}
}

// TestBlame_MatchesGitWhenOnlyTheModeDiffersBetweenParents covers a merge whose
// parents hold the same blob, one with a different mode. git blame passes the blame
// to the first parent with the same blob and ignores the mode, so the blame must not
// be taken from the other parent.
func TestBlame_MatchesGitWhenOnlyTheModeDiffersBetweenParents(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := writeHistory(t, cfg, repoPath, []historyCommit{
		{name: "root", changes: []fileChange{{path: "f", content: "a\n"}}},
		{name: "b1", parents: []string{"root"}, changes: []fileChange{{path: "f", content: "b\n"}}},
		{name: "b2", parents: []string{"root"}, changes: []fileChange{{path: "f", content: "b\n", mode: "100755"}}},
		{name: "mm", parents: []string{"b1", "b2"}, changes: []fileChange{{path: "f", content: "b\n", mode: "100755"}}},
	})

	want, err := derived.ParsePorcelain(gittest.Exec(t, cfg, "-C", repoPath, "blame", "--porcelain", "mm", "--", "f"))
	require.NoError(t, err)
	require.Equal(t, ids["b1"].String(), want[0].CommitID, "git blame attributes the line to the first parent with the same blob")

	responses := readBlame(t, ctx, svc.derived, &gitalypb.BlameRequest{
		Repository: repoProto,
		Revision:   []byte("mm"),
		Path:       []byte("f"),
	})
	require.Equal(t, want, blameLines(responses))
}

// TestBlame_MatchesGitAcrossModeOnlyChanges checks that blame stays equal to git
// blame when a mode-only change sits between the blob's origin and the revision.
func TestBlame_MatchesGitAcrossModeOnlyChanges(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	writeHistory(t, cfg, repoPath, []historyCommit{
		{name: "root", changes: []fileChange{{path: "f", content: "a\nb\n"}}},
		{name: "exec", parents: []string{"root"}, changes: []fileChange{{path: "f", content: "a\nb\n", mode: "100755"}}},
		{name: "edit", parents: []string{"exec"}, changes: []fileChange{{path: "f", content: "a\nB\n", mode: "100755"}}},
		{name: "merge", parents: []string{"edit", "root"}, changes: []fileChange{{path: "f", content: "a\nB\n", mode: "100755"}}},
	})

	for _, revision := range []string{"exec", "edit", "merge"} {
		t.Run(revision, func(t *testing.T) {
			want, err := derived.ParsePorcelain(gittest.Exec(t, cfg, "-C", repoPath, "blame", "--porcelain", revision, "--", "f"))
			require.NoError(t, err)

			responses := readBlame(t, ctx, svc.derived, &gitalypb.BlameRequest{
				Repository: repoProto,
				Revision:   []byte(revision),
				Path:       []byte("f"),
			})
			require.Equal(t, want, blameLines(responses))
		})
	}
}
