package review

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	housekeepingcfg "github.com/feder-cr/git-ai/internal/git/housekeeping/config"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/reviewroots"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// sweptStore is a store that requires the review roots, whose first swept
// writes fail with ErrReviewRootsMissing, as when a sweep deletes the
// keep-around references between their write and the review write.
type sweptStore struct {
	store.ReviewStore
	swept, writes int
}

func (f *sweptStore) WithRequiredRoots() store.ReviewStore { return &sweptWriter{f: f} }

type sweptWriter struct {
	store.ReviewStore
	f *sweptStore
}

func (w *sweptWriter) Create(r *gitalypb.Review) (*gitalypb.Review, error) {
	w.f.writes++
	if w.f.writes <= w.f.swept {
		return nil, fmt.Errorf("insert review: %w", corestore.ErrReviewRootsMissing)
	}
	return r, nil
}

func TestWriteRootsRetriesAfterASweep(t *testing.T) {
	ctx := testhelper.Context(t)
	s := newServer(&service.Dependencies{})

	for _, tc := range []struct {
		swept  int
		writes int
		code   codes.Code
	}{
		{swept: 0, writes: 1, code: codes.OK},
		{swept: 1, writes: 2, code: codes.OK},
		{swept: 2, writes: 3, code: codes.OK},
		{swept: 3, writes: 3, code: codes.Aborted},
	} {
		t.Run(fmt.Sprintf("swept %d", tc.swept), func(t *testing.T) {
			st := &sweptStore{swept: tc.swept}
			err := s.writeRoots(ctx, nil, st, func(st store.ReviewStore, _ rootsProtect) error {
				_, err := st.Create(&gitalypb.Review{})
				return err
			})
			require.Equal(t, tc.code, status.Code(err), "%v", err)
			require.Equal(t, tc.writes, st.writes, "every attempt writes again")
		})
	}

	// The files store does not require the roots in a database.
	files := store.Open(t.TempDir())
	require.NoError(t, s.writeRoots(ctx, nil, files, func(st store.ReviewStore, _ rootsProtect) error {
		require.Same(t, files, st)
		return nil
	}))
}

// housekeep runs the object housekeeping of Gitaly on the repository with an
// expiry of now: a full repack that drops unreachable objects into an expired
// cruft pack, then a prune of the unreachable loose objects. Production
// housekeeping does the same with a grace period of two weeks.
func housekeep(t *testing.T, cfg config.Cfg, repo *gitalypb.Repository) {
	t.Helper()

	ctx := testhelper.Context(t)
	local := localrepo.NewTestRepo(t, cfg, repo)
	now := time.Now()
	require.NoError(t, housekeeping.RepackObjects(ctx, local, housekeepingcfg.RepackObjectsConfig{
		Strategy:          housekeepingcfg.RepackObjectsStrategyFullWithCruft,
		CruftExpireBefore: now,
	}))
	require.NoError(t, housekeeping.PruneObjects(ctx, local, housekeeping.PruneObjectsConfig{ExpireBefore: now}))
}

func hasObject(t *testing.T, cfg config.Cfg, repoPath string, oid git.ObjectID) bool {
	t.Helper()
	return gittest.NewCommand(t, cfg, "-C", repoPath, "cat-file", "-e", oid.String()).Run() == nil
}

// keepArounds returns the keep-around references of the repository in the
// database.
func keepArounds(t *testing.T, ctx context.Context, core *corestore.Core, repoID int64) map[string]corestore.Target {
	t.Helper()

	refs, _, err := core.Meta.Refs(ctx, repoID)
	require.NoError(t, err)
	kept := map[string]corestore.Target{}
	for name, target := range refs {
		if strings.HasPrefix(name, corestore.KeepAroundPrefix) {
			kept[name] = target
		}
	}
	return kept
}

func keepAroundOf(commits ...git.ObjectID) map[string]corestore.Target {
	kept := map[string]corestore.Target{}
	for _, c := range commits {
		kept[corestore.KeepAroundRef(c.String())] = corestore.Target(c)
	}
	return kept
}

// TestAuthoritativeReviewRootsAreReferences: in authoritative mode a review
// write records keep-around references of its commits in the storage core,
// also for a commit that only the serving node had, and a close deletes those
// that no other open review uses (doc/graft/PLAN7.md, REVIEW ROOTS RULE).
func TestAuthoritativeReviewRootsAreReferences(t *testing.T) {
	ctx, cfg, core, client := setupAuthoritative(t)

	repo, repoPath := createLocalRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("base"), gittest.WithBranch("main"))
	tracked := importRepository(t, cfg, core, repo, repoPath, coretest.ImportOptions{})

	// The first request lets the gate replace the imported copy by a cache built
	// from the storage core; commits written into that cache afterwards are only
	// on this node.
	require.Empty(t, listStoredReviews(t, ctx, client, repo, true))
	head := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("head"), gittest.WithParents(base))
	next := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("next"), gittest.WithParents(head))

	first, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{Repository: repo, TargetBranch: []byte("main"), HeadCommitId: head.String()})
	require.NoError(t, err)
	require.Equal(t, keepAroundOf(head, base), keepArounds(t, ctx, core, tracked.ID))
	second, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{Repository: repo, TargetBranch: []byte("main"), HeadCommitId: head.String()})
	require.NoError(t, err)

	// A head move references the new head; the old one is still the second
	// review's.
	_, err = client.UpdateReview(ctx, &gitalypb.UpdateReviewRequest{Repository: repo, Id: first.GetReview().GetId(), HeadCommitId: next.String()})
	require.NoError(t, err)
	require.Equal(t, keepAroundOf(head, next, base), keepArounds(t, ctx, core, tracked.ID))

	// Closing the first review releases only what no open review uses.
	_, err = client.CloseReview(ctx, &gitalypb.CloseReviewRequest{Repository: repo, Id: first.GetReview().GetId()})
	require.NoError(t, err)
	require.Equal(t, keepAroundOf(head, base), keepArounds(t, ctx, core, tracked.ID))
	_, err = client.CloseReview(ctx, &gitalypb.CloseReviewRequest{Repository: repo, Id: second.GetReview().GetId(), Landed: true})
	require.NoError(t, err)
	require.Empty(t, keepArounds(t, ctx, core, tracked.ID))
}

