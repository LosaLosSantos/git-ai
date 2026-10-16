package dblog_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/dblog"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

// oid returns a 40 hex character object ID for n.
func oid(n int) string { return fmt.Sprintf("%040x", n+1) }

// newRepo registers a repository named name and returns its ID.
func newRepo(t *testing.T, ctx context.Context, meta corestore.MetaStore, name string) int64 {
	t.Helper()

	repo, _, err := meta.RegisterRepository(ctx,
		corestore.RepoKey{Storage: "default", RelativePath: name},
		"sha1", corestore.InitialState{})
	require.NoError(t, err)
	return repo.ID
}

// commitBranch records a changeset creating branch refs/heads/b<i>, which
// appends exactly one changelog entry.
func commitBranch(t *testing.T, ctx context.Context, meta corestore.MetaStore, repoID int64, i int, actor string) {
	t.Helper()

	_, err := meta.CommitChangeset(ctx, corestore.ChangesetInput{
		RepoID: repoID,
		Updates: []corestore.RefUpdate{{
			Name:   fmt.Sprintf("refs/heads/b%d", i),
			Expect: corestore.ExpectValue,
			New:    corestore.Target(oid(i)),
		}},
		Operation: "land",
		Actor:     actor,
		NodeID:    "node-a",
		Changelog: &changelog.Meta{Operation: "land", Actor: actor},
	})
	require.NoError(t, err)
}

// scanAll collects every entry of src after the given sequence.
func scanAll(t *testing.T, src changelog.Source, after uint64) []changelog.Entry {
	t.Helper()

	var entries []changelog.Entry
	require.NoError(t, src.Scan(testhelper.Context(t), after, func(e changelog.Entry) (bool, error) {
		entries = append(entries, e)
		return true, nil
	}))
	return entries
}

func TestScan(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			ctx := testhelper.Context(t)
			core := coretest.Open(t, coretest.Options{Dialect: dialect})
			repoID := newRepo(t, ctx, core.Meta, "repo")

			for i := 0; i < 3; i++ {
				commitBranch(t, ctx, core.Meta, repoID, i, "alice")
			}

			t.Run("all entries in order", func(t *testing.T) {
				entries := scanAll(t, dblog.New(core.Meta, repoID), 0)
				require.Len(t, entries, 3)
				for i, e := range entries {
					require.Equal(t, uint64(i+1), e.Sequence)
					require.Equal(t, fmt.Sprintf("refs/heads/b%d", i), e.Reference)
					require.Equal(t, oid(i), e.NewOID)
					require.Equal(t, "land", e.Operation)
					require.Equal(t, "alice", e.Actor)
				}
			})

			t.Run("after a sequence", func(t *testing.T) {
				entries := scanAll(t, dblog.New(core.Meta, repoID), 1)
				require.Len(t, entries, 2)
				require.Equal(t, uint64(2), entries[0].Sequence)
				require.Equal(t, uint64(3), entries[1].Sequence)
			})

			t.Run("after the last sequence", func(t *testing.T) {
				require.Empty(t, scanAll(t, dblog.New(core.Meta, repoID), 3))
			})

			t.Run("stops when the callback returns false", func(t *testing.T) {
				var seen []uint64
				require.NoError(t, dblog.New(core.Meta, repoID).Scan(ctx, 0, func(e changelog.Entry) (bool, error) {
					seen = append(seen, e.Sequence)
					return false, nil
				}))
				require.Equal(t, []uint64{1}, seen)
			})

			t.Run("callback error is returned", func(t *testing.T) {
				err := dblog.New(core.Meta, repoID).Scan(ctx, 0, func(changelog.Entry) (bool, error) {
					return false, fmt.Errorf("stop here")
				})
				require.EqualError(t, err, "stop here")
			})

			t.Run("repository without entries", func(t *testing.T) {
				other := newRepo(t, ctx, core.Meta, "empty")
				require.Empty(t, scanAll(t, dblog.New(core.Meta, other), 0))
			})
		})
	}
}

func TestScanPagesAcrossDatabasePages(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			ctx := testhelper.Context(t)
			core := coretest.Open(t, coretest.Options{Dialect: dialect})
			repoID := newRepo(t, ctx, core.Meta, "repo")

			// More than one page of 500 entries, so that the scan must continue
			// from the last sequence of the first page.
			const n = 503
			for i := 0; i < n; i++ {
				commitBranch(t, ctx, core.Meta, repoID, i, "alice")
			}

			entries := scanAll(t, dblog.New(core.Meta, repoID), 0)
			require.Len(t, entries, n)
			for i, e := range entries {
				require.Equal(t, uint64(i+1), e.Sequence)
			}
		})
	}
}

