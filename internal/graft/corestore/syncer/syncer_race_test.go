package syncer_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// otherNode is the node ID of the changesets committed "by another node" in
// these tests; the syncer's own node is coretest's default, "node-a".
const otherNode = "node-b"

// commitFrom commits the updates as the next changeset of repository id, made
// by node, with the pack of the tips (if any).
func (e *env) commitFrom(t *testing.T, id int64, node string, src source, updates []corestore.RefUpdate, tips ...git.ObjectID) corestore.CommitResult {
	t.Helper()

	in := corestore.ChangesetInput{RepoID: id, Updates: updates, Operation: "test", Actor: "tester", NodeID: node}
	if len(tips) > 0 {
		pack, err := gitops.BuildPack(e.ctx, src.repo, tips, t.TempDir())
		require.NoError(t, err)
		defer func() { require.NoError(t, pack.Remove()) }()
		if !pack.Empty() {
			in.PackKey = corestore.NewPackKey(id, pack.SHA256)
			in.PackBytes = pack.Size
			rc, err := pack.Open()
			require.NoError(t, err)
			require.NoError(t, e.core.Blobs.Put(e.ctx, in.PackKey, rc))
			require.NoError(t, rc.Close())
		}
	}

	res, err := e.core.Meta.CommitChangeset(e.ctx, in)
	require.NoError(t, err)
	require.False(t, res.NoOp)
	return res
}

// localWrite imitates a write of this node: the changeset is committed to the
// database by "node-a" (no pack: the objects are local), then the local
// reference is set, then the hook's Committed phase notes the sequence.
func (e *env) localWrite(t *testing.T, id int64, update corestore.RefUpdate) corestore.CommitResult {
	t.Helper()

	res, err := e.core.Meta.CommitChangeset(e.ctx, corestore.ChangesetInput{
		RepoID: id, Updates: []corestore.RefUpdate{update}, Operation: "test", Actor: "tester", NodeID: "node-a",
	})
	require.NoError(t, err)
	require.False(t, res.NoOp)
	require.NoError(t, gitops.ApplyRefs(e.ctx, e.localRepo(), map[string]corestore.Target{update.Name: update.New}))
	e.sy.NoteCommitted(e.ctx, e.localPath(), id, res.Seq)
	return res
}

// once returns a test hook that runs fn the first time point is reached.
func once(point string, fn func()) func(string) {
	var o sync.Once
	return func(p string) {
		if p == point {
			o.Do(fn)
		}
	}
}

// A reference the range changed but whose local value is final already is not
// rewritten: a local write moving it between the check and the apply survives,
// and the watermark that its NoteCommitted moves reflects it.
func TestSyncerKeepsLocalWriteToFinalReference(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		// Changeset 2: a write of this node to main, in the local repository but
		// not noted (its NoteCommitted is late).
		c2 := e.writeCommit(t, src, c1)
		e.commitFrom(t, repo.ID, "node-a", src, []corestore.RefUpdate{
			{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c1), New: corestore.Target(c2)},
		}, c2)
		require.NoError(t, gitops.IndexPack(e.ctx, e.localRepo(), packOf(t, e, src, c2)))
		require.NoError(t, gitops.ApplyRefs(e.ctx, e.localRepo(), map[string]corestore.Target{"refs/heads/main": corestore.Target(c2)}))

		// Changeset 3: another node creates a branch.
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/other", Expect: corestore.ExpectValue, New: corestore.Target(c1)},
		})

		// Changeset 4: between the check and the apply, a local write moves main.
		c3 := gittest.WriteCommit(t, e.cfg, e.localPath(), gittest.WithParents(c2), gittest.WithMessage("local c3"))
		e.sy.SetTestHook(once("checked", func() {
			e.localWrite(t, repo.ID, corestore.RefUpdate{
				Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c2), New: corestore.Target(c3),
			})
		}))

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		refs, err := gitops.ReadRefs(e.ctx, e.localRepo())
		require.NoError(t, err)
		require.Equal(t, corestore.Target(c3), refs["refs/heads/main"])
		require.Equal(t, corestore.Target(c1), refs["refs/heads/other"])
	})
}

