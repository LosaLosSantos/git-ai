package derived

import (
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func TestLastMod_ManyAbsentPathsWriteNothing(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	const length = 200
	ids := writeHistory(t, cfg, repoPath, linearHistory(length, func(i int) string { return "" }))
	head := ids[fmt.Sprintf("c%d", length-1)]

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	for i := 0; i < 20; i++ {
		got, err := deriver.LastMod(ctx, head, fmt.Sprintf("never-existed-%d", i))
		require.NoError(t, err)
		require.Empty(t, got)
	}

	// Each request walks the whole history, but none of them may write to disk.
	require.Zero(t, countStoreEntries(t, repoPath))
}

func TestLastMod_WritesPerWalkAreBounded(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	length := maxStoredPerWalk + 200
	ids := writeHistory(t, cfg, repoPath, linearHistory(length, func(i int) string {
		if i == 0 {
			return "target"
		}
		return ""
	}))
	head := ids[fmt.Sprintf("c%d", length-1)]

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	got, err := deriver.LastMod(ctx, head, "target")
	require.NoError(t, err)
	require.Equal(t, ids["c0"], got)
	require.Equal(t, int64(length), deriver.CommitsComputed())

	// The walk visits every commit of the chain, but writes only checkpoints and never
	// more than maxStoredPerWalk entries, so the writes of one request stay far below
	// one per commit.
	count := countStoreEntries(t, repoPath)
	require.Positive(t, count)
	require.LessOrEqual(t, count, maxStoredPerWalk)
	require.LessOrEqual(t, count, length/checkpointInterval+2)
}

func TestLastMod_MissingParentIsAnError(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := writeHistory(t, cfg, repoPath, linearHistory(2, func(i int) string { return "a" }))
	tree := gittest.ResolveRevision(t, cfg, repoPath, ids["c1"].String()+"^{tree}")

	// A commit whose parent does not exist. The path exists in it, but the answer
	// cannot be known without the parent, so it must not be reported as absent.
	missingParent := strings.Repeat("1", len(tree.String()))
	commit := fmt.Sprintf("tree %s\nparent %s\nauthor T <t@example.com> 1600000000 +0000\ncommitter T <t@example.com> 1600000000 +0000\n\norphan\n", tree, missingParent)
	orphan := git.ObjectID(strings.TrimSpace(string(gittest.ExecOpts(t, cfg, gittest.ExecConfig{Stdin: strings.NewReader(commit)},
		"-C", repoPath, "hash-object", "-t", "commit", "-w", "--stdin"))))

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	got, err := deriver.LastMod(ctx, orphan, "a")
	require.Error(t, err)
	require.Empty(t, got)
}
