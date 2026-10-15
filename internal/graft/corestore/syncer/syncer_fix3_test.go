package syncer_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/updateref"
	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/corestore/gitops"
	"github.com/feder-cr/open_scion/internal/graft/corestore/sqlstore"
	reviewstore "github.com/feder-cr/open_scion/internal/graft/review/store"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// A write of this node whose changeset is in the range of a catch-up can still
// be between git's prepared and committed states: git holds the reference
// lock and the local reference shows its old value. When another node set the
// reference back to that value (main c1 -> c2 here, then c2 -> c1), the local
// value equals the final value of the range. The catch-up must not claim the
// range before the write committed: it verifies the reference under git's
// lock, which waits for the write, then sees main at c2 and moves it to c1.
// Claiming the range at once left main at c2 locally, with the watermark at the
// head and the database at c1, for good.
func TestSyncerWaitsForPreparedLocalWrite(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		// The local write main c1 -> c2, prepared (git holds the lock), and its
		// changeset 2 in the database, as the voter commits it in the Prepared
		// phase.
		c2 := e.writeCommit(t, src, c1)
		require.NoError(t, gitops.IndexPack(e.ctx, e.localRepo(), packOf(t, e, src, c2)))
		u, err := updateref.New(e.ctx, e.localRepo(), updateref.WithDisabledTransactions())
		require.NoError(t, err)
		defer func() { _ = u.Close() }()
		require.NoError(t, u.Start())
		require.NoError(t, u.Update("refs/heads/main", c2, c1))
		require.NoError(t, u.Prepare())
		write := e.commitFrom(t, repo.ID, "node-a", src, []corestore.RefUpdate{
			{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c1), New: corestore.Target(c2)},
		})

		// Changeset 3: another node sets main back to c1.
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c2), New: corestore.Target(c1)},
		})

		checked := make(chan struct{})
		e.sy.SetTestHook(once("checked", func() { close(checked) }))
		synced := make(chan error, 1)
		go func() { synced <- e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)) }()

		select {
		case err := <-synced:
			wm, _, _ := gitops.ReadWatermark(e.localPath())
			require.FailNow(t, "the catch-up claimed the range without waiting for the prepared local write",
				"error %v, watermark %+v", err, wm)
		case <-checked:
		}

		// The catch-up waits for git's lock as long as the write holds it.
		time.Sleep(300 * time.Millisecond)
		select {
		case err := <-synced:
			wm, _, _ := gitops.ReadWatermark(e.localPath())
			require.FailNow(t, "the catch-up finished while the prepared local write held the lock",
				"error %v, watermark %+v", err, wm)
		default:
		}

		// The write commits locally, then its hook notes the sequence.
		require.NoError(t, u.Commit())
		e.sy.NoteCommitted(e.ctx, e.localPath(), repo.ID, write.Seq)
		require.NoError(t, <-synced)

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		require.Equal(t, corestore.Target(c1), e.localValues(t, "refs/heads/main")["refs/heads/main"])
	})
}

// The references of a range whose local value is final already are verified
// (verify, symref-verify, and verify of an absent reference with the zero
// object ID), never rewritten: HEAD goes to another branch and back, a branch
// is created and deleted, and main is moved and set back, all by another node.
func TestSyncerVerifiesFinalReferences(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		c2 := e.writeCommit(t, src, c1)
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/other", Expect: corestore.ExpectValue, New: corestore.Target(c1)},
			{Name: "refs/heads/tmp", Expect: corestore.ExpectValue, New: corestore.Target(c2)},
			{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c1), New: corestore.Target(c2)},
			{Name: "HEAD", Expect: corestore.ExpectAny, New: corestore.SymbolicTarget("refs/heads/other")},
		}, c2)
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/tmp", Expect: corestore.ExpectValue, Old: corestore.Target(c2)},
			{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c2), New: corestore.Target(c1)},
			{Name: "HEAD", Expect: corestore.ExpectAny, New: corestore.SymbolicTarget("refs/heads/main")},
		})

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		require.Equal(t, float64(1), syncCount(t, e.sy, "caught_up"))
		require.Zero(t, syncCount(t, e.sy, "error"))
		require.Equal(t, map[string]corestore.Target{
			"HEAD":            corestore.SymbolicTarget("refs/heads/main"),
			"refs/heads/main": corestore.Target(c1),
		}, e.localValues(t, "HEAD", "refs/heads/main", "refs/heads/tmp"), "refs/heads/tmp stays absent")
	})
}

