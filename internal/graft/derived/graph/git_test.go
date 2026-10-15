package graph

import (
	"bytes"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// randomHistory returns the parents of n commits as indexes. Commit 0 is the only
// root, so every pair of commits has a common ancestor and git merge-base always
// prints something.
func randomHistory(rnd *rand.Rand, n int) [][]int {
	parents := make([][]int, n)
	for i := 1; i < n; i++ {
		first := i - 1
		if rnd.Intn(2) == 0 {
			first = rnd.Intn(i)
		}
		parents[i] = []int{first}

		if rnd.Intn(3) == 0 {
			if second := rnd.Intn(i); second != first {
				parents[i] = append(parents[i], second)
			}
		}
	}
	return parents
}

// writeCommits creates the commits with git fast-import. Commit i has the parents
// parents[i], which must be lower indexes, and is also reachable as refs/heads/c<i>.
func writeCommits(tb testing.TB, cfg config.Cfg, repoPath string, parents [][]int) []git.ObjectID {
	tb.Helper()

	var stream bytes.Buffer
	for i, list := range parents {
		name := fmt.Sprintf("c%d", i)
		fmt.Fprintf(&stream, "commit refs/heads/%s\n", name)
		fmt.Fprintf(&stream, "mark :%d\n", i+1)
		fmt.Fprintf(&stream, "committer Tester <tester@example.com> %d +0000\n", 1600000000+i)
		fmt.Fprintf(&stream, "data %d\n%s\n", len(name), name)
		for j, parent := range list {
			if j == 0 {
				fmt.Fprintf(&stream, "from :%d\n", parent+1)
			} else {
				fmt.Fprintf(&stream, "merge :%d\n", parent+1)
			}
		}
		stream.WriteString("\n")
	}

	gittest.ExecOpts(tb, cfg, gittest.ExecConfig{Stdin: &stream}, "-C", repoPath, "fast-import", "--quiet")

	ids := make([]git.ObjectID, 0, len(parents))
	for i := range parents {
		ids = append(ids, gittest.ResolveRevision(tb, cfg, repoPath, fmt.Sprintf("refs/heads/c%d", i)))
	}
	return ids
}

// gitAncestors returns the commits that git rev-list prints for id.
func gitAncestors(tb testing.TB, cfg config.Cfg, repoPath string, id git.ObjectID) map[git.ObjectID]bool {
	tb.Helper()

	ancestors := make(map[git.ObjectID]bool)
	for _, line := range splitLines(gittest.Exec(tb, cfg, "-C", repoPath, "rev-list", id.String())) {
		ancestors[git.ObjectID(line)] = true
	}
	return ancestors
}

// gitMergeBases returns the sorted output of git merge-base --all for a and b.
func gitMergeBases(tb testing.TB, cfg config.Cfg, repoPath string, a, b git.ObjectID) []string {
	tb.Helper()

	bases := splitLines(gittest.Exec(tb, cfg, "-C", repoPath, "merge-base", "--all", a.String(), b.String()))
	sort.Strings(bases)
	return bases
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

// createRepo creates an empty bare repository with the object format of the test
// configuration.
func createRepo(tb testing.TB, cfg config.Cfg) (*gitalypb.Repository, string) {
	tb.Helper()

	storagePath := cfg.Storages[0].Path
	repo := gittest.InitRepoDir(tb, storagePath, gittest.NewRepositoryName(tb))
	repoPath := filepath.Join(storagePath, repo.GetRelativePath())

	objectFormat := "sha1"
	if gittest.ObjectHashIsSHA256() {
		objectFormat = "sha256"
	}
	gittest.Exec(tb, cfg, "init", "--bare", "--quiet", "--object-format="+objectFormat, repoPath)

	return repo, repoPath
}

func TestIndex_MatchesGit(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	rnd := rand.New(rand.NewSource(42))
	const n = 70
	ids := writeCommits(t, cfg, repoPath, randomHistory(rnd, n))

	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)
	reader, cancel, err := catfileCache.ObjectReader(ctx, repo)
	require.NoError(t, err)
	t.Cleanup(cancel)

	ix := NewIndex(NewCatfileSource(reader), noStore())

	ancestors := make([]map[git.ObjectID]bool, n)
	for i, id := range ids {
		ancestors[i] = gitAncestors(t, cfg, repoPath, id)
	}

	for j := range ids {
		for i := range ids {
			got, err := ix.IsAncestor(ctx, ids[i], ids[j])
			require.NoError(t, err)
			require.Equal(t, ancestors[j][ids[i]], got, "IsAncestor(c%d, c%d)", i, j)
		}
	}

	for k := 0; k < 300; k++ {
		i, j := rnd.Intn(n), rnd.Intn(n)

		bases, err := ix.MergeBases(ctx, ids[i], ids[j])
		require.NoError(t, err)

		var got []string
		for _, base := range bases {
			got = append(got, base.String())
		}
		sort.Strings(got)

		require.Equal(t, gitMergeBases(t, cfg, repoPath, ids[i], ids[j]), got, "MergeBases(c%d, c%d)", i, j)
	}
}

// TestIndex_MatchesGitOnRandomDAGWithMerges compares the answers with git on a history
// of more than 200 commits with many merges.
func TestIndex_MatchesGitOnRandomDAGWithMerges(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	rnd := rand.New(rand.NewSource(9))
	const n = 220
	parents := randomHistory(rnd, n)
	merges := 0
	for _, list := range parents {
		if len(list) > 1 {
			merges++
		}
	}
	require.GreaterOrEqual(t, merges, 20)

	ids := writeCommits(t, cfg, repoPath, parents)

	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)
	reader, cancel, err := catfileCache.ObjectReader(ctx, repo)
	require.NoError(t, err)
	t.Cleanup(cancel)

	ix := NewIndex(NewCatfileSource(reader), noStore())

	ancestors := make([]map[git.ObjectID]bool, n)
	for i, id := range ids {
		ancestors[i] = gitAncestors(t, cfg, repoPath, id)
	}

	for k := 0; k < 3000; k++ {
		i, j := rnd.Intn(n), rnd.Intn(n)

		got, err := ix.IsAncestor(ctx, ids[i], ids[j])
		require.NoError(t, err)
		require.Equal(t, ancestors[j][ids[i]], got, "IsAncestor(c%d, c%d)", i, j)
	}

	for k := 0; k < 300; k++ {
		i, j := rnd.Intn(n), rnd.Intn(n)

		bases, err := ix.MergeBases(ctx, ids[i], ids[j])
		require.NoError(t, err)

		var got []string
		for _, base := range bases {
			got = append(got, base.String())
		}
		sort.Strings(got)

		require.Equal(t, gitMergeBases(t, cfg, repoPath, ids[i], ids[j]), got, "MergeBases(c%d, c%d)", i, j)
	}
}