// TestAuthoritativeReviewWriteSurvivesASweep: a maintenance sweep that deletes
// the keep-around references between their write and the review write makes
// the write fail in the database (ErrReviewRootsMissing), and the service
// writes them again; after maxRootsAttempts such sweeps the request is
// Aborted and nothing is recorded.
func TestAuthoritativeReviewWriteSurvivesASweep(t *testing.T) {
	for _, tc := range []struct {
		desc   string
		sweeps int
		code   codes.Code
	}{
		{desc: "one sweep", sweeps: 1, code: codes.OK},
		{desc: "every attempt swept", sweeps: maxRootsAttempts, code: codes.Aborted},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var (
				mu       sync.Mutex
				protects int
				deleted  int
				sweepErr error
			)
			ctx, cfg, core, client := setupWithStorageCore(t, corestore.ModeAuthoritative, func(s *server) {
				s.rootsProtected = func(ctx context.Context, repo *localrepo.Repo, st store.ReviewStore) {
					mu.Lock()
					defer mu.Unlock()
					protects++
					if protects > tc.sweeps {
						return
					}
					report, err := reviewroots.Sweep(ctx, repo, st, nil)
					deleted += report.Deleted
					if err != nil && sweepErr == nil {
						sweepErr = err
					}
				}
			})

			repo, repoPath := createLocalRepository(t, ctx, cfg)
			base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("base"), gittest.WithBranch("main"))
			head := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("head"), gittest.WithParents(base), gittest.WithBranch("feature"))
			tracked := importRepository(t, cfg, core, repo, repoPath, coretest.ImportOptions{})

			_, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{Repository: repo, TargetBranch: []byte("main"), HeadCommitId: head.String()})
			require.Equal(t, tc.code, status.Code(err), "%v", err)

			mu.Lock()
			defer mu.Unlock()
			require.NoError(t, sweepErr)
			require.Equal(t, 2*tc.sweeps, deleted, "every sweep deleted the head's and the base's reference")

			reviews, err := core.Meta.ListReviews(ctx, tracked.ID, "", true, 0)
			require.NoError(t, err)
			if tc.code == codes.OK {
				require.Equal(t, tc.sweeps+1, protects)
				require.Len(t, reviews, 1)
				require.Equal(t, keepAroundOf(head, base), keepArounds(t, ctx, core, tracked.ID))
			} else {
				require.Equal(t, maxRootsAttempts, protects)
				require.Empty(t, reviews, "nothing is recorded")
			}
		})
	}
}

// TestReviewRootsOutsideAuthoritativeMode: with the storage core off or in
// shadow mode the reviews are files, and the service still writes the
// keep-around references: housekeeping keeps an open review's commit after its
// branch is deleted. In shadow mode the voter records the references in the
// database, which does not require them (the files are the source of truth).
func TestReviewRootsOutsideAuthoritativeMode(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		h := setupHarness(t)
		base, head := h.fixture(codeV2)
		gittest.WriteRef(t, h.cfg, h.repoPath, "refs/heads/feature", head)
		h.create(head)
		gittest.Exec(t, h.cfg, "-C", h.repoPath, "update-ref", "-d", "refs/heads/feature")

		housekeep(t, h.cfg, h.repo)
		require.True(t, hasObject(t, h.cfg, h.repoPath, head))
		require.True(t, hasObject(t, h.cfg, h.repoPath, base))
	})

	t.Run("shadow", func(t *testing.T) {
		ctx, cfg, core, client := setupWithStorageCore(t, corestore.ModeShadow)
		repo, repoPath := createLocalRepository(t, ctx, cfg)
		base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("base"), gittest.WithBranch("main"))
		head := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("head"), gittest.WithParents(base), gittest.WithBranch("feature"))
		tracked := importRepository(t, cfg, core, repo, repoPath, coretest.ImportOptions{})

		_, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{Repository: repo, TargetBranch: []byte("main"), HeadCommitId: head.String()})
		require.NoError(t, err)
		require.Equal(t, keepAroundOf(head, base), keepArounds(t, ctx, core, tracked.ID), "the voter records the references")
		gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "-d", "refs/heads/feature")

		housekeep(t, cfg, repo)
		require.True(t, hasObject(t, cfg, repoPath, head))
	})
}