// A reference the catch-up moves is moved with the value the check read as the
// expected old value: a local write committed between the check and the apply
// makes the transaction fail, and the sync extends the range over it instead
// of reverting it.
func TestSyncerApplyDoesNotOverwriteConcurrentLocalWrite(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		// Changeset 2: another node moves main.
		c2 := e.writeCommit(t, src, c1)
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c1), New: corestore.Target(c2)},
		}, c2)

		// Changeset 3: a forced local write of main lands between the check
		// (main = c1, to be moved to c2) and the apply.
		c3 := gittest.WriteCommit(t, e.cfg, e.localPath(), gittest.WithParents(c1), gittest.WithMessage("local c3"))
		e.sy.SetTestHook(once("checked", func() {
			e.localWrite(t, repo.ID, corestore.RefUpdate{
				Name: "refs/heads/main", Expect: corestore.ExpectAny, New: corestore.Target(c3),
			})
		}))

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		refs, err := gitops.ReadRefs(e.ctx, e.localRepo())
		require.NoError(t, err)
		require.Equal(t, corestore.Target(c3), refs["refs/heads/main"])
	})
}

// A local value accepted by the check can be the effect of a later changeset
// of this node that equals a value of the range (here main goes c1 -> c2 -> c1
// on another node, and this node then moves it to c2 again): the range is
// extended through that changeset before main is moved, so the write is not
// reverted.
func TestSyncerExtendsRangeOverNewerLocalWrite(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		c2 := e.writeCommit(t, src, c1)
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c1), New: corestore.Target(c2)},
		}, c2)
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c2), New: corestore.Target(c1)},
		})

		// After the sync read the head (3) and indexed the packs (c2 is local
		// now), this node moves main from c1 to c2 as changeset 4.
		e.sy.SetTestHook(once("fetched", func() {
			e.localWrite(t, repo.ID, corestore.RefUpdate{
				Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c1), New: corestore.Target(c2),
			})
		}))

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		refs, err := gitops.ReadRefs(e.ctx, e.localRepo())
		require.NoError(t, err)
		require.Equal(t, corestore.Target(c2), refs["refs/heads/main"])
	})
}

// A call that joins a sync which read the database head before the call
// arrived is not answered by it: it sees a changeset committed in between.
func TestSyncerLateJoinerSeesNewerChangeset(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		e.advanceMain(t, repo.ID, src, c1) // changeset 2

		joined := make(chan error, 1)
		e.sy.SetTestHook(once("fetched", func() {
			// The sync of changeset 2 has read the head. Another node commits
			// changeset 3, then a request arrives on this node.
			e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
				{Name: "refs/heads/other", Expect: corestore.ExpectValue, New: corestore.Target(c1)},
			})
			go func() { joined <- e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)) }()
			// Leave the late caller the time to join the running sync.
			time.Sleep(200 * time.Millisecond)
		}))

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		require.NoError(t, <-joined)

		wm, ok, err := gitops.ReadWatermark(e.localPath())
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, uint64(3), wm.Seq, "the late caller saw changeset 3")
		e.verifySynced(t, repo)
	})
}

// Outside authoritative mode NoteCommitted does nothing: nothing is
// remembered and the watermark is not written on the shadow write path.
func TestSyncerNoteCommittedIgnoredOutsideAuthoritative(t *testing.T) {
	for _, mode := range []corestore.Mode{corestore.ModeShadow, corestore.ModeOff} {
		t.Run(string(mode), func(t *testing.T) {
			e := newEnv(t, envOptions{mode: mode})
			gittest.CreateRepository(t, e.ctx, e.cfg, gittest.CreateRepositoryConfig{
				RelativePath:           localRelPath,
				SkipCreationViaService: true,
			})
			require.NoError(t, gitops.WriteWatermark(e.localPath(), corestore.Watermark{RepoID: 7, Seq: 1}))

			e.sy.NoteCommitted(e.ctx, e.localPath(), 7, 2)

			wm, ok, err := gitops.ReadWatermark(e.localPath())
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, corestore.Watermark{RepoID: 7, Seq: 1}, wm)
		})
	}
}

