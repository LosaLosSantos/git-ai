package rebase

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/ancestry"
	"github.com/stretchr/testify/require"
)

// countingChecker counts the questions that a plan asks, each a git process for the git checker.
type countingChecker struct {
	ancestry.Checker
	lists, reachedBottoms, reachables, mergeBases int
}

// processes is the number of git processes that the questions cost with the git checker.
func (c *countingChecker) processes() int {
	return c.lists + c.reachedBottoms + c.reachables + c.mergeBases
}

func (c *countingChecker) List(ctx context.Context, head git.ObjectID, exclude []git.ObjectID) ([]ancestry.RangeCommit, error) {
	c.lists++
	return c.Checker.List(ctx, head, exclude)
}

func (c *countingChecker) ReachedBottoms(ctx context.Context, base git.ObjectID, bottoms []git.ObjectID) (map[git.ObjectID]bool, error) {
	c.reachedBottoms++
	return c.Checker.ReachedBottoms(ctx, base, bottoms)
}

func (c *countingChecker) Reachable(ctx context.Context, base git.ObjectID, commits []git.ObjectID) (map[git.ObjectID]bool, error) {
	c.reachables++
	return c.Checker.Reachable(ctx, base, commits)
}

func (c *countingChecker) MergeBases(ctx context.Context, a, b git.ObjectID) ([]git.ObjectID, error) {
	c.mergeBases++
	return c.Checker.MergeBases(ctx, a, b)
}

// writeChain writes commits with fast-import: commit i has the parent parents[i] (-1 for a root),
// the committer date dates[i] and a file of its own, and is reachable as refs/heads/c<i>.
func writeChain(t *testing.T, f *fixture, parents []int, dates []int64) []git.ObjectID {
	t.Helper()

	var stream bytes.Buffer
	for i := range parents {
		name := fmt.Sprintf("c%d", i)
		fmt.Fprintf(&stream, "commit refs/heads/%s\nmark :%d\n", name, i+1)
		fmt.Fprintf(&stream, "committer Tester <tester@example.com> %d +0000\n", dates[i])
		fmt.Fprintf(&stream, "data %d\n%s\n", len(name), name)
		if parents[i] >= 0 {
			fmt.Fprintf(&stream, "from :%d\n", parents[i]+1)
		}
		fmt.Fprintf(&stream, "M 100644 inline %s\ndata %d\n%s\n\n", name, len(name)+1, name+"\n")
	}
	gittest.ExecOpts(t, f.cfg, gittest.ExecConfig{Stdin: &stream}, "-C", f.repoPath, "fast-import", "--quiet")

	ids := make([]git.ObjectID, len(parents))
	for i := range ids {
		ids[i] = gittest.ResolveRevision(t, f.cfg, f.repoPath, fmt.Sprintf("refs/heads/c%d", i))
	}
	return ids
}

// TestPlanOfTheReviewChain is the history that the review of phase 4b reported: a root R dated
// 2026-01, A on it dated 2026-02, and ten commits on A dated 2025-06 that make the tip. git rev-list
// tip..A lists A and R. Landing A again must find it already landed, and a commit C on A must be
// rebased alone.
func TestPlanOfTheReviewChain(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	parents := []int{-1, 0}
	dates := []int64{1767225600, 1769904000}
	for i := 0; i < 10; i++ {
		parents = append(parents, len(parents)-1)
		dates = append(dates, 1748736000+int64(i))
	}
	parents = append(parents, 1)
	dates = append(dates, 1769904100)
	ids := writeChain(t, f, parents, dates)
	root, a, tip, c := ids[0], ids[1], ids[11], ids[12]

	listed := strings.Fields(string(gittest.Exec(t, f.cfg, "-C", f.repoPath, "rev-list", tip.String()+".."+a.String())))
	require.ElementsMatch(t, []string{a.String(), root.String()}, listed, "git rev-list no longer shows the skew this test is about")

	plan, err := Plan(f.ctx, ancestry.Git(f.repo), tip, a)
	require.NoError(t, err)
	require.Equal(t, LandAlreadyLanded, plan.Kind())

	plan, err = Plan(f.ctx, ancestry.Git(f.repo), tip, c)
	require.NoError(t, err)
	require.Equal(t, LandDiverged, plan.Kind())
	todo, err := plan.Replay()
	require.NoError(t, err)
	require.Equal(t, []git.ObjectID{c}, todo)
}