func TestScanIsolatesRepositories(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			ctx := testhelper.Context(t)
			core := coretest.Open(t, coretest.Options{Dialect: dialect})
			a := newRepo(t, ctx, core.Meta, "a")
			b := newRepo(t, ctx, core.Meta, "b")

			commitBranch(t, ctx, core.Meta, a, 0, "alice")
			commitBranch(t, ctx, core.Meta, b, 0, "bob")
			commitBranch(t, ctx, core.Meta, b, 1, "bob")

			entriesA := scanAll(t, dblog.New(core.Meta, a), 0)
			require.Len(t, entriesA, 1)
			require.Equal(t, "alice", entriesA[0].Actor)

			entriesB := scanAll(t, dblog.New(core.Meta, b), 0)
			require.Len(t, entriesB, 2)
			require.Equal(t, uint64(1), entriesB[0].Sequence)
		})
	}
}

func TestScanKeepsMembers(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			ctx := testhelper.Context(t)
			core := coretest.Open(t, coretest.Options{Dialect: dialect})
			repoID := newRepo(t, ctx, core.Meta, "repo")

			members := []changelog.Member{
				{OriginalCommit: oid(10), Commit: oid(11), Actor: "agent-1", Rewritten: true, ResolvedPaths: []string{"a.txt"}},
				{OriginalCommit: oid(12), Commit: oid(12), Actor: "agent-2"},
			}

			_, err := core.Meta.CommitChangeset(ctx, corestore.ChangesetInput{
				RepoID: repoID,
				Updates: []corestore.RefUpdate{
					{Name: "refs/heads/main", Expect: corestore.ExpectValue, New: corestore.Target(oid(11))},
					{Name: "refs/heads/other", Expect: corestore.ExpectValue, New: corestore.Target(oid(12))},
				},
				Operation: changelog.OperationLandBatch,
				Actor:     "agent-1",
				NodeID:    "node-a",
				Changelog: &changelog.Meta{
					Operation: changelog.OperationLandBatch,
					Actor:     "agent-1",
					Members:   members,
				},
			})
			require.NoError(t, err)

			entries := scanAll(t, dblog.New(core.Meta, repoID), 0)
			require.Len(t, entries, 2)
			for i, e := range entries {
				require.Equal(t, uint64(i+1), e.Sequence)
				require.Equal(t, changelog.OperationLandBatch, e.Operation)
				require.Equal(t, members, e.Members)
			}
		})
	}
}

func TestScanCancelledContext(t *testing.T) {
	ctx := testhelper.Context(t)
	core := coretest.Open(t, coretest.Options{})
	repoID := newRepo(t, ctx, core.Meta, "repo")
	commitBranch(t, ctx, core.Meta, repoID, 0, "alice")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	err := dblog.New(core.Meta, repoID).Scan(cancelled, 0, func(changelog.Entry) (bool, error) {
		return true, nil
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestSubscribeWakesOnNotification(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			ctx := testhelper.Context(t)
			core := coretest.Open(t, coretest.Options{Dialect: dialect})
			repoID := newRepo(t, ctx, core.Meta, "repo")

			wake, cancel := dblog.New(core.Meta, repoID).Subscribe()
			defer cancel()

			corestore.NotifyChangelog(repoID)

			select {
			case <-wake:
			case <-time.After(time.Second):
				require.FailNow(t, "no wake-up after a notification")
			}
		})
	}
}

func TestSubscribePollsForOtherWriters(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			ctx := testhelper.Context(t)
			core := coretest.Open(t, coretest.Options{Dialect: dialect})
			repoID := newRepo(t, ctx, core.Meta, "repo")

			wake, cancel := dblog.New(core.Meta, repoID).Subscribe()
			defer cancel()

			// No notification: the append stands for one made by another node.
			commitBranch(t, ctx, core.Meta, repoID, 0, "carol")

			select {
			case <-wake:
			case <-time.After(2 * time.Second):
				require.FailNow(t, "no wake-up by polling after an append")
			}
		})
	}
}

func TestSubscribeCoalesces(t *testing.T) {
	ctx := testhelper.Context(t)
	core := coretest.Open(t, coretest.Options{})
	repoID := newRepo(t, ctx, core.Meta, "repo")

	wake, cancel := dblog.New(core.Meta, repoID).Subscribe()
	defer cancel()

	// Many notifications while nobody reads leave one pending value: the
	// writer never blocks.
	for i := 0; i < 100; i++ {
		corestore.NotifyChangelog(repoID)
	}

	select {
	case <-wake:
	case <-time.After(time.Second):
		require.FailNow(t, "no wake-up after notifications")
	}
	require.LessOrEqual(t, len(wake), 1)
}

func TestSubscribeCancel(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			ctx := testhelper.Context(t)
			core := coretest.Open(t, coretest.Options{Dialect: dialect})
			repoID := newRepo(t, ctx, core.Meta, "repo")

			_, cancel := dblog.New(core.Meta, repoID).Subscribe()
			cancel()
			cancel() // a second call must not panic

			// Notifying after the cancel must not block or panic.
			corestore.NotifyChangelog(repoID)
		})
	}
}