// The stress version of the race of the round-1 review: a local writer (the
// only one that changes main) runs the hook sequence on main, another node
// commits on another branch, and two loops call EnsureFresh. A catch-up that
// reverted the local main would make the local writer's next database CAS
// fail; at the end the local repository must equal the database.
func TestSyncerConcurrentLocalWritesAreNeverReverted(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		c2 := e.writeCommit(t, src, c1)
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/remote", Expect: corestore.ExpectValue, New: corestore.Target(c2)},
		}, c2)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		tree := git.ObjectID(strings.TrimSpace(string(gittest.Exec(t, e.cfg, "-C", e.localPath(), "rev-parse", c1.String()+"^{tree}"))))
		local := e.localRepo()

		const duration = 3 * time.Second
		deadline := time.Now().Add(duration)
		var (
			wg          sync.WaitGroup
			stop        atomic.Bool
			writes      atomic.Int64
			conflicts   atomic.Int64
			errMu       sync.Mutex
			unexpected  []error
			syncErrors  []error
			remoteFlips atomic.Int64
		)
		record := func(list *[]error, err error) {
			errMu.Lock()
			defer errMu.Unlock()
			*list = append(*list, err)
		}

		// The local writer: the hook sequence of a write on this node.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				values, err := gitops.ReadRefValues(e.ctx, local, []string{"refs/heads/main"})
				if err != nil {
					record(&unexpected, err)
					return
				}
				old := git.ObjectID(values["refs/heads/main"])
				var stdout bytes.Buffer
				if err := local.ExecAndWait(e.ctx, gitcmd.Command{
					Name:  "commit-tree",
					Flags: []gitcmd.Option{gitcmd.ValueFlag{Name: "-p", Value: old.String()}, gitcmd.ValueFlag{Name: "-m", Value: fmt.Sprintf("local %d", i)}},
					Args:  []string{tree.String()},
				}, gitcmd.WithStdout(&stdout), gitcmd.WithEnv(
					"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com",
					"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com",
				)); err != nil {
					record(&unexpected, err)
					return
				}
				next := git.ObjectID(strings.TrimSpace(stdout.String()))

				u, err := updateref.New(e.ctx, local, updateref.WithDisabledTransactions())
				if err != nil {
					record(&unexpected, err)
					return
				}
				if err := u.Start(); err != nil {
					_ = u.Close()
					continue
				}
				if err := u.Update("refs/heads/main", next, old); err != nil {
					_ = u.Close()
					continue
				}
				if err := u.Prepare(); err != nil {
					// The syncer holds the lock, or moved main: try again.
					_ = u.Close()
					continue
				}

				res, err := e.core.Meta.CommitChangeset(e.ctx, corestore.ChangesetInput{
					RepoID: repo.ID, Operation: "test", Actor: "tester", NodeID: "node-a",
					Updates: []corestore.RefUpdate{{
						Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(old), New: corestore.Target(next),
					}},
				})
				if err != nil {
					_ = u.Close()
					var conflict *corestore.ErrRefConflict
					if errors.As(err, &conflict) {
						conflicts.Add(1)
						continue
					}
					record(&unexpected, err)
					return
				}
				if err := u.Commit(); err != nil {
					_ = u.Close()
					record(&unexpected, fmt.Errorf("local commit after the database commit: %w", err))
					return
				}
				_ = u.Close()
				e.sy.NoteCommitted(e.ctx, e.localPath(), repo.ID, res.Seq)
				writes.Add(1)
			}
		}()

		// Another node flips its branch every 3 ms.
		wg.Add(1)
		go func() {
			defer wg.Done()
			targets := []git.ObjectID{c1, c2}
			for i := 0; !stop.Load(); i++ {
				if _, err := e.core.Meta.CommitChangeset(e.ctx, corestore.ChangesetInput{
					RepoID: repo.ID, Operation: "test", Actor: "tester", NodeID: otherNode,
					Updates: []corestore.RefUpdate{{Name: "refs/heads/remote", Expect: corestore.ExpectAny, New: corestore.Target(targets[i%2])}},
				}); err != nil {
					record(&unexpected, err)
					return
				}
				remoteFlips.Add(1)
				time.Sleep(3 * time.Millisecond)
			}
		}()

		// Two request loops.
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stop.Load() {
					if err := e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)); err != nil {
						record(&syncErrors, err)
					}
				}
			}()
		}

		time.Sleep(time.Until(deadline))
		stop.Store(true)
		wg.Wait()

		require.Empty(t, unexpected)
		require.Empty(t, syncErrors)
		require.Positive(t, writes.Load(), "the local writer made progress")
		require.Positive(t, remoteFlips.Load())
		require.Zero(t, conflicts.Load(), "a catch-up reverted the local main")

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
	})
}
