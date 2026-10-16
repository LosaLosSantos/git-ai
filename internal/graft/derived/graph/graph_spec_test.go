// Black-box spec tests for WP25 (graph kind), written from PLAN3.md only.
//
// ADAPTATION POINTS (adapted at the WP25 gate to the binding API of the
// RECONCILIATION section, which keeps the draft Index):
//   - newSpecStore: derived.NewStore over a temp dir.
//   - specIndex: NewIndex(NewCatfileSource(reader), store.Kind("graph", 1)).
//   - Module path github.com/feder-cr/git-ai.
//   - MergeBase(ctx, a, b) (git.ObjectID, error); the empty ID means no base.
package graph

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// specRepo is a repository plus the recorded DAG used as ground truth.
type specRepo struct {
	cfg     config.Cfg
	repo    *gitalypb.Repository
	path    string
	ids     []git.ObjectID // commits in creation order (parents always precede children)
	gen     []uint32       // expected generation, computed from the recorded parents
	indexOf map[git.ObjectID]int
}

// newSpecRepo creates a repository and n random commits (merges included).
// If roots is true, a few commits are unrelated roots. n == 0 yields an empty repo.
func newSpecRepo(t *testing.T, n int, seed int64, roots bool) *specRepo {
	t.Helper()
	cfg := testcfg.Build(t)
	repo, path := gittest.CreateRepository(t, testhelper.Context(t), cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	sr := &specRepo{cfg: cfg, repo: repo, path: path, indexOf: map[git.ObjectID]int{}}

	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < n; i++ {
		var parents []git.ObjectID
		if i > 0 && !(roots && rng.Float64() < 0.03) {
			parents = append(parents, sr.ids[rng.Intn(i)])
			if rng.Float64() < 0.3 {
				if q := sr.ids[rng.Intn(i)]; q != parents[0] {
					parents = append(parents, q) // merge commit
				}
			}
		}
		id := sr.commit(t, fmt.Sprintf("c%d", i), parents...)
		sr.gen = append(sr.gen, sr.expectedGen(parents))
		sr.indexOf[id] = i
		sr.ids = append(sr.ids, id)
	}
	return sr
}

func (sr *specRepo) commit(t *testing.T, msg string, parents ...git.ObjectID) git.ObjectID {
	t.Helper()
	return gittest.WriteCommit(t, sr.cfg, sr.path,
		gittest.WithMessage(msg),
		gittest.WithParents(parents...),
	)
}

func (sr *specRepo) expectedGen(parents []git.ObjectID) uint32 {
	g := uint32(1)
	for _, p := range parents {
		if pg := sr.gen[sr.indexOf[p]]; pg+1 > g {
			g = pg + 1
		}
	}
	return g
}

// specIndex builds the graph index for the repository.
// ADAPT: source/store construction is not fixed by the spec.
func specIndex(t *testing.T, sr *specRepo) (*Index, derived.KindStore) {
	t.Helper()
	store := newSpecStore(t)
	kind := store.Kind("graph", 1)
	repo := localrepo.NewTestRepo(t, sr.cfg, sr.repo)
	catfileCache := catfile.NewCache(sr.cfg)
	t.Cleanup(catfileCache.Stop)
	reader, cancel, err := catfileCache.ObjectReader(testhelper.Context(t), repo)
	require.NoError(t, err)
	t.Cleanup(cancel)
	idx := NewIndex(NewCatfileSource(reader), kind)
	return idx, kind
}

// newSpecStore is an ADAPT point: returns a derived store backed by a temp dir.
func newSpecStore(t *testing.T) *derived.Store {
	t.Helper()
	return derived.NewStore(t.TempDir())
}

// gitRun runs git -C repoPath args and returns trimmed stdout and the exit code.
func gitRun(t *testing.T, cfg config.Cfg, repoPath string, args ...string) (string, int) {
	t.Helper()
	cmd := gittest.NewCommand(t, cfg, append([]string{"-C", repoPath}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return strings.TrimSpace(string(out)), exitErr.ExitCode()
		}
		require.NoError(t, err)
	}
	return strings.TrimSpace(string(out)), 0
}

// gitIsAncestor is the ground truth for IsAncestor.
func gitIsAncestor(t *testing.T, cfg config.Cfg, repoPath string, a, b git.ObjectID) bool {
	t.Helper()
	_, code := gitRun(t, cfg, repoPath, "merge-base", "--is-ancestor", a.String(), b.String())
	require.Contains(t, []int{0, 1}, code, "unexpected git exit code")
	return code == 0
}

// gitMergeBaseAll is the ground truth for best common ancestors (empty if none).
func gitMergeBaseAll(t *testing.T, cfg config.Cfg, repoPath string, a, b git.ObjectID) map[string]bool {
	t.Helper()
	out, code := gitRun(t, cfg, repoPath, "merge-base", "--all", a.String(), b.String())
	set := map[string]bool{}
	if code == 1 {
		return set
	}
	require.Equal(t, 0, code, "git merge-base --all failed")
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			set[line] = true
		}
	}
	return set
}

