package storagecore

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	reviewstore "github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The tests of this file follow the IMPORT RULE of doc/graft/PLAN6.md: an
// import that fails after its registration rolls nothing back and deletes
// nothing; the registration stays with head sequence 0 and the next import
// resumes it.

// errDatabaseDown is what hookedMeta returns while the database is "down".
var errDatabaseDown = errors.New("database is down")

// hookedMeta is a MetaStore whose import commit can be intercepted once (to
// simulate what happens between the steps of an import) and which can be made
// unavailable.
type hookedMeta struct {
	corestore.MetaStore

	mu   sync.Mutex
	down bool
	// beforeImportCommit runs once, before the next CommitChangeset of an
	// import; its error is returned instead of committing.
	beforeImportCommit func(ctx context.Context, in corestore.ChangesetInput) error
}

func (m *hookedMeta) setDown(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down = down
}

func (m *hookedMeta) isDown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.down
}

func (m *hookedMeta) onImportCommit(fn func(ctx context.Context, in corestore.ChangesetInput) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.beforeImportCommit = fn
}

func (m *hookedMeta) CommitChangeset(ctx context.Context, in corestore.ChangesetInput) (corestore.CommitResult, error) {
	if in.Operation == importOperation {
		m.mu.Lock()
		hook := m.beforeImportCommit
		m.beforeImportCommit = nil
		m.mu.Unlock()
		if hook != nil {
			if err := hook(ctx, in); err != nil {
				return corestore.CommitResult{}, err
			}
		}
	}
	if m.isDown() {
		return corestore.CommitResult{}, errDatabaseDown
	}
	return m.MetaStore.CommitChangeset(ctx, in)
}

func (m *hookedMeta) GetRepository(ctx context.Context, key corestore.RepoKey) (corestore.Repository, error) {
	if m.isDown() {
		return corestore.Repository{}, errDatabaseDown
	}
	return m.MetaStore.GetRepository(ctx, key)
}

func (m *hookedMeta) DeleteRepository(context.Context, corestore.RepoKey) (bool, error) {
	// No import path deletes anything (IMPORT RULE).
	panic("DeleteRepository called by the import")
}

// setupHooked is setup with a hookedMeta in front of the database.
func setupHooked(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) (*harness, *hookedMeta) {
	t.Helper()

	var meta *hookedMeta
	h := setup(t, mode, dialect, func(core *corestore.Core) {
		meta = &hookedMeta{MetaStore: core.Meta}
		core.Meta = meta
	})
	return h, meta
}

// toggledBlobs is a BlobStore whose Put fails while failing is set.
type toggledBlobs struct {
	corestore.BlobStore
	failing *atomic.Bool
}

func (b toggledBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	if b.failing.Load() {
		return os.ErrPermission
	}
	return b.BlobStore.Put(ctx, key, r)
}

// interruptImport leaves the state of an import that stopped right after its
// registration (step 1): the repository registered with its history and
// reviews, head sequence 0, no references, and the local repository as it was.
func (h *harness) interruptImport() corestore.Repository {
	h.t.Helper()

	history, err := scanChangelog(h.repoPath)
	require.NoError(h.t, err)
	reviews, err := reviewstore.Open(h.repoPath).List("", true, 0)
	require.NoError(h.t, err)

	registered, _, err := h.core.Meta.RegisterRepository(h.ctx, corestore.KeyOf(h.repo), gittest.DefaultObjectHash.Format,
		corestore.InitialState{Changelog: history, Reviews: reviews})
	require.NoError(h.t, err)
	require.Zero(h.t, registered.HeadSeq)
	return registered
}

// requireIncomplete checks that the repository is registered under repoID as
// an incomplete import: head sequence 0 and no references.
func (h *harness) requireIncomplete(repoID int64) {
	h.t.Helper()

	current, err := h.core.Meta.GetRepository(h.ctx, corestore.KeyOf(h.repo))
	require.NoError(h.t, err)
	require.Equal(h.t, repoID, current.ID, "the registration is kept")
	require.Zero(h.t, current.HeadSeq)

	refs, head, err := h.core.Meta.Refs(h.ctx, repoID)
	require.NoError(h.t, err)
	require.Zero(h.t, head)
	require.Empty(h.t, refs)
}

// requireResumeHint checks that err tells the operator to run the import again.
func requireResumeHint(t *testing.T, err error, code codes.Code) {
	t.Helper()

	testhelper.RequireGrpcCode(t, err, code)
	require.ErrorContains(t, err, "run ImportRepository again")
}

