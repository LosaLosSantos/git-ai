package reviewroots_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/reviewroots"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

type fixture struct {
	ctx     context.Context
	t       *testing.T
	repo    *localrepo.Repo
	path    string
	reviews store.ReviewStore
	write   func(message string, parents ...git.ObjectID) git.ObjectID
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	pb, path := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	return fixture{
		ctx:     ctx,
		t:       t,
		repo:    localrepo.NewTestRepo(t, cfg, pb),
		path:    path,
		reviews: store.Open(path),
		write: func(message string, parents ...git.ObjectID) git.ObjectID {
			return gittest.WriteCommit(t, cfg, path, gittest.WithMessage(message), gittest.WithParents(parents...))
		},
	}
}

// keepArounds returns the keep-around references of the repository.
func (f fixture) keepArounds() map[string]corestore.Target {
	f.t.Helper()

	refs, err := gitops.ReadRefs(f.ctx, f.repo)
	require.NoError(f.t, err)
	kept := map[string]corestore.Target{}
	for name, target := range refs {
		if strings.HasPrefix(name, corestore.KeepAroundPrefix) {
			kept[name] = target
		}
	}
	return kept
}

func (f fixture) review(head, base string, state gitalypb.Review_State) *gitalypb.Review {
	f.t.Helper()

	r, err := f.reviews.Create(&gitalypb.Review{TargetBranch: []byte("main"), HeadCommitId: head, BaseCommitId: base})
	require.NoError(f.t, err)
	if state != gitalypb.Review_STATE_OPEN_UNSPECIFIED {
		r, err = f.reviews.Update(r.GetId(), func(r *gitalypb.Review) error {
			r.State = state
			return nil
		})
		require.NoError(f.t, err)
	}
	return r
}

func keepAroundOf(commits ...git.ObjectID) map[string]corestore.Target {
	kept := map[string]corestore.Target{}
	for _, c := range commits {
		kept[corestore.KeepAroundRef(c.String())] = corestore.Target(c)
	}
	return kept
}

func TestProtect(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	c1 := f.write("c1")
	c2 := f.write("c2", c1)

	require.NoError(t, reviewroots.Protect(f.ctx, f.repo, []git.ObjectID{c1, c2, c1}))
	require.Equal(t, keepAroundOf(c1, c2), f.keepArounds())

	// Protected commits: nothing to write.
	require.NoError(t, reviewroots.Protect(f.ctx, f.repo, []git.ObjectID{c2}))
	require.NoError(t, reviewroots.Protect(f.ctx, f.repo, nil))
	require.Equal(t, keepAroundOf(c1, c2), f.keepArounds())

	// A keep-around reference with another value is set to its commit.
	c3 := f.write("c3", c2)
	gittest.WriteRef(t, testcfg.Build(t), f.path, git.ReferenceName(corestore.KeepAroundRef(c3.String())), c1)
	require.NoError(t, reviewroots.Protect(f.ctx, f.repo, []git.ObjectID{c3}))
	require.Equal(t, keepAroundOf(c1, c2, c3), f.keepArounds())

	// A commit that is not in the repository cannot be protected.
	absent := git.ObjectID(strings.Repeat("a", gittest.DefaultObjectHash.EncodedLen()))
	require.Error(t, reviewroots.Protect(f.ctx, f.repo, []git.ObjectID{absent}))
	require.Equal(t, keepAroundOf(c1, c2, c3), f.keepArounds())
}

func TestSweep(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	c0 := f.write("c0")
	c1 := f.write("c1", c0)
	c2 := f.write("c2", c0)
	c3 := f.write("c3", c0)
	f.review(c1.String(), c0.String(), gitalypb.Review_STATE_OPEN_UNSPECIFIED)
	f.review(c2.String(), c0.String(), gitalypb.Review_STATE_CLOSED)
	require.NoError(t, reviewroots.Protect(f.ctx, f.repo, []git.ObjectID{c0, c1, c2, c3}))

	// Candidates only: the open review's head is kept, the closed review's goes.
	report, err := reviewroots.Sweep(f.ctx, f.repo, f.reviews, []git.ObjectID{c1, c2})
	require.NoError(t, err)
	require.Equal(t, reviewroots.SweepReport{Deleted: 1, Kept: 1}, report)
	require.Equal(t, keepAroundOf(c0, c1, c3), f.keepArounds())

	// Every keep-around reference: the one no review uses goes.
	report, err = reviewroots.Sweep(f.ctx, f.repo, f.reviews, nil)
	require.NoError(t, err)
	require.Equal(t, reviewroots.SweepReport{Deleted: 1, Kept: 2}, report)
	require.Equal(t, keepAroundOf(c0, c1), f.keepArounds())

	// A sweep of what is gone does nothing.
	report, err = reviewroots.Sweep(f.ctx, f.repo, f.reviews, []git.ObjectID{c2, c3})
	require.NoError(t, err)
	require.Equal(t, reviewroots.SweepReport{}, report)
}

func TestBackfill(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	c0 := f.write("c0")
	c1 := f.write("c1", c0)
	c4 := f.write("c4", c0)
	require.NoError(t, reviewroots.Protect(f.ctx, f.repo, []git.ObjectID{c0}))

	absent := strings.Repeat("b", gittest.DefaultObjectHash.EncodedLen())
	f.review(c1.String(), c0.String(), gitalypb.Review_STATE_OPEN_UNSPECIFIED)
	lost := f.review(absent, c0.String(), gitalypb.Review_STATE_OPEN_UNSPECIFIED)
	garbled := f.review("not an object ID", "", gitalypb.Review_STATE_OPEN_UNSPECIFIED)
	f.review(c4.String(), c0.String(), gitalypb.Review_STATE_LANDED)

	report, err := reviewroots.Backfill(f.ctx, f.repo, f.reviews)
	require.NoError(t, err)
	require.Equal(t, 1, report.Protected, "the open review's head; its base had its reference")
	require.Equal(t, []string{lost.GetId(), garbled.GetId()}, report.Missing)
	require.Equal(t, keepAroundOf(c0, c1), f.keepArounds(), "closed reviews are not backfilled")

	// A second backfill has nothing left to protect.
	report, err = reviewroots.Backfill(f.ctx, f.repo, f.reviews)
	require.NoError(t, err)
	require.Zero(t, report.Protected)
}