func TestSpecGenerationRootAndMerge(t *testing.T) {
	ctx := testhelper.Context(t)
	sr := newSpecRepo(t, 0, 0, false)

	// Hand-built: r; a<-r; b<-a; c<-r; m = merge(b, c).
	r := sr.commit(t, "r")
	a := sr.commit(t, "a", r)
	b := sr.commit(t, "b", a)
	c := sr.commit(t, "c", r)
	m := sr.commit(t, "m", b, c)

	idx, _ := specIndex(t, sr)
	want := map[git.ObjectID]uint32{r: 1, a: 2, b: 3, c: 2, m: 4}
	for id, w := range want {
		got, err := idx.Generation(ctx, id)
		require.NoError(t, err)
		require.Equal(t, w, got, "generation of %s: root must be 1, child 1+parent, merge 1+max(parents)", id)
	}
}

func TestSpecGenerationMatchesRecordedDAG(t *testing.T) {
	ctx := testhelper.Context(t)
	sr := newSpecRepo(t, 220, 7, true)
	idx, _ := specIndex(t, sr)

	for i, id := range sr.ids {
		got, err := idx.Generation(ctx, id)
		require.NoError(t, err)
		require.Equal(t, sr.gen[i], got, "generation mismatch for commit %d (%s)", i, id)
	}
}

func TestSpecIsAncestorReflexiveAndBasic(t *testing.T) {
	ctx := testhelper.Context(t)
	sr := newSpecRepo(t, 0, 0, false)
	r := sr.commit(t, "r")
	x := sr.commit(t, "x", r)
	y := sr.commit(t, "y", x)
	other := sr.commit(t, "other")

	idx, _ := specIndex(t, sr)

	ok, err := idx.IsAncestor(ctx, y, y)
	require.NoError(t, err)
	require.True(t, ok, "a commit must be an ancestor of itself")

	ok, err = idx.IsAncestor(ctx, r, y)
	require.NoError(t, err)
	require.True(t, ok, "root must be an ancestor of descendant")

	ok, err = idx.IsAncestor(ctx, y, r)
	require.NoError(t, err)
	require.False(t, ok, "descendant must not be an ancestor of its ancestor")

	ok, err = idx.IsAncestor(ctx, other, y)
	require.NoError(t, err)
	require.False(t, ok, "unrelated history must not be an ancestor")
}

func TestSpecIsAncestorAgainstGit(t *testing.T) {
	ctx := testhelper.Context(t)
	sr := newSpecRepo(t, 220, 11, true)
	idx, _ := specIndex(t, sr)

	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 400; i++ {
		a := sr.ids[rng.Intn(len(sr.ids))]
		b := sr.ids[rng.Intn(len(sr.ids))]
		want := gitIsAncestor(t, sr.cfg, sr.path, a, b)
		got, err := idx.IsAncestor(ctx, a, b)
		require.NoError(t, err)
		require.Equal(t, want, got, "IsAncestor(%s, %s) disagrees with git merge-base --is-ancestor", a, b)
	}
}

func TestSpecMergeBaseIsBestCommonAncestor(t *testing.T) {
	ctx := testhelper.Context(t)
	sr := newSpecRepo(t, 220, 23, true)
	idx, _ := specIndex(t, sr)

	rng := rand.New(rand.NewSource(5))
	for i := 0; i < 300; i++ {
		a := sr.ids[rng.Intn(len(sr.ids))]
		b := sr.ids[rng.Intn(len(sr.ids))]
		best := gitMergeBaseAll(t, sr.cfg, sr.path, a, b)

		base, err := idx.MergeBase(ctx, a, b)
		require.NoError(t, err)
		ok := base != ""
		if len(best) == 0 {
			require.False(t, ok, "MergeBase(%s, %s) must report no base for disjoint histories", a, b)
			continue
		}
		require.True(t, ok, "MergeBase(%s, %s) must find a base", a, b)
		// On criss-cross histories any best base is acceptable; it must be one
		// of the bases git reports as best (non-redundant common ancestors).
		require.True(t, best[base.String()],
			"MergeBase(%s, %s) = %s is not among git's best common ancestors %v", a, b, base, best)
	}
}

