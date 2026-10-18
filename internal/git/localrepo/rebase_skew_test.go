package localrepo

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// TestRebaseTodoUnderSkewedDates checks the todo list of Rebase on a random history in which runs
// of commits are dated a year before the first commit. git rev-list upstream...branch lists commits
// of the shared history for some pairs of such a history; the todo list must still hold exactly
// the commits of the branch that upstream does not reach, without merge commits, parents first.
// Every commit changes a file of its own, so no commit is patch-equivalent to another and
// --cherry-pick drops nothing.
func TestRebaseTodoUnderSkewedDates(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := NewTestRepo(t, cfg, repoProto)

	rnd := rand.New(rand.NewSource(7))
	const n = 400
	parents := make([][]int, n)
	dates := make([]int64, n)
	for i := 0; i < n; i++ {
		dates[i] = 1700000000 + int64(i)*60
		if i == 0 {
			continue
		}
		first := i - 1
		if rnd.Intn(4) == 0 {
			first = rnd.Intn(i)
		}
		parents[i] = []int{first}
		if rnd.Intn(6) == 0 {
			if second := rnd.Intn(i); second != first {
				parents[i] = append(parents[i], second)
			}
		}
	}
	for run := 0; run < 25; run++ {
		start := 1 + rnd.Intn(n-20)
		for k := 0; k < 6+rnd.Intn(12); k++ {
			dates[start+k] = 1600000000 + int64(k)
		}
	}

	var stream bytes.Buffer
	for i, list := range parents {
		name := fmt.Sprintf("c%d", i)
		fmt.Fprintf(&stream, "commit refs/heads/%s\nmark :%d\n", name, i+1)
		fmt.Fprintf(&stream, "committer Tester <tester@example.com> %d +0000\n", dates[i])
		fmt.Fprintf(&stream, "data %d\n%s\n", len(name), name)
		for j, parent := range list {
			if j == 0 {
				fmt.Fprintf(&stream, "from :%d\n", parent+1)
			} else {
				fmt.Fprintf(&stream, "merge :%d\n", parent+1)
			}
		}
		fmt.Fprintf(&stream, "M 100644 inline %s\ndata %d\n%s\n\n", name, len(name)+1, name+"\n")
	}
	gittest.ExecOpts(t, cfg, gittest.ExecConfig{Stdin: &stream}, "-C", repoPath, "fast-import", "--quiet")

	ids := make([]git.ObjectID, n)
	index := map[string]int{}
	for i := range ids {
		ids[i] = gittest.ResolveRevision(t, cfg, repoPath, fmt.Sprintf("refs/heads/c%d", i))
		index[ids[i].String()] = i
	}

	ancestors := func(x int) map[int]bool {
		seen := map[int]bool{x: true}
		stack := []int{x}
		for len(stack) > 0 {
			c := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, p := range parents[c] {
				if !seen[p] {
					seen[p] = true
					stack = append(stack, p)
				}
			}
		}
		return seen
	}

	wrongBefore := 0
	for k := 0; k < 300; k++ {
		upstream, branch := rnd.Intn(n), rnd.Intn(n)
		upstreamAncestors := ancestors(upstream)
		want := map[int]bool{}
		for x := range ancestors(branch) {
			if !upstreamAncestors[x] && len(parents[x]) <= 1 {
				want[x] = true
			}
		}

		// The listing that Rebase used before, to show that the history exercises the skew.
		listed := map[int]bool{}
		for _, line := range strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-list",
			"--right-only", "--no-merges", ids[upstream].String()+"..."+ids[branch].String()))) {
			listed[index[line]] = true
		}
		if !equalSets(want, listed) {
			wrongBefore++
		}

		for _, reapply := range []bool{true, false} {
			todo, err := repo.rebaseTodo(ctx, ids[upstream], ids[branch], reapply)
			require.NoError(t, err)

			got := map[int]bool{}
			position := map[int]int{}
			for i, id := range todo {
				got[index[id]] = true
				position[index[id]] = i
			}
			require.True(t, equalSets(want, got), "todo of c%d onto c%d (reapply %v): got %v, want %v", branch, upstream, reapply, got, want)

			// Parents come before their children.
			for x := range got {
				for _, p := range parents[x] {
					if got[p] {
						require.Less(t, position[p], position[x], "c%d before its parent c%d", x, p)
					}
				}
			}
		}
	}
	require.Positive(t, wrongBefore, "git rev-list got no pair of this history wrong")
	t.Logf("git rev-list upstream...branch listed a wrong todo for %d of 300 pairs", wrongBefore)
}

func equalSets(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for x := range a {
		if !b[x] {
			return false
		}
	}
	return true
}
