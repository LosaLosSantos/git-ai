package ancestry

// Black-box spec tests for WP36 (ancestry checker). Written from PLAN4.md only.
//
// ASSUMPTIONS (not named in the spec; orchestrator must confirm or wire):
//   - localrepo.NewTestRepo(t, cfg, repoProto) builds the *localrepo.Repo.
//   - gittest.CreateRepository(t, ctx, cfg) returns (*gitalypb.Repository, repoPath).
//   - gittest.TreeEntry{Mode, Path, Content} is the tree-entry type for WithTreeEntries.
//   - testhelper.NewLogger(t) returns a log.Logger.
//
// Phase 4b (PLAN8): the checker answers one question, Range; IsAncestor and MergeBase were removed
// with the land decision that used them. These tests check Range with the same fixtures.
//   - objectReader and derivedStore are placeholders for the catfile reader and the
//     derived store of the test repo; they fail the test until wired.

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

type specFixture struct {
	ctx    context.Context
	cfg    config.Cfg
	path   string
	repo   *localrepo.Repo
	reader catfile.ObjectContentReader
}

func newSpecFixture(t *testing.T) *specFixture {
	t.Helper()
	ctx := testhelper.Context(t)
	// Harness wiring: gittest.CreateRepository needs a running server, so the
	// repository is created as in the package's own tests.
	cfg, repoPath, repo, reader := newAncestryEnv(t)
	return &specFixture{
		ctx:    ctx,
		cfg:    cfg,
		path:   repoPath,
		repo:   repo,
		reader: reader,
	}
}

// commit writes a commit with a unique tree entry n and the given parents.
func (fx *specFixture) commit(t *testing.T, n int, parents ...git.ObjectID) git.ObjectID {
	t.Helper()
	return gittest.WriteCommit(t, fx.cfg, fx.path,
		gittest.WithParents(parents...),
		gittest.WithTreeEntries(gittest.TreeEntry{
			Mode:    "100644",
			Path:    fmt.Sprintf("file-%d", n),
			Content: fmt.Sprintf("content %d\n", n),
		}),
	)
}

// objectReader is a placeholder for the catfile reader of fx.repo (not named in the spec).
func (fx *specFixture) objectReader(t *testing.T) catfile.ObjectContentReader {
	t.Helper()
	return fx.reader
}

// derivedStore is a placeholder for the derived store of fx.repo (constructor not named in spec).
func (fx *specFixture) derivedStore(t *testing.T) *derived.Store {
	t.Helper()
	return derived.NewStore(fx.path)
}

func (fx *specFixture) mustGraph(t *testing.T) Checker {
	t.Helper()
	chk, err := Graph(fx.ctx, fx.repo, fx.objectReader(t), fx.derivedStore(t))
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	return chk
}

func testLogger(t *testing.T) log.Logger {
	t.Helper()
	return testhelper.NewLogger(t)
}

// assertSameAnswers checks that got and want list the same range x..y.
func assertSameAnswers(t *testing.T, fx *specFixture, want, got Checker, x, y git.ObjectID) {
	t.Helper()

	wRange, wErr := want.Range(fx.ctx, x, y)
	gRange, gErr := got.Range(fx.ctx, x, y)
	if wErr != nil || gErr != nil {
		t.Fatalf("Range(%s, %s): want err %v, got err %v", x, y, wErr, gErr)
	}
	if !reflect.DeepEqual(rangeSet(wRange), rangeSet(gRange)) {
		t.Fatalf("Range(%s, %s): want %v, got %v", x, y, rangeSet(wRange), rangeSet(gRange))
	}
}

// mustRange returns the range base..head of chk as a set.
func mustRange(t *testing.T, fx *specFixture, chk Checker, base, head git.ObjectID) map[git.ObjectID][]git.ObjectID {
	t.Helper()

	rng, err := chk.Range(fx.ctx, base, head)
	if err != nil {
		t.Fatalf("Range(%s, %s): %v", base, head, err)
	}
	return rangeSet(rng)
}

// buildForkDAG builds root -> {left, right} -> merged plus an unrelated lone root.
func buildForkDAG(t *testing.T, fx *specFixture) (root, left, right, merged, lone git.ObjectID) {
	t.Helper()
	root = fx.commit(t, 1)
	left = fx.commit(t, 2, root)
	right = fx.commit(t, 3, root)
	merged = fx.commit(t, 4, left, right)
	lone = fx.commit(t, 5)
	return
}

// TestSpecGitRangeLinear: on a linear history Git(repo) lists the commits above the base with
// their parents, and nothing when the head is reachable from the base.
func TestSpecGitRangeLinear(t *testing.T) {
	fx := newSpecFixture(t)
	chk := Git(fx.repo)

	a := fx.commit(t, 1)
	b := fx.commit(t, 2, a)
	c := fx.commit(t, 3, b)

	if got, want := mustRange(t, fx, chk, a, c), map[git.ObjectID][]git.ObjectID{b: {a}, c: {b}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Range(root, tip) = %v, want %v", got, want)
	}
	if got := mustRange(t, fx, chk, c, a); len(got) != 0 {
		t.Fatalf("Range(tip, root) = %v, want empty: the root is reachable from the tip", got)
	}
	if got := mustRange(t, fx, chk, c, c); len(got) != 0 {
		t.Fatalf("Range(tip, tip) = %v, want empty", got)
	}
}