// requireImported checks that the repository is imported under repoID as
// changeset 1 and that the local repository verifies clean.
func (h *harness) requireImported(repoID int64, localBefore map[string]corestore.Target) {
	h.t.Helper()

	state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
	require.NoError(h.t, err)
	require.True(h.t, state.GetTracked())
	require.Equal(h.t, repoID, state.GetRepositoryId())
	require.EqualValues(h.t, 1, state.GetHeadSequence())

	dbRefs, head, err := h.core.Meta.Refs(h.ctx, repoID)
	require.NoError(h.t, err)
	require.EqualValues(h.t, 1, head)
	require.Equal(h.t, localBefore, dbRefs)

	changesets, err := h.core.Meta.ListChangesets(h.ctx, repoID, 0, 1)
	require.NoError(h.t, err)
	require.Len(h.t, changesets, 1)
	require.Equal(h.t, importOperation, changesets[0].Operation)

	verify := h.verify(true)
	require.Empty(h.t, verify.GetDivergences())
	require.True(h.t, verify.GetConnectivityOk(), verify.GetConnectivityError())
	require.Equal(h.t, localBefore, h.localRefs())
}

// localSnapshot is what an EnsureFresh must not change in the local repository
// of an incomplete import.
type localSnapshot struct {
	dir       os.FileInfo
	refs      map[string]corestore.Target
	watermark corestore.Watermark
	marked    bool
}

func (h *harness) snapshot() localSnapshot {
	h.t.Helper()

	dir, err := os.Stat(h.repoPath)
	require.NoError(h.t, err)
	w, ok, err := gitops.ReadWatermark(h.repoPath)
	require.NoError(h.t, err)
	return localSnapshot{dir: dir, refs: h.localRefs(), watermark: w, marked: ok}
}

// requireUntouched checks that the local repository is the same directory at
// the same path, with the same references and watermark, as in before.
func (h *harness) requireUntouched(before localSnapshot) {
	h.t.Helper()

	after := h.snapshot()
	require.True(h.t, os.SameFile(before.dir, after.dir), "the local repository was moved away")
	require.Equal(h.t, before.refs, after.refs)
	require.Equal(h.t, before.marked, after.marked)
	require.Equal(h.t, before.watermark, after.watermark)
}

// TestImportRepositoryResumesInterruptedImport: an import that stopped after
// its registration is resumed under the same ID by the next import, which ends
// with VerifyRepository clean.
func TestImportRepositoryResumesInterruptedImport(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setup(t, mode, dialect)
		h.seed()
		localBefore := h.localRefs()
		interrupted := h.interruptImport()

		// Until the import is resumed, VerifyRepository refuses the repository.
		_, err := h.client.VerifyRepository(h.ctx, &gitalypb.VerifyRepositoryRequest{Repository: h.repo})
		requireResumeHint(t, err, codes.FailedPrecondition)

		state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		require.True(t, state.GetTracked())
		require.Zero(t, state.GetHeadSequence())

		res, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.EqualValues(t, 1, res.GetSequence())
		require.EqualValues(t, 3, res.GetReferences())
		require.Positive(t, res.GetPackBytes())
		require.EqualValues(t, 2, res.GetChangelogEntries(), "the history recorded by the interrupted import")
		require.EqualValues(t, 2, res.GetReviews(), "the reviews recorded by the interrupted import")

		w, ok, err := gitops.ReadWatermark(h.repoPath)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, corestore.Watermark{RepoID: interrupted.ID, Seq: 1}, w)

		// The history was not imported twice.
		dbLog, err := h.core.Meta.ScanChangelog(h.ctx, interrupted.ID, 0, 0)
		require.NoError(t, err)
		require.Len(t, dbLog, 2)

		h.requireImported(interrupted.ID, localBefore)

		// A completed import is not resumed again: only the history written since
		// then is imported, and there is none.
		again, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Zero(t, again.GetReferences())
		require.Zero(t, again.GetChangelogEntries())
		require.Zero(t, again.GetReviews())
		dbLog, err = h.core.Meta.ScanChangelog(h.ctx, interrupted.ID, 0, 0)
		require.NoError(t, err)
		require.Len(t, dbLog, 2)
	})
}