func TestSpecMergeBaseDisjointHistories(t *testing.T) {
	ctx := testhelper.Context(t)
	sr := newSpecRepo(t, 0, 0, false)
	x := sr.commit(t, "x")
	xc := sr.commit(t, "xc", x)
	y := sr.commit(t, "y")

	idx, _ := specIndex(t, sr)
	base, err := idx.MergeBase(ctx, xc, y)
	require.NoError(t, err)
	ok := base != ""
	require.False(t, ok, "two unrelated roots have no merge base, got %s", base)
}

func TestSpecDeepLinearHistoryNoRecursion(t *testing.T) {
	if testing.Short() {
		t.Skip("5,000-commit history is slow to build; skipped in -short mode")
	}
	ctx := testhelper.Context(t)
	const depth = 5000
	sr := newLinearSpecRepo(t, depth)
	idx, _ := specIndex(t, sr)

	tip := sr.ids[depth-1]
	root := sr.ids[0]

	g, err := idx.Generation(ctx, tip)
	require.NoError(t, err)
	require.Equal(t, uint32(depth), g, "tip of a linear history of %d commits must have generation %d", depth, depth)

	ok, err := idx.IsAncestor(ctx, root, tip)
	require.NoError(t, err)
	require.True(t, ok, "root must be an ancestor of tip on a 5,000-deep chain")

	ok, err = idx.IsAncestor(ctx, tip, root)
	require.NoError(t, err)
	require.False(t, ok)

	mid := sr.ids[depth/2]
	base, err := idx.MergeBase(ctx, mid, tip)
	require.NoError(t, err)
	found := base != ""
	require.True(t, found)
	require.Equal(t, mid, base, "merge base of an ancestor and its descendant is the ancestor")
}

// newLinearSpecRepo builds a straight chain of n commits.
func newLinearSpecRepo(t *testing.T, n int) *specRepo {
	t.Helper()
	sr := newSpecRepo(t, 0, 0, false)
	var prev git.ObjectID
	for i := 0; i < n; i++ {
		var parents []git.ObjectID
		if i > 0 {
			parents = []git.ObjectID{prev}
		}
		prev = sr.commit(t, fmt.Sprintf("l%d", i), parents...)
		sr.gen = append(sr.gen, sr.expectedGen(parents))
		sr.indexOf[prev] = i
		sr.ids = append(sr.ids, prev)
	}
	return sr
}

func TestSpecStoredValueBinaryFormat(t *testing.T) {
	ctx := testhelper.Context(t)
	sr := newSpecRepo(t, 0, 0, false)
	c0 := sr.commit(t, "c0")
	c1 := sr.commit(t, "c1", c0)
	c2 := sr.commit(t, "c2", c1)
	c3 := sr.commit(t, "c3", c2)

	idx, kind := specIndex(t, sr)
	g, err := idx.Generation(ctx, c3)
	require.NoError(t, err)
	require.Equal(t, uint32(4), g)

	oidLen := len(c0.String()) / 2
	raw := func(id git.ObjectID) []byte {
		b, err := hex.DecodeString(id.String())
		require.NoError(t, err)
		return b
	}
	wantGen := map[git.ObjectID]uint32{c0: 1, c1: 2, c2: 3, c3: 4}

	for id, wg := range wantGen {
		val, found, err := kind.Get(id)
		require.NoError(t, err)
		require.True(t, found, "computed entry for %s must be persisted under kind graph v1", id)
		require.GreaterOrEqual(t, len(val), 5, "value must hold uint32 generation and uint8 count")

		// uint32 generation, little-endian, first; then uint8 n; then n object IDs.
		require.Equal(t, wg, binary.LittleEndian.Uint32(val[0:4]), "little-endian generation of %s", id)
		n := int(val[4])
		require.Len(t, val, 5+n*oidLen, "encoded length must be 5 + n object IDs for %s", id)
	}

	// c3: skip[0] = first parent c2, skip[1] = distance-2 ancestor c1.
	val, _, err := kind.Get(c3)
	require.NoError(t, err)
	n := int(val[4])
	require.GreaterOrEqual(t, n, 2, "c3 has ancestors at distances 1 and 2, so n must be at least 2")
	require.Equal(t, raw(c2), val[5:5+oidLen], "skip[0] must be the first parent")
	require.Equal(t, raw(c1), val[5+oidLen:5+2*oidLen], "skip[1] must be the distance-2 first-parent ancestor")

	// Root has no ancestors, so n is zero.
	rootVal, _, err := kind.Get(c0)
	require.NoError(t, err)
	require.Equal(t, byte(0), rootVal[4], "root must store zero skip pointers")
}

func TestSpecGraphWarmerRegistered(t *testing.T) {
	require.True(t, slices.Contains(derived.Warmers(), "graph"),
		"the graph kind must register an eager warmer named \"graph\" from init(); got %v", derived.Warmers())
}
