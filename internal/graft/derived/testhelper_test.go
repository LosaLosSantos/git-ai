package derived

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

// fileChange is one change to a path in a commit created by writeHistory.
type fileChange struct {
	path    string
	mode    string // defaults to 100644
	content string
	delete  bool
}

// historyCommit describes one commit of a history created by writeHistory. The
// first parent is the tree the changes are applied to; parents must be listed
// before the commits that reference them.
type historyCommit struct {
	name    string
	parents []string
	changes []fileChange
}

// writeHistory creates the commits in the repository with git fast-import and
// returns the object ID of every commit by name. Each commit is also reachable
// as refs/heads/<name>.
func writeHistory(tb testing.TB, cfg config.Cfg, repoPath string, commits []historyCommit) map[string]git.ObjectID {
	tb.Helper()

	marks := make(map[string]int, len(commits))
	var stream bytes.Buffer

	for i, commit := range commits {
		marks[commit.name] = i + 1

		fmt.Fprintf(&stream, "commit refs/heads/%s\n", commit.name)
		fmt.Fprintf(&stream, "mark :%d\n", i+1)
		fmt.Fprintf(&stream, "committer Tester <tester@example.com> %d +0000\n", 1600000000+i)
		fmt.Fprintf(&stream, "data %d\n%s\n", len(commit.name), commit.name)

		for j, parent := range commit.parents {
			mark, ok := marks[parent]
			require.True(tb, ok, "parent %q must be defined before %q", parent, commit.name)
			if j == 0 {
				fmt.Fprintf(&stream, "from :%d\n", mark)
			} else {
				fmt.Fprintf(&stream, "merge :%d\n", mark)
			}
		}

		for _, change := range commit.changes {
			if change.delete {
				fmt.Fprintf(&stream, "D %s\n", change.path)
				continue
			}

			mode := change.mode
			if mode == "" {
				mode = "100644"
			}
			fmt.Fprintf(&stream, "M %s inline %s\n", mode, change.path)
			fmt.Fprintf(&stream, "data %d\n%s\n", len(change.content), change.content)
		}

		stream.WriteString("\n")
	}

	gittest.ExecOpts(tb, cfg, gittest.ExecConfig{Stdin: &stream}, "-C", repoPath, "fast-import", "--quiet")

	ids := make(map[string]git.ObjectID, len(commits))
	for _, commit := range commits {
		ids[commit.name] = gittest.ResolveRevision(tb, cfg, repoPath, "refs/heads/"+commit.name)
	}

	return ids
}

// linearHistory returns n commits on the branch "main", where commit i (0-based)
// writes "content i" to the file path given by pathOf(i) if it is not empty.
func linearHistory(n int, pathOf func(i int) string) []historyCommit {
	var commits []historyCommit
	for i := 0; i < n; i++ {
		commit := historyCommit{name: fmt.Sprintf("c%d", i)}
		if i > 0 {
			commit.parents = []string{fmt.Sprintf("c%d", i-1)}
		}
		if p := pathOf(i); p != "" {
			commit.changes = []fileChange{{path: p, content: fmt.Sprintf("content %d\n", i)}}
		} else {
			commit.changes = []fileChange{{path: "unrelated", content: fmt.Sprintf("unrelated %d\n", i)}}
		}
		commits = append(commits, commit)
	}
	return commits
}

// createRepo creates an empty bare repository in the first storage of cfg and
// returns it with its absolute path. The repository is created on disk without
// a running server.
func createRepo(tb testing.TB, cfg config.Cfg) (*gitalypb.Repository, string) {
	tb.Helper()

	storagePath := cfg.Storages[0].Path
	repo := gittest.InitRepoDir(tb, storagePath, gittest.NewRepositoryName(tb))
	repoPath := filepath.Join(storagePath, repo.GetRelativePath())

	// The object format follows the test configuration, so that the SHA256 CI job
	// runs these tests on SHA256 repositories.
	objectFormat := "sha1"
	if gittest.ObjectHashIsSHA256() {
		objectFormat = "sha256"
	}
	gittest.Exec(tb, cfg, "init", "--bare", "--quiet", "--object-format="+objectFormat, repoPath)

	return repo, repoPath
}

// newTestDeriver starts a Deriver for the repository. The object reader is
// released when the test ends.
func newTestDeriver(tb testing.TB, ctx context.Context, cfg config.Cfg, repo *localrepo.Repo, cache bool) *Deriver {
	tb.Helper()

	catfileCache := catfile.NewCache(cfg)
	tb.Cleanup(catfileCache.Stop)

	objectReader, cancel, err := catfileCache.ObjectReader(ctx, repo)
	require.NoError(tb, err)
	tb.Cleanup(cancel)

	deriver, err := NewDeriver(ctx, testhelper.NewLogger(tb), repo, objectReader, cache)
	require.NoError(tb, err)
	return deriver
}

// gitLastCommitForPath returns what `git log -1 -- path` prints for the commit
// (empty when no commit changed path).
func gitLastCommitForPath(tb testing.TB, cfg config.Cfg, repoPath string, commit git.ObjectID, path string) string {
	tb.Helper()
	out := gittest.Exec(tb, cfg, "-C", repoPath, "log", "-1", "--format=%H", commit.String(), "--", path)
	return string(bytes.TrimSpace(out))
}

// gitFirstParentPathHistory returns what `git log --first-parent -- path` prints
// for the commit, newest first.
func gitFirstParentPathHistory(tb testing.TB, cfg config.Cfg, repoPath string, commit git.ObjectID, path string) []string {
	tb.Helper()
	out := gittest.Exec(tb, cfg, "-C", repoPath, "log", "--first-parent", "--format=%H", commit.String(), "--", path)
	return splitLines(out)
}

func splitLines(out []byte) []string {
	var lines []string
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) > 0 {
			lines = append(lines, string(line))
		}
	}
	return lines
}
