package derived

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func TestSupportsPath(t *testing.T) {
	t.Parallel()

	for path, want := range map[string]bool{
		"":          true,
		"/":         true,
		".":         true,
		"a":         true,
		"dir/x":     true,
		"a b/c.txt": true,
		"..":        false,
		"../a":      false,
		"a/":        false,
		"a/.":       false,
		"./a":       false,
		"./a/.":     false,
		"a/x/..":    false,
		"dir//x":    false,
		"dir/x/.":   false,
		"a\x00b":    false,
		"a*":        false,
		"a?":        false,
		"[ab]":      false,
		":(top)a":   false,
		"/abs":      false,
	} {
		require.Equal(t, want, SupportsPath(path), "SupportsPath(%q)", path)
	}
}

func TestSupportsRevision(t *testing.T) {
	t.Parallel()

	for revision, want := range map[string]bool{
		"main":        true,
		"HEAD~2":      true,
		"main^{/fix}": true,
		"^main":       false,
		"c1..c4":      false,
		"c1...c4":     false,
		"m^@":         false,
		"m^!":         false,
		"m^-":         false,
	} {
		require.Equal(t, want, SupportsRevision(revision), "SupportsRevision(%q)", revision)
	}
}

// TestBlameOrigin_FollowsParentsWithTheSameBlob covers a merge whose parents hold
// the same blob with different modes. The last-modified commit follows the mode,
// while the blame origin follows the blob like git blame does.
func TestBlameOrigin_FollowsParentsWithTheSameBlob(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := writeHistory(t, cfg, repoPath, []historyCommit{
		{name: "root", changes: []fileChange{{path: "f", content: "a\n"}}},
		{name: "b1", parents: []string{"root"}, changes: []fileChange{{path: "f", content: "b\n"}}},
		{name: "b2", parents: []string{"root"}, changes: []fileChange{{path: "f", content: "b\n", mode: "100755"}}},
		{name: "mm", parents: []string{"b1", "b2"}, changes: []fileChange{{path: "f", content: "b\n", mode: "100755"}}},
	})

	deriver := newTestDeriver(t, ctx, cfg, repo, true)

	lastMod, err := deriver.LastMod(ctx, ids["mm"], "f")
	require.NoError(t, err)
	require.Equal(t, ids["b2"], lastMod, "the mode changed in b1, so b2 is the last commit that changed f")

	origin, err := deriver.BlameOrigin(ctx, ids["mm"], "f")
	require.NoError(t, err)
	require.Equal(t, ids["b1"], origin, "git blame passes the blame to b1, the first parent with the same blob")

	originAtB2, err := deriver.BlameOrigin(ctx, ids["b2"], "f")
	require.NoError(t, err)
	require.Equal(t, ids["b2"], originAtB2)

	missing, err := deriver.BlameOrigin(ctx, ids["mm"], "missing")
	require.NoError(t, err)
	require.Equal(t, git.ObjectID(""), missing)
}

// TestLastMod_CanceledContextStoresNothing checks that a walk whose client has gone
// stops, and does not write values for the commits it did not finish.
func TestLastMod_CanceledContextStoresNothing(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	// Only the first commit changes "a", so the walk from the last commit visits all of them.
	ids := writeHistory(t, cfg, repoPath, linearHistory(200, func(i int) string {
		if i == 0 {
			return "a"
		}
		return ""
	}))

	canceled, cancel := context.WithCancel(ctx)
	cancel()

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	_, err := deriver.LastMod(canceled, ids["c199"], "a")
	require.Error(t, err)

	_, err = os.Stat(filepath.Join(repoPath, "graft", "derived", "lastmod"))
	require.ErrorIs(t, err, os.ErrNotExist, "no lastmod entry may be written after the walk was canceled")
}
