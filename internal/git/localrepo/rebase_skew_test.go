package localrepo

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/revrange"
	"github.com/feder-cr/git-ai/internal/log"
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

// TestRebaseTodoKeepsAReappliedCommitUnderSkewedDates rebases a branch whose commit X adds a file
// again, after the shared history added it (m10) and deleted it (m20), onto an upstream that merged
// the shared history in through a path git rev-list walks before the merge base marks it. The main
// line m0..m59 is followed by ten commits dated a year before it, then M; the branch is X and T on
// M; upstream is H, which merges M into F, a fork of m50. git rev-list --left-right H...T then
// lists m0..m50 on the side of upstream, although the branch has them, and with --cherry-pick it
// drops X as equivalent to m10. X is not equivalent to any commit that only upstream has, so the
// todo list keeps it; without the exact boundary of the side of upstream it would not.
func TestRebaseTodoKeepsAReappliedCommitUnderSkewedDates(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := NewTestRepo(t, cfg, repoProto)

	type spec struct {
		name    string
		parents []int
		date    int64
		ops     []string
	}
	base := int64(1767225600)
	var specs []spec
	add := func(s spec) int {
		specs = append(specs, s)
		return len(specs) - 1
	}

	prev := -1
	for i := 0; i < 60; i++ {
		ops := []string{fmt.Sprintf("M 100644 inline m%d\ndata 3\nm%d\n", i, i%10)}
		switch i {
		case 10:
			ops = []string{"M 100644 inline f\ndata 3\nre\n"}
		case 20:
			ops = []string{"D f\n"}
		}
		var parents []int
		if prev >= 0 {
			parents = []int{prev}
		}
		prev = add(spec{name: fmt.Sprintf("m%d", i), parents: parents, date: base + int64(i)*60, ops: ops})
	}
	m50 := 50
	for j := 0; j < 10; j++ {
		prev = add(spec{name: fmt.Sprintf("old%d", j), parents: []int{prev}, date: 1735689600 + int64(j),
			ops: []string{fmt.Sprintf("M 100644 inline old%d\ndata 2\no\n", j)}})
	}
	now := base + 60*60 + 1000
	m := add(spec{name: "M", parents: []int{prev}, date: now, ops: []string{"M 100644 inline M\ndata 2\nM\n"}})
	x := add(spec{name: "X", parents: []int{m}, date: now + 30, ops: []string{"M 100644 inline f\ndata 3\nre\n"}})
	tip := add(spec{name: "T", parents: []int{x}, date: now + 60, ops: []string{"M 100644 inline T\ndata 2\nT\n"}})
	f := add(spec{name: "F", parents: []int{m50}, date: now + 120, ops: []string{"M 100644 inline F\ndata 2\nF\n"}})
	h := add(spec{name: "H", parents: []int{f, m}, date: now + 180})

	var stream bytes.Buffer
	for i, s := range specs {
		fmt.Fprintf(&stream, "commit refs/heads/%s\nmark :%d\n", s.name, i+1)
		fmt.Fprintf(&stream, "committer Tester <tester@example.com> %d +0000\n", s.date)
		fmt.Fprintf(&stream, "data %d\n%s\n", len(s.name), s.name)
		for j, parent := range s.parents {
			if j == 0 {
				fmt.Fprintf(&stream, "from :%d\n", parent+1)
			} else {
				fmt.Fprintf(&stream, "merge :%d\n", parent+1)
			}
		}
		for _, op := range s.ops {
			stream.WriteString(op)
		}
		stream.WriteString("\n")
	}
	gittest.ExecOpts(t, cfg, gittest.ExecConfig{Stdin: &stream}, "-C", repoPath, "fast-import", "--quiet")

	resolve := func(i int) git.ObjectID {
		return gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/"+specs[i].name)
	}
	upstream, branch, xID, tipID := resolve(h), resolve(tip), resolve(x), resolve(tip)

	// The skew this test is about: git lists m10 on the side of upstream and drops X.
	listed := strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-list", "--left-only",
		upstream.String()+"..."+branch.String())))
	require.Contains(t, listed, resolve(10).String(), "git rev-list no longer shows the skew this test is about")
	dropped := strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-list", "--cherry-pick", "--right-only",
		"--no-merges", "--topo-order", "--reverse", upstream.String()+"..."+branch.String())))
	require.Equal(t, []string{tipID.String()}, dropped, "git rev-list --cherry-pick no longer drops X")

	for _, reapply := range []bool{false, true} {
		todo, err := repo.rebaseTodo(ctx, upstream, branch, reapply)
		require.NoError(t, err)
		require.Equal(t, []string{xID.String(), tipID.String()}, todo, "reapply %v", reapply)
	}
}