// TestImportRepositoryIncompleteLeavesLocalRepositoryAlone: in authoritative
// mode, EnsureFresh (through VerifyRepository) on a repository whose import is
// not complete is a FailedPrecondition and leaves the local repository, the
// only copy of what is being imported, untouched: same directory at the same
// path, same references, same watermark. This holds whatever watermark the
// local repository has, for the files backend of the test repositories too.
func TestImportRepositoryIncompleteLeavesLocalRepositoryAlone(t *testing.T) {
	for _, tc := range []struct {
		desc      string
		watermark func(repoID int64) *corestore.Watermark
	}{
		{desc: "no watermark", watermark: func(int64) *corestore.Watermark { return nil }},
		{desc: "watermark of the registration", watermark: func(id int64) *corestore.Watermark { return &corestore.Watermark{RepoID: id} }},
		{desc: "watermark of another repository", watermark: func(id int64) *corestore.Watermark { return &corestore.Watermark{RepoID: id + 1000, Seq: 5} }},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
				h := setup(t, corestore.ModeAuthoritative, dialect)
				h.seed()
				localBefore := h.localRefs()
				interrupted := h.interruptImport()
				if w := tc.watermark(interrupted.ID); w != nil {
					require.NoError(t, gitops.WriteWatermark(h.repoPath, *w))
				}
				before := h.snapshot()

				for i := 0; i < 2; i++ {
					_, err := h.client.VerifyRepository(h.ctx, &gitalypb.VerifyRepositoryRequest{Repository: h.repo, CheckConnectivity: true})
					requireResumeHint(t, err, codes.FailedPrecondition)
					require.ErrorContains(t, err, "repository import is not complete")
					h.requireUntouched(before)
				}
				h.requireIncomplete(interrupted.ID)

				res, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
				require.NoError(t, err)
				require.EqualValues(t, 3, res.GetReferences(), "the branch and the tag are imported, not only HEAD")
				h.requireImported(interrupted.ID, localBefore)
			})
		})
	}
}

// TestImportRepositoryDatabaseDownDuringCommit: when the database fails during
// the commit, nothing is undone: the registration stays incomplete, the local
// repository is unchanged, and a retry resumes the import.
func TestImportRepositoryDatabaseDownDuringCommit(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h, meta := setupHooked(t, mode, dialect)
		h.seed()
		localBefore := h.localRefs()

		var repoID int64
		meta.onImportCommit(func(_ context.Context, in corestore.ChangesetInput) error {
			repoID = in.RepoID
			meta.setDown(true)
			return errDatabaseDown
		})

		_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		requireResumeHint(t, err, codes.Unavailable)
		meta.setDown(false)

		h.requireIncomplete(repoID)
		_, ok, err := gitops.ReadWatermark(h.repoPath)
		require.NoError(t, err)
		require.False(t, ok, "no watermark before the import is complete")
		require.Equal(t, localBefore, h.localRefs())

		res, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.EqualValues(t, 1, res.GetSequence())
		h.requireImported(repoID, localBefore)
	})
}

// TestImportRepositoryFailedUploadIsResumed: a failed upload leaves the
// registration incomplete (it is not deleted) and the next import, once the
// bucket works again, resumes it under the same ID.
func TestImportRepositoryFailedUploadIsResumed(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		failing := &atomic.Bool{}
		failing.Store(true)
		h := setup(t, mode, dialect, func(core *corestore.Core) {
			core.Blobs = toggledBlobs{BlobStore: core.Blobs, failing: failing}
		})
		h.seed()
		localBefore := h.localRefs()

		_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		requireResumeHint(t, err, codes.Unavailable)

		state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		require.True(t, state.GetTracked(), "the registration is not deleted")
		repoID := state.GetRepositoryId()
		h.requireIncomplete(repoID)

		// Still failing: the retry resumes the same registration and fails again.
		_, err = h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		requireResumeHint(t, err, codes.Unavailable)
		h.requireIncomplete(repoID)

		failing.Store(false)
		res, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.EqualValues(t, 2, res.GetChangelogEntries())
		require.EqualValues(t, 2, res.GetReviews())
		h.requireImported(repoID, localBefore)
	})
}

// TestImportRepositoryRegistrationReplaced: when the registration of an import
// is removed and the key registered again (removed and created on another
// node) before the import commits, the import fails and touches neither
// the replacement nor the local repository; a retry is AlreadyExists.
func TestImportRepositoryRegistrationReplaced(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h, meta := setupHooked(t, corestore.ModeShadow, dialect)
		h.seed()
		localBefore := h.localRefs()
		key := corestore.KeyOf(h.repo)

		// The hook runs on a server goroutine: it records its outcome for the
		// test goroutine to check.
		var importID int64
		var replacement corestore.Repository
		var hookErr error
		meta.onImportCommit(func(ctx context.Context, in corestore.ChangesetInput) error {
			importID = in.RepoID
			if _, hookErr = meta.MetaStore.DeleteRepository(ctx, key); hookErr != nil {
				return hookErr
			}
			replacement, _, hookErr = meta.MetaStore.RegisterRepository(ctx, key, gittest.DefaultObjectHash.Format, corestore.InitialState{
				Changeset: &corestore.ChangesetInput{
					Updates: []corestore.RefUpdate{{
						Name:   "HEAD",
						Expect: corestore.ExpectValue,
						New:    corestore.SymbolicTarget("refs/heads/main"),
					}},
					Operation: "create_repository",
					Actor:     "test",
					NodeID:    "node-b",
				},
			})
			return hookErr
		})

		_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
		require.ErrorContains(t, err, "removed concurrently; run ImportRepository again")
		require.NoError(t, hookErr)
		require.NotEqual(t, importID, replacement.ID)

		current, err := h.core.Meta.GetRepository(h.ctx, key)
		require.NoError(t, err)
		require.Equal(t, replacement.ID, current.ID, "the replacement is not touched")
		require.EqualValues(t, 1, current.HeadSeq)
		require.Equal(t, localBefore, h.localRefs())
		_, ok, err := gitops.ReadWatermark(h.repoPath)
		require.NoError(t, err)
		require.False(t, ok)

		_, err = h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		testhelper.RequireGrpcCode(t, err, codes.AlreadyExists)
	})
}

