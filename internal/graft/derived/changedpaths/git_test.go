package changedpaths

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// universe holds the paths that random commits change. No path is a prefix of
// another, so every path is either a file or a directory, never both.
var universe = []string{"a/b/c", "a/b/x", "a/d", "e/f/g", "e/h", "h/i/j", "top"}

const zeroOID = "0000000000000000000000000000000000000000"

// runGit runs git in dir and returns its standard output. The command goes through
// gittest, which runs the bundled Git binary that the tests are configured for.
func runGit(t testing.TB, cfg config.Cfg, dir string, env []string, stdin string, args ...string) string {
	t.Helper()

	return string(gittest.ExecOpts(t, cfg, gittest.ExecConfig{
		Stdin: strings.NewReader(stdin),
		Env:   env,
	}, append([]string{"-C", dir}, args...)...))
}

// gitObjects reads commits and trees of a repository with the git command.
type gitObjects struct {
	t       *testing.T
	cfg     config.Cfg
	dir     string
	commits map[string]Commit
	trees   map[string][]Entry
}

func (g *gitObjects) Commit(_ context.Context, id string) (Commit, error) {
	if commit, ok := g.commits[id]; ok {
		return commit, nil
	}

	out := runGit(g.t, g.cfg, g.dir, nil, "", "cat-file", "commit", id)
	commit := Commit{ID: id}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			break // the header ends at the first empty line
		}
		switch {
		case strings.HasPrefix(line, "tree "):
			commit.Tree = strings.TrimPrefix(line, "tree ")
		case strings.HasPrefix(line, "parent "):
			commit.Parents = append(commit.Parents, strings.TrimPrefix(line, "parent "))
		}
	}

	g.commits[id] = commit
	return commit, nil
}

func (g *gitObjects) Tree(_ context.Context, id string) ([]Entry, error) {
	if id == "" {
		return nil, nil
	}
	if entries, ok := g.trees[id]; ok {
		return entries, nil
	}

	var entries []Entry
	out := runGit(g.t, g.cfg, g.dir, nil, "", "ls-tree", id)
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		meta, name, found := strings.Cut(line, "\t")
		require.True(g.t, found, "malformed ls-tree line %q", line)

		fields := strings.Fields(meta)
		mode, err := strconv.ParseUint(fields[0], 8, 32)
		require.NoError(g.t, err)
		entries = append(entries, Entry{Name: name, Mode: uint32(mode), OID: fields[2]})
	}

	g.trees[id] = entries
	return entries, nil
}

// buildRandomHistory creates count commits in the repository dir. Each commit
// applies a few random changes to the tree of its first parent. Some commits change
// nothing, some branch off earlier commits, and some merge two of them, so that
// first-parent and full histories differ.
func buildRandomHistory(t *testing.T, cfg config.Cfg, rng *rand.Rand, dir string, count int) []string {
	t.Helper()

	indexFile := filepath.Join(dir, "index")
	env := []string{
		"GIT_INDEX_FILE=" + indexFile,
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	}

	var blobs []string
	for _, content := range []string{"one\n", "two\n", "three\n"} {
		blobs = append(blobs, strings.TrimSpace(runGit(t, cfg, dir, nil, content, "hash-object", "-w", "--stdin")))
	}

	// applyChanges returns the tree that results from applying n random changes to
	// base, the tree of the first parent, or to an empty index when base is empty.
	applyChanges := func(base string, n int) string {
		if base == "" {
			require.NoError(t, os.RemoveAll(indexFile))
		} else {
			runGit(t, cfg, dir, env, "", "read-tree", base)
		}

		var changes strings.Builder
		for i := 0; i < n; i++ {
			path := universe[rng.Intn(len(universe))]
			if rng.Intn(4) == 0 {
				fmt.Fprintf(&changes, "0 %s\t%s\n", zeroOID, path)
				continue
			}
			mode := []string{"100644", "100755", "120000"}[rng.Intn(3)]
			fmt.Fprintf(&changes, "%s %s\t%s\n", mode, blobs[rng.Intn(len(blobs))], path)
		}
		if changes.Len() > 0 {
			runGit(t, cfg, dir, env, changes.String(), "update-index", "--index-info")
		}
		return strings.TrimSpace(runGit(t, cfg, dir, env, "", "write-tree"))
	}

	var ids []string
	treeOf := map[string]string{}
	for i := 0; i < count; i++ {
		var parents []string
		base := ""
		var changes int

		switch r := rng.Float64(); {
		case i == 0:
			changes = 1 + rng.Intn(3)
		case r < 0.6:
			parents = []string{ids[i-1]}
			changes = rng.Intn(3)
		case r < 0.8:
			parents = []string{ids[rng.Intn(i)]}
			changes = rng.Intn(3)
		default:
			first := ids[rng.Intn(i)]
			second := ids[rng.Intn(i)]
			for second == first && i > 1 {
				second = ids[rng.Intn(i)]
			}
			parents = []string{first, second}
			if first == second {
				parents = parents[:1]
			}
			changes = rng.Intn(2)
		}
		if len(parents) > 0 {
			base = treeOf[parents[0]]
		}

		tree := applyChanges(base, changes)
		args := []string{"commit-tree", tree, "-m", fmt.Sprintf("commit %d", i)}
		for _, p := range parents {
			args = append(args, "-p", p)
		}
		id := strings.TrimSpace(runGit(t, cfg, dir, env, "", args...))

		ids = append(ids, id)
		treeOf[id] = tree
	}

	return ids
}

// splitLines returns the non-empty lines of s, or nil when there are none.
func splitLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestIndex_MatchesGitFirstParentLog(t *testing.T) {
	t.Parallel()

	paths := append([]string{"a", "a/b", "e", "e/f", "h", "missing", "top/x"}, universe...)

	for seed := int64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()

			cfg := testcfg.Build(t)
			dir := t.TempDir()
			runGit(t, cfg, dir, nil, "", "init", "-q")
			commits := buildRandomHistory(t, cfg, rand.New(rand.NewSource(seed)), dir, 40)

			objects := &gitObjects{t: t, cfg: cfg, dir: dir, commits: map[string]Commit{}, trees: map[string][]Entry{}}
			stored := NewIndex(objects, newMemStore())
			uncached := NewIndex(objects, nil)

			ctx := context.Background()
			for _, start := range commits {
				for _, path := range paths {
					want := splitLines(runGit(t, cfg, dir, nil, "", "log", "--first-parent", "--format=%H", start, "--", path))

					for name, index := range map[string]*Index{"stored": stored, "uncached": uncached} {
						var got []string
						require.NoError(t, index.PathHistory(ctx, start, path, unlimited, func(c Commit) error {
							got = append(got, c.ID)
							return nil
						}), name)
						require.Equal(t, want, got, "%s: path %q from %s", name, path, start)
					}

					// A limit yields the prefix of the full history.
					var limited []string
					require.NoError(t, stored.PathHistory(ctx, start, path, 3, func(c Commit) error {
						limited = append(limited, c.ID)
						return nil
					}))
					if len(want) > 3 {
						want = want[:3]
					}
					require.Equal(t, want, limited, "limited: path %q from %s", path, start)
				}
			}
		})
	}
}