// TestPlanOnALongSkewedHistory measures what a skewed listing costs on a long history: a main line
// of 2000 commits, the commit A of a change on it, and a tip of ten commits on A dated a year
// before the main line. git rev-list tip..A then lists A and the whole main line. The plan asks a
// fixed number of questions, whatever the length of the history.
func TestPlanOnALongSkewedHistory(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	const mainLine = 2000
	parents := make([]int, 0, mainLine+13)
	dates := make([]int64, 0, mainLine+13)
	for i := 0; i < mainLine; i++ {
		parents = append(parents, i-1)
		dates = append(dates, 1767225600+int64(i)*60)
	}
	a := len(parents)
	parents = append(parents, a-1)
	dates = append(dates, 1767225600+int64(mainLine)*60)
	for i := 0; i < 10; i++ {
		parents = append(parents, len(parents)-1)
		dates = append(dates, 1735689600+int64(i))
	}
	tipIndex := len(parents) - 1
	parents = append(parents, a)
	dates = append(dates, 1767225600+int64(mainLine+1)*60)
	ids := writeChain(t, f, parents, dates)
	tip, change, onA := ids[tipIndex], ids[a], ids[len(ids)-1]

	listed := strings.Fields(string(gittest.Exec(t, f.cfg, "-C", f.repoPath, "rev-list", tip.String()+".."+change.String())))
	require.Greater(t, len(listed), mainLine, "git rev-list must list the main line for this test to measure anything")

	for _, tc := range []struct {
		desc          string
		commit        git.ObjectID
		wantKind      LandKind
		wantProcesses int
	}{
		// The listing, the bottoms, the merge bases (only commit: already landed).
		{desc: "already landed", commit: change, wantKind: LandAlreadyLanded, wantProcesses: 3},
		// The same and the listing below the merge base, which is the chain of the change.
		{desc: "diverged", commit: onA, wantKind: LandDiverged, wantProcesses: 4},
	} {
		checker := &countingChecker{Checker: ancestry.Git(f.repo)}
		start := time.Now()
		plan, err := Plan(f.ctx, checker, tip, tc.commit)
		elapsed := time.Since(start)
		require.NoError(t, err)
		require.Equal(t, tc.wantKind, plan.Kind(), tc.desc)
		require.Equal(t, tc.wantProcesses, checker.processes(), tc.desc)
		require.Zero(t, checker.reachables, "%s: a land never walks the history of the tip", tc.desc)
		t.Logf("%s: %d listed by git rev-list, %d processes, %v", tc.desc, len(listed), checker.processes(), elapsed)
	}
}

// TestPlanOfAMergeOfTheTipIntoTheChange is the shape that the review of the skew fix reported: a
// main line of 3000 commits, ten commits on it dated a year before the main line, then M and the
// tip T. A change F forks from main line commit 2500, and H merges M into it. The merge base of T
// and H is M, but the path H -> F -> m2500 reaches the shared history without running through M,
// so the listing below M still holds m0..m2500. Removing them one check at a time took 2503
// checks and 43 s. The plan asks a fixed number of questions instead: the range has a merge
// commit, which a rebase refuses.
func TestPlanOfAMergeOfTheTipIntoTheChange(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	const n, k = 3000, 2500
	base := int64(1767225600)
	var parents []int
	var dates []int64
	for i := 0; i < n; i++ {
		parents = append(parents, i-1)
		dates = append(dates, base+int64(i)*60)
	}
	for j := 0; j < 10; j++ {
		parents = append(parents, len(parents)-1)
		dates = append(dates, 1735689600+int64(j))
	}
	now := base + int64(n)*60 + 1000
	m := len(parents)
	parents = append(parents, m-1)
	dates = append(dates, now)
	tipIndex := len(parents)
	parents = append(parents, m)
	dates = append(dates, now+60)
	fIndex := len(parents)
	parents = append(parents, k)
	dates = append(dates, now+120)
	ids := writeChain(t, f, parents, dates)

	// H merges M into F.
	h := git.ObjectID(strings.TrimSpace(string(gittest.ExecOpts(t, f.cfg, gittest.ExecConfig{
		Env: []string{"GIT_COMMITTER_DATE=" + fmt.Sprintf("%d +0000", now+180), "GIT_AUTHOR_DATE=" + fmt.Sprintf("%d +0000", now+180)},
	}, "-C", f.repoPath, "commit-tree", ids[fIndex].String()+"^{tree}", "-p", ids[fIndex].String(), "-p", ids[m].String(), "-m", "merge main"))))
	tip := ids[tipIndex]

	relisted := strings.Fields(string(gittest.Exec(t, f.cfg, "-C", f.repoPath, "rev-list", h.String(), "^"+ids[m].String())))
	require.Greater(t, len(relisted), k, "the listing below the merge base must hold the main line for this test to measure anything")

	checker := &countingChecker{Checker: ancestry.Git(f.repo)}
	start := time.Now()
	plan, err := Plan(f.ctx, checker, tip, h)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, LandDiverged, plan.Kind())
	_, err = plan.Replay()
	require.ErrorIs(t, err, ErrMergesInRange)

	require.LessOrEqual(t, checker.processes(), 4)
	require.Zero(t, checker.reachables)
	require.Less(t, elapsed, 10*time.Second)
	t.Logf("%d listed below the merge base by git rev-list; %d processes, %v", len(relisted), checker.processes(), elapsed)
}
