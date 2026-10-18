package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// staleStore is a rootsStore whose first stale writes are refused with
// ErrReviewRootsStale, as when a checkpoint is recorded between the check and
// each of those writes.
type staleStore struct {
	store.ReviewStore
	stale int
	// snapshots counts the Refs calls; snapshot n has sequence 10*n.
	snapshots int
	// rootsAt are the sequences the writes carried, in order.
	rootsAt []uint64
}

func (f *staleStore) Refs() (map[string]corestore.Target, uint64, error) {
	f.snapshots++
	return map[string]corestore.Target{}, uint64(10 * f.snapshots), nil
}

func (f *staleStore) WithRootsAt(rootsAt uint64) store.ReviewStore {
	return &staleWriter{f: f, rootsAt: rootsAt}
}

type staleWriter struct {
	store.ReviewStore
	f       *staleStore
	rootsAt uint64
}

func (w *staleWriter) Create(r *gitalypb.Review) (*gitalypb.Review, error) {
	w.f.rootsAt = append(w.f.rootsAt, w.rootsAt)
	if len(w.f.rootsAt) <= w.f.stale {
		return nil, fmt.Errorf("insert review: %w", corestore.ErrReviewRootsStale)
	}
	return r, nil
}

func TestWriteRootsTakesANewSnapshotAfterAConflict(t *testing.T) {
	ctx := testhelper.Context(t)
	s := newServer(&service.Dependencies{})

	for _, tc := range []struct {
		desc    string
		stale   int
		rootsAt []uint64
		code    codes.Code
	}{
		{desc: "no conflict", stale: 0, rootsAt: []uint64{10}, code: codes.OK},
		{desc: "exactly one conflict", stale: 1, rootsAt: []uint64{10, 20}, code: codes.OK},
		{desc: "two conflicts", stale: 2, rootsAt: []uint64{10, 20, 30}, code: codes.OK},
		{desc: "attempts exhausted", stale: 3, rootsAt: []uint64{10, 20, 30}, code: codes.Aborted},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			st := &staleStore{stale: tc.stale}
			checks := 0
			err := s.writeRoots(ctx, nil, nil, st, func(st store.ReviewStore, _ rootsCheck) error {
				checks++
				_, err := st.Create(&gitalypb.Review{})
				return err
			})
			require.Equal(t, tc.code, status.Code(err), "%v", err)
			require.Equal(t, tc.rootsAt, st.rootsAt, "every attempt writes with its own snapshot")
			require.Equal(t, len(tc.rootsAt), st.snapshots)
			require.Equal(t, len(tc.rootsAt), checks, "every attempt checks again")
		})
	}

	t.Run("other errors end the write", func(t *testing.T) {
		st := &staleStore{}
		broken := errors.New("database is down")
		err := s.writeRoots(ctx, nil, nil, st, func(store.ReviewStore, rootsCheck) error { return broken })
		require.ErrorIs(t, err, broken)
		require.Equal(t, 1, st.snapshots)
	})

	// The files store holds no storage core roots: nothing is checked.
	t.Run("files store", func(t *testing.T) {
		files := store.Open(t.TempDir())
		err := s.writeRoots(ctx, nil, nil, files, func(st store.ReviewStore, check rootsCheck) error {
			require.Same(t, files, st)
			return check(git.ObjectID("not even an object ID"))
		})
		require.NoError(t, err)
	})
}

// countingFreshener counts EnsureFresh calls.
type countingFreshener struct {
	corestore.NopFreshener
	calls int
}

func (f *countingFreshener) EnsureFresh(context.Context, storage.Repository) error {
	f.calls++
	return nil
}

func TestRootsCheckReachability(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("base"))
	head := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("head"), gittest.WithParents(base))
	unreachable := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("unreachable"), gittest.WithParents(base))
	unrelated := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("unrelated"))

	snapshot := func(fresh corestore.Freshener, refs map[string]corestore.Target) *rootsSnapshot {
		return &rootsSnapshot{ctx: ctx, fresh: fresh, repoProto: repoProto, repo: repo, refs: refs}
	}
	refs := map[string]corestore.Target{
		"HEAD":            corestore.SymbolicTarget("refs/heads/main"),
		"refs/heads/main": corestore.Target(head),
	}

	fresh := &countingFreshener{}
	s := snapshot(fresh, refs)
	require.NoError(t, s.check(head, base))
	for _, commit := range []git.ObjectID{unreachable, unrelated} {
		err := s.check(head, commit)
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
		require.ErrorContains(t, err, commit.String())
	}
	require.Zero(t, fresh.calls, "every tip is local: no catch-up")

	// No reference reaches anything.
	err := snapshot(fresh, map[string]corestore.Target{"HEAD": corestore.SymbolicTarget("refs/heads/main")}).check(base)
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)

	// A tip the local repository does not have yet (committed on another node
	// after the gate caught it up): the repository is caught up before the
	// check. The counting Freshener does not bring the object, so the check
	// then fails as Internal: a Freshener that returns nil must have brought
	// every object of the snapshot.
	absent := git.ObjectID(strings.Repeat("a", gittest.DefaultObjectHash.EncodedLen()))
	err = snapshot(fresh, map[string]corestore.Target{"refs/heads/other": corestore.Target(absent)}).check(base)
	require.Equal(t, 1, fresh.calls)
	require.Equal(t, codes.Internal, status.Code(err), "%v", err)
}

// TestAuthoritativeReviewCommitsMustBeReachable is REVIEW ROOTS R1 end to end:
// a review cannot be created on, or moved to, a commit that the references in
// the storage core do not reach, even when the local repository has it.
func TestAuthoritativeReviewCommitsMustBeReachable(t *testing.T) {
	ctx, cfg, core, client := setupAuthoritative(t)

	repo, repoPath := createLocalRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("base"), gittest.WithBranch("main"))
	head := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("head"), gittest.WithParents(base), gittest.WithBranch("feature"))
	tracked := importRepository(t, cfg, core, repo, repoPath, coretest.ImportOptions{})

	// The first request lets the gate replace the imported copy by a cache built
	// from the storage core; a commit written into that cache afterwards is only
	// in the local repository, never recorded in the storage core.
	require.Empty(t, listStoredReviews(t, ctx, client, repo, true))
	local := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("local only"), gittest.WithParents(base))
	_, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository:   repo,
		TargetBranch: []byte("main"),
		HeadCommitId: local.String(),
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
	reviews, err := core.Meta.ListReviews(ctx, tracked.ID, "", true, 0)
	require.NoError(t, err)
	require.Empty(t, reviews)

	created, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository:   repo,
		TargetBranch: []byte("main"),
		HeadCommitId: head.String(),
	})
	require.NoError(t, err)
	id := created.GetReview().GetId()

	_, err = client.UpdateReview(ctx, &gitalypb.UpdateReviewRequest{
		Repository:   repo,
		Id:           id,
		HeadCommitId: local.String(),
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
	stored, _, err := core.Meta.GetReview(ctx, tracked.ID, id)
	require.NoError(t, err)
	require.Equal(t, head.String(), stored.GetHeadCommitId(), "the head did not move")

	// An update that keeps the head moves no commit and is not checked.
	_, err = client.UpdateReview(ctx, &gitalypb.UpdateReviewRequest{
		Repository:   repo,
		Id:           id,
		HeadCommitId: head.String(),
	})
	require.NoError(t, err)
}