// TestSpecGitRangeForkAndUnrelated: the range of a branch onto a sibling stops at their fork
// point, and an unrelated history is listed down to its root, without an error.
func TestSpecGitRangeForkAndUnrelated(t *testing.T) {
	fx := newSpecFixture(t)
	chk := Git(fx.repo)
	root, left, right, _, lone := buildForkDAG(t, fx)

	if got, want := mustRange(t, fx, chk, left, right), map[git.ObjectID][]git.ObjectID{right: {root}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Range(left, right) = %v, want %v", got, want)
	}
	if got, want := mustRange(t, fx, chk, left, lone), map[git.ObjectID][]git.ObjectID{lone: nil}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Range(left, lone) = %v, want %v", got, want)
	}
}

// TestSpecGraphMatchesGitOnForkAndUnrelated: Graph returns exactly what Git returns for
// every pair of commits in a small DAG with a merge and an unrelated root.
func TestSpecGraphMatchesGitOnForkAndUnrelated(t *testing.T) {
	fx := newSpecFixture(t)
	root, left, right, merged, lone := buildForkDAG(t, fx)
	ids := []git.ObjectID{root, left, right, merged, lone}

	gitChk := Git(fx.repo)
	graph := fx.mustGraph(t)
	for _, x := range ids {
		for _, y := range ids {
			assertSameAnswers(t, fx, gitChk, graph, x, y)
		}
	}
}

// TestSpecGraphMatchesGitOnSeededDAG: on a seeded random DAG of at least 200 commits with
// merges, criss-cross shapes and several unrelated roots, Graph agrees with Git on every
// sampled pair.
func TestSpecGraphMatchesGitOnSeededDAG(t *testing.T) {
	fx := newSpecFixture(t)
	rng := rand.New(rand.NewSource(1))

	const total = 220
	var ids []git.ObjectID
	merges := 0
	for i := 0; i < total; i++ {
		var parents []git.ObjectID
		switch {
		case i == 0 || i == 37 || i == 111:
			// extra roots: start unrelated histories
		case len(ids) >= 2 && rng.Intn(10) < 3:
			p1 := ids[rng.Intn(len(ids))]
			p2 := ids[rng.Intn(len(ids))]
			parents = []git.ObjectID{p1}
			if p2 != p1 {
				parents = append(parents, p2)
				merges++
			}
		default:
			parents = []git.ObjectID{ids[rng.Intn(len(ids))]}
		}
		ids = append(ids, fx.commit(t, i, parents...))
	}
	if len(ids) < 200 || merges < 10 {
		t.Fatalf("fixture too weak: %d commits, %d merges", len(ids), merges)
	}

	gitChk := Git(fx.repo)
	graph := fx.mustGraph(t)
	for k := 0; k < 400; k++ {
		x := ids[rng.Intn(len(ids))]
		y := ids[rng.Intn(len(ids))]
		assertSameAnswers(t, fx, gitChk, graph, x, y)
	}
}

// TestSpecNewDisabledUsesGit: New with enabled=false answers exactly as Git does.
func TestSpecNewDisabledUsesGit(t *testing.T) {
	fx := newSpecFixture(t)
	root, left, right, merged, lone := buildForkDAG(t, fx)
	ids := []git.ObjectID{root, left, right, merged, lone}

	disabled := New(fx.ctx, false, fx.repo, nil, nil, testLogger(t))
	gitChk := Git(fx.repo)
	for _, x := range ids {
		for _, y := range ids {
			assertSameAnswers(t, fx, gitChk, disabled, x, y)
		}
	}
}

// TestSpecNewEnabledFallsBackToGit: New with enabled=true but no usable graph store
// (a construction error) falls back to Git and gives Git's answers.
func TestSpecNewEnabledFallsBackToGit(t *testing.T) {
	fx := newSpecFixture(t)
	root, left, right, merged, lone := buildForkDAG(t, fx)
	ids := []git.ObjectID{root, left, right, merged, lone}

	enabled := New(fx.ctx, true, fx.repo, nil, nil, testLogger(t))
	gitChk := Git(fx.repo)
	for _, x := range ids {
		for _, y := range ids {
			assertSameAnswers(t, fx, gitChk, enabled, x, y)
		}
	}
}

// TestSpecRangeOfDescendantWithMerge: the range from an ancestor to a merge lists both sides of
// the merge with all their parents (checked through the Graph checker too).
func TestSpecRangeOfDescendantWithMerge(t *testing.T) {
	fx := newSpecFixture(t)
	root, left, right, merged, _ := buildForkDAG(t, fx)

	graph := fx.mustGraph(t)
	for _, chk := range []Checker{Git(fx.repo), graph} {
		want := map[git.ObjectID][]git.ObjectID{merged: {left, right}, left: {root}, right: {root}}
		if got := mustRange(t, fx, chk, root, merged); !reflect.DeepEqual(got, want) {
			t.Fatalf("Range(root, merged) = %v, want %v", got, want)
		}

		want = map[git.ObjectID][]git.ObjectID{merged: {left, right}, right: {root}}
		if got := mustRange(t, fx, chk, left, merged); !reflect.DeepEqual(got, want) {
			t.Fatalf("Range(left, merged) = %v, want %v", got, want)
		}
	}
}