// TestImportRepositoryCommitConflictKeepsChangesets: when a changeset is
// recorded for the registration before the import commits (another import of
// it, or any other writer), the import is AlreadyExists and deletes nothing:
// the registration and its changeset stay.
func TestImportRepositoryCommitConflictKeepsChangesets(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h, meta := setupHooked(t, corestore.ModeShadow, dialect)
		main := h.seed()
		key := corestore.KeyOf(h.repo)

		meta.onImportCommit(func(ctx context.Context, in corestore.ChangesetInput) error {
			_, err := meta.MetaStore.CommitChangeset(ctx, corestore.ChangesetInput{
				RepoID: in.RepoID,
				Updates: []corestore.RefUpdate{{
					Name:   "refs/heads/main",
					Expect: corestore.ExpectAny,
					New:    corestore.Target(main.String()),
				}},
				Operation: "push",
				Actor:     "test",
				NodeID:    "node-a",
			})
			return err
		})

		_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		testhelper.RequireGrpcCode(t, err, codes.AlreadyExists)

		current, err := h.core.Meta.GetRepository(h.ctx, key)
		require.NoError(t, err)
		require.EqualValues(t, 1, current.HeadSeq)
		changesets, err := h.core.Meta.ListChangesets(h.ctx, current.ID, 0, 1)
		require.NoError(t, err)
		require.Len(t, changesets, 1)
		require.Equal(t, "push", changesets[0].Operation, "the recorded changeset is kept")
	})
}

// TestImportRepositoryCompletedElsewhere: when another import (another node)
// commits the import of the same registration first, the import that loses is
// AlreadyExists and deletes nothing.
func TestImportRepositoryCompletedElsewhere(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		name := "created"
		if resumed {
			name = "resumed"
		}
		t.Run(name, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
				h, meta := setupHooked(t, corestore.ModeShadow, dialect)
				h.seed()
				localBefore := h.localRefs()
				if resumed {
					h.interruptImport()
				}

				var repoID int64
				meta.onImportCommit(func(ctx context.Context, in corestore.ChangesetInput) error {
					repoID = in.RepoID
					_, err := meta.MetaStore.CommitChangeset(ctx, in)
					return err
				})

				_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
				testhelper.RequireGrpcCode(t, err, codes.AlreadyExists)

				current, err := h.core.Meta.GetRepository(h.ctx, corestore.KeyOf(h.repo))
				require.NoError(t, err)
				require.Equal(t, repoID, current.ID)
				require.EqualValues(t, 1, current.HeadSeq)

				h.requireImported(repoID, localBefore)
			})
		})
	}
}

// TestImportRepositoryConcurrent: of concurrent imports of one repository,
// exactly one imports it; the others are AlreadyExists, or, when they start
// after the import completed, only import the history written since then
// (none).
func TestImportRepositoryConcurrent(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setup(t, mode, dialect)
		h.seed()
		localBefore := h.localRefs()

		const n = 8
		codesSeen := make([]codes.Code, n)
		responses := make([]*gitalypb.ImportRepositoryResponse, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				res, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
				codesSeen[i] = status.Code(err)
				responses[i] = res
			}(i)
		}
		wg.Wait()

		var imported, historyOnly, exists int
		for i, c := range codesSeen {
			switch c {
			case codes.OK:
				if responses[i].GetReferences() > 0 {
					imported++
				} else {
					require.Zero(t, responses[i].GetChangelogEntries())
					require.Zero(t, responses[i].GetReviews())
					historyOnly++
				}
			case codes.AlreadyExists:
				exists++
			}
		}
		require.Equal(t, 1, imported, "codes: %v", codesSeen)
		require.Equal(t, n-1, historyOnly+exists, "codes: %v", codesSeen)

		state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		h.requireImported(state.GetRepositoryId(), localBefore)
	})
}