// STALE CACHE RULE: EnsureFresh on the cache of a repository removed from the
// database (a local repository with a watermark) moves it into the storage's
// temporary directory and returns NotFound. A local repository with a corrupt
// watermark is no proof of a cache: it stays, as untracked.
func TestSyncerMovesStaleCacheOfRemovedRepository(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		deleted, err := e.core.Meta.DeleteRepository(e.ctx, e.key(localRelPath))
		require.NoError(t, err)
		require.True(t, deleted)

		err = e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath))
		require.Equal(t, codes.NotFound, status.Code(err), "error: %v", err)
		require.NoDirExists(t, e.localPath())
		tmp, err := e.locator.TempDir(e.storageName)
		require.NoError(t, err)
		require.DirExists(t, tmp)

		// Nothing is left to move: still NotFound.
		err = e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath))
		require.Equal(t, codes.NotFound, status.Code(err), "error: %v", err)

		// A local repository with a corrupt watermark is untracked and kept.
		const corrupt = "corrupt.git"
		_, corruptPath := gittest.CreateRepository(t, e.ctx, e.cfg, gittest.CreateRepositoryConfig{
			RelativePath:           corrupt,
			SkipCreationViaService: true,
		})
		require.NoError(t, os.MkdirAll(filepath.Dir(gitops.WatermarkPath(corruptPath)), 0o755))
		require.NoError(t, os.WriteFile(gitops.WatermarkPath(corruptPath), []byte("not json"), 0o644))
		err = e.sy.EnsureFresh(e.ctx, e.pbRepo(corrupt))
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "error: %v", err)
		require.Contains(t, err.Error(), "repository is not imported into the storage core")
		require.DirExists(t, corruptPath)
	})
}

// LOCK AND HISTORY RULE with the REFTABLE RULE: a files-backend copy imported
// in shadow mode is replaced by a reftable hydration in authoritative mode, but
// not while it holds shadow-period history (file changelog entries beyond the
// database's log sequence, or file reviews the database does not have):
// EnsureFresh fails with FailedPrecondition and leaves the copy in place. Once
// the history is imported, the copy is replaced.
func TestSyncerKeepsCopyWithShadowHistory(t *testing.T) {
	for _, tc := range []struct {
		desc string
		// shadowWrite writes the history into the local repository and returns
		// what ImportHistory needs to bring it into the database.
		shadowWrite func(t *testing.T, path string, oid string) ([]changelog.Entry, []*gitalypb.Review)
		// kept checks that the history is still in the local repository.
		kept func(t *testing.T, path string)
	}{
		{
			desc: "file changelog",
			shadowWrite: func(t *testing.T, path, oid string) ([]changelog.Entry, []*gitalypb.Review) {
				entries, err := changelog.Append(path, changelog.Meta{Operation: "shadow", Actor: "alice"}, []changelog.Update{
					{Reference: "refs/heads/shadow", OldOID: gittest.DefaultObjectHash.ZeroOID.String(), NewOID: oid},
				})
				require.NoError(t, err)
				return entries, nil
			},
			kept: func(t *testing.T, path string) {
				last, err := changelog.LastSequence(path)
				require.NoError(t, err)
				require.Equal(t, uint64(1), last)
			},
		},
		{
			desc: "file review",
			shadowWrite: func(t *testing.T, path, oid string) ([]changelog.Entry, []*gitalypb.Review) {
				review, err := reviewstore.Open(path).Create(&gitalypb.Review{
					TargetBranch: []byte("refs/heads/main"),
					HeadCommitId: oid,
					Title:        []byte("shadow review"),
				})
				require.NoError(t, err)
				return nil, []*gitalypb.Review{review}
			},
			kept: func(t *testing.T, path string) {
				reviews, err := reviewstore.Open(path).List("", true, 0)
				require.NoError(t, err)
				require.Len(t, reviews, 1)
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
				e := newEnv(t, envOptions{dialect: dialect})
				src := e.newSource(t)
				c1 := e.writeCommit(t, src)
				repo := e.register(t, e.key(localRelPath), src, c1)
				require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

				// The copy as shadow mode left it: files backend, watermark of
				// the repository, history only in the local files.
				e.gitOut(t, "refs", "migrate", "--ref-format=files")
				entries, reviews := tc.shadowWrite(t, e.localPath(), c1.String())

				err := e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath))
				require.Equal(t, codes.FailedPrecondition, status.Code(err), "error: %v", err)
				require.Contains(t, err.Error(), "the shadow-mode history of this repository is not in the storage core yet")
				require.Equal(t, "files", e.gitOut(t, "rev-parse", "--show-ref-format"), "the copy is left in place")
				tc.kept(t, e.localPath())

				_, _, err = e.core.Meta.ImportHistory(e.ctx, repo.ID, entries, reviews)
				require.NoError(t, err)

				require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
				require.Equal(t, "reftable", e.gitOut(t, "rev-parse", "--show-ref-format"))
				e.verifySynced(t, repo)
			})
		})
	}
}