// TestRebaseTodoCostsAFixedNumberOfProcesses lists the todo of a branch and an upstream that both
// merge a dozen commits of the shared history, so that each side joins it at a dozen bottoms. With
// growing dates the listings are exact, and the todo list costs the listing of both sides, one
// question for the bottoms of each side that matters, and the final listing: four processes with
// --cherry-pick, three without, however many bottoms the sides have.
func TestRebaseTodoCostsAFixedNumberOfProcesses(t *testing.T) {
	t.Parallel()

	ctx := log.InitContextCustomFields(testhelper.Context(t))
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := NewTestRepo(t, cfg, repoProto)

	var stream bytes.Buffer
	mark := 0
	commit := func(name string, date int64, parents ...int) int {
		mark++
		fmt.Fprintf(&stream, "commit refs/heads/%s\nmark :%d\ncommitter Tester <tester@example.com> %d +0000\n", name, mark, date)
		fmt.Fprintf(&stream, "data %d\n%s\n", len(name), name)
		for j, parent := range parents {
			if j == 0 {
				fmt.Fprintf(&stream, "from :%d\n", parent)
			} else {
				fmt.Fprintf(&stream, "merge :%d\n", parent)
			}
		}
		fmt.Fprintf(&stream, "M 100644 inline %s\ndata %d\n%s\n\n", name, len(name)+1, name+"\n")
		return mark
	}

	date := int64(1767225600)
	var main []int
	for i := 0; i < 40; i++ {
		var parents []int
		if i > 0 {
			parents = []int{main[i-1]}
		}
		date += 60
		main = append(main, commit(fmt.Sprintf("m%d", i), date, parents...))
	}

	// Side lines fork from the shared history and are merged into both sides, a dozen times.
	upstream, branch := main[27], main[27]
	for i := 0; i < 12; i++ {
		date += 60
		side := commit(fmt.Sprintf("s%d", i), date, main[2*i])
		date += 60
		upstream = commit(fmt.Sprintf("u%d", i), date, upstream, side)
		date += 60
		branch = commit(fmt.Sprintf("b%d", i), date, branch, main[2*i+1])
	}
	date += 60
	commit("upstream", date, upstream)
	date += 60
	commit("branch", date, branch)
	gittest.ExecOpts(t, cfg, gittest.ExecConfig{Stdin: &stream}, "-C", repoPath, "fast-import", "--quiet")

	upstreamID := gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/upstream")
	branchID := gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/branch")

	leftSide, rightSide, err := revrange.Sides(ctx, repo, upstreamID, branchID)
	require.NoError(t, err)
	leftBottoms, err := revrange.Bottoms(leftSide)
	require.NoError(t, err)
	rightBottoms, err := revrange.Bottoms(rightSide)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(leftBottoms)+len(rightBottoms), 12, "the sides must join the shared history at many bottoms")

	processes := func() int {
		count, _ := log.CustomFieldsFromContext(ctx).Fields()["command.count"].(int)
		return count
	}

	for _, tc := range []struct {
		reapply       bool
		wantProcesses int
	}{
		{reapply: false, wantProcesses: 4},
		{reapply: true, wantProcesses: 3},
	} {
		before := processes()
		todo, err := repo.rebaseTodo(ctx, upstreamID, branchID, tc.reapply)
		require.NoError(t, err)
		require.NotEmpty(t, todo)
		require.Equal(t, tc.wantProcesses, processes()-before, "reapply %v with %d and %d bottoms", tc.reapply, len(leftBottoms), len(rightBottoms))
	}
}
