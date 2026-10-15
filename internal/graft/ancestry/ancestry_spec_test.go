package ancestry

// Black-box spec tests for WP36 (ancestry checker). Written from PLAN4.md only.
//
// ASSUMPTIONS (not named in the spec; orchestrator must confirm or wire):
//   - localrepo.NewTestRepo(t, cfg, repoProto) builds the *localrepo.Repo.
//   - gittest.CreateRepository(t, ctx, cfg) returns (*gitalypb.Repository, repoPath).
//   - gittest.TreeEntry{Mode, Path, Content} is the tree-entry type for WithTreeEntries.
//   - testhelper.NewLogger(t) returns a log.Logger.
//   - MergeBase's bool result means "a merge base was found".
//   - objectReader and derivedStore are placeholders for the catfile reader and the
//     derived store of the test repo; they fail the test until wired.

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/graft/derived"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/testhelper"
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

// assertSameAnswers checks that got and want agree on IsAncestor and MergeBase for (x, y).
func assertSameAnswers(t *testing.T, fx *specFixture, want, got Checker, x, y git.ObjectID) {
	t.Helper()

	wAnc, wErr := want.IsAncestor(fx.ctx, x, y)
	gAnc, gErr := got.IsAncestor(fx.ctx, x, y)
	if wErr != nil || gErr != nil {
		t.Fatalf("IsAncestor(%s, %s): want err %v, got err %v", x, y, wErr, gErr)
	}
	if wAnc != gAnc {
		t.Fatalf("IsAncestor(%s, %s): want %v, got %v", x, y, wAnc, gAnc)
	}

	wBase, wOK, wErr := want.MergeBase(fx.ctx, x, y)
	gBase, gOK, gErr := got.MergeBase(fx.ctx, x, y)
	if wErr != nil || gErr != nil {
		t.Fatalf("MergeBase(%s, %s): want err %v, got err %v", x, y, wErr, gErr)
	}
	if wOK != gOK || wBase != gBase {
		t.Fatalf("MergeBase(%s, %s): want (%s, %v), got (%s, %v)", x, y, wBase, wOK, gBase, gOK)
	}
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

// TestSpecGitIsAncestorAndMergeBaseLinear: Git(repo) answers IsAncestor and MergeBase
// on a linear history as Git does.
func TestSpecGitIsAncestorAndMergeBaseLinear(t *testing.T) {
	fx := newSpecFixture(t)
	chk := Git(fx.repo)

	a := fx.commit(t, 1)
	b := fx.commit(t, 2, a)
	c := fx.commit(t, 3, b)

	cases := []struct {
		name       string
		anc, desc  git.ObjectID
		wantAncest bool
	}{
		{"root is ancestor of tip", a, c, true},
		{"parent is ancestor of child", b, c, true},
		{"tip is not ancestor of root", c, a, false},
	}
	for _, tc := range cases {
		got, err := chk.IsAncestor(fx.ctx, tc.anc, tc.desc)
		if err != nil {
			t.Fatalf("%s: IsAncestor: %v", tc.name, err)
		}
		if got != tc.wantAncest {
			t.Fatalf("%s: IsAncestor = %v, want %v", tc.name, got, tc.wantAncest)
		}
	}

	base, found, err := chk.MergeBase(fx.ctx, a, c)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	if !found || base != a {
		t.Fatalf("MergeBase(root, tip) = (%s, %v), want (%s, true)", base, found, a)
	}
}

// TestSpecGitMergeBaseForkAndUnrelated: MergeBase finds the fork point of two branches
// and reports no merge base for unrelated histories, without an error.
func TestSpecGitMergeBaseForkAndUnrelated(t *testing.T) {
	fx := newSpecFixture(t)
	chk := Git(fx.repo)
	root, left, right, _, lone := buildForkDAG(t, fx)

	base, found, err := chk.MergeBase(fx.ctx, left, right)
	if err != nil {
		t.Fatalf("MergeBase(left, right): %v", err)
	}
	if !found || base != root {
		t.Fatalf("MergeBase(left, right) = (%s, %v), want (%s, true)", base, found, root)
	}

	_, found, err = chk.MergeBase(fx.ctx, left, lone)
	if err != nil {
		t.Fatalf("MergeBase(left, lone): unexpected error %v", err)
	}
	if found {
		t.Fatalf("MergeBase(left, lone) found a base for unrelated histories")
	}

	anc, err := chk.IsAncestor(fx.ctx, root, lone)
	if err != nil {
		t.Fatalf("IsAncestor(root, lone): %v", err)
	}
	if anc {
		t.Fatalf("IsAncestor(root, lone) = true for unrelated histories")
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

// TestSpecMergeBaseOfAncestorIsAncestor: when one commit is an ancestor of the other,
// MergeBase returns the ancestor (checked through the Graph checker too).
func TestSpecMergeBaseOfAncestorIsAncestor(t *testing.T) {
	fx := newSpecFixture(t)
	root, left, _, merged, _ := buildForkDAG(t, fx)

	graph := fx.mustGraph(t)
	for _, chk := range []Checker{Git(fx.repo), graph} {
		base, found, err := chk.MergeBase(fx.ctx, root, merged)
		if err != nil {
			t.Fatalf("MergeBase(root, merged): %v", err)
		}
		if !found || base != root {
			t.Fatalf("MergeBase(root, merged) = (%s, %v), want (%s, true)", base, found, root)
		}

		base, found, err = chk.MergeBase(fx.ctx, left, merged)
		if err != nil {
			t.Fatalf("MergeBase(left, merged): %v", err)
		}
		if !found || base != left {
			t.Fatalf("MergeBase(left, merged) = (%s, %v), want (%s, true)", base, found, left)
		}
	}
}
