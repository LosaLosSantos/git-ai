package sqlstore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// forEachDialect runs fn once per dialect of coretest.Dialects, with a new
// database each.
func forEachDialect(t *testing.T, fn func(t *testing.T, ctx context.Context, s *sqlstore.Store)) {
	t.Helper()

	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			ctx := testhelper.Context(t)
			s := openStore(t, ctx, coretest.DatabaseURL(t, dialect))
			fn(t, ctx, s)
		})
	}
}

// openedDSNs maps the stores opened by the tests to their database URLs.
var openedDSNs sync.Map

// openStore opens a store on dsn and closes it when the test ends.
func openStore(t *testing.T, ctx context.Context, dsn string) *sqlstore.Store {
	t.Helper()

	s, err := sqlstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	openedDSNs.Store(s, dsn)

	return s
}

// coreDSN returns the database URL that a store was opened with.
func coreDSN(t *testing.T, s *sqlstore.Store) string {
	t.Helper()

	dsn, ok := openedDSNs.Load(s)
	require.True(t, ok, "store was not opened by openStore")
	return dsn.(string)
}

// testKey returns the key of a test repository.
func testKey(name string) corestore.RepoKey {
	return corestore.RepoKey{Storage: "default", RelativePath: "@hashed/" + name + ".git"}
}

// register registers an empty sha1 repository.
func register(t *testing.T, ctx context.Context, s *sqlstore.Store, key corestore.RepoKey) corestore.Repository {
	t.Helper()

	repo, _, err := s.RegisterRepository(ctx, key, "sha1", corestore.InitialState{})
	require.NoError(t, err)
	return repo
}

// oid returns a sha1 object ID made of n.
func oid(n int) corestore.Target {
	return corestore.Target(fmt.Sprintf("%040x", n))
}

// commit commits a changeset of the updates, with no changelog.
func commit(ctx context.Context, s *sqlstore.Store, repoID int64, updates ...corestore.RefUpdate) (corestore.CommitResult, error) {
	return s.CommitChangeset(ctx, corestore.ChangesetInput{
		RepoID:    repoID,
		Updates:   updates,
		Operation: "test",
		Actor:     "tester",
		NodeID:    "node-a",
	})
}

// create expects the reference to be absent.
func create(name string, newValue corestore.Target) corestore.RefUpdate {
	return corestore.RefUpdate{Name: name, Expect: corestore.ExpectValue, Old: "", New: newValue}
}

// update expects the reference to be old.
func update(name string, old, newValue corestore.Target) corestore.RefUpdate {
	return corestore.RefUpdate{Name: name, Expect: corestore.ExpectValue, Old: old, New: newValue}
}

// force overwrites the reference whatever its value.
func force(name string, newValue corestore.Target) corestore.RefUpdate {
	return corestore.RefUpdate{Name: name, Expect: corestore.ExpectAny, New: newValue}
}

// requireRefs checks the references of the repository in the database.
func requireRefs(t *testing.T, ctx context.Context, s *sqlstore.Store, repoID int64, want map[string]corestore.Target) {
	t.Helper()

	got, _, err := s.Refs(ctx, repoID)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// newReview returns a review with the fields that the tests check.
func newReview(title string, target string, state gitalypb.Review_State) *gitalypb.Review {
	return &gitalypb.Review{
		TargetBranch: []byte(target),
		Title:        []byte(title),
		HeadCommitId: string(oid(1)),
		State:        state,
		UpdatedAt:    nil,
	}
}

func TestParseDSN(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		dsn     string
		dialect sqlstore.Dialect
		target  string
		err     bool
	}{
		{dsn: "sqlite:///var/opt/meta.db", dialect: sqlstore.SQLite, target: "/var/opt/meta.db"},
		{dsn: "postgres://u@h/db?sslmode=disable", dialect: sqlstore.Postgres, target: "postgres://u@h/db?sslmode=disable"},
		{dsn: "postgresql://u@h/db", dialect: sqlstore.Postgres, target: "postgresql://u@h/db"},
		{dsn: "sqlite://relative.db", err: true},
		{dsn: "sqlite://host/abs.db", err: true},
		{dsn: "mysql://x", err: true},
	} {
		dialect, target, err := sqlstore.ParseDSN(tc.dsn)
		if tc.err {
			require.Error(t, err, tc.dsn)
			continue
		}
		require.NoError(t, err, tc.dsn)
		require.Equal(t, tc.dialect, dialect)
		require.Equal(t, tc.target, target)
	}
}

func TestOpenTwiceAppliesMigrationsOnce(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		register(t, ctx, s, testKey("twice"))

		second := openStore(t, ctx, coreDSN(t, s))
		repo, err := second.GetRepository(ctx, testKey("twice"))
		require.NoError(t, err)
		require.NotZero(t, repo.ID)
	})
}

func TestCASTable(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("cas"))

		// Absent expected absent: created.
		res, err := commit(ctx, s, repo.ID, create("refs/heads/a", oid(1)))
		require.NoError(t, err)
		require.Equal(t, uint64(1), res.Seq)
		require.Empty(t, res.Conflicts)

		// Non-zero old equal to the current value: updated.
		res, err = commit(ctx, s, repo.ID, update("refs/heads/a", oid(1), oid(2)))
		require.NoError(t, err)
		require.Equal(t, uint64(2), res.Seq)
		require.Equal(t, corestore.AppliedUpdate{Name: "refs/heads/a", Old: oid(1), New: oid(2)}, res.Updates[0])

		// Non-zero old that differs: conflict, nothing written.
		_, err = commit(ctx, s, repo.ID, update("refs/heads/a", oid(1), oid(3)))
		var conflict *corestore.ErrRefConflict
		require.True(t, errors.As(err, &conflict), "got %v", err)
		require.Equal(t, []corestore.RefConflict{{Name: "refs/heads/a", Expected: oid(1), Actual: oid(2)}}, conflict.Conflicts)
		requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{"refs/heads/a": oid(2)})

		// Absent expected but present: conflict.
		_, err = commit(ctx, s, repo.ID, create("refs/heads/a", oid(4)))
		require.True(t, errors.As(err, &conflict), "got %v", err)

		// Absent expected, deleting an absent reference: no change.
		res, err = commit(ctx, s, repo.ID, update("refs/heads/missing", "", ""))
		require.NoError(t, err)
		require.True(t, res.NoOp)
		require.Equal(t, uint64(0), res.Seq)

		// Expected value on an absent reference: conflict with an absent actual.
		_, err = commit(ctx, s, repo.ID, update("refs/heads/missing", oid(9), oid(5)))
		require.True(t, errors.As(err, &conflict), "got %v", err)
		require.Equal(t, corestore.Target(""), conflict.Conflicts[0].Actual)

		// Forced update overwrites whatever is there.
		res, err = commit(ctx, s, repo.ID, force("refs/heads/a", oid(6)))
		require.NoError(t, err)
		require.False(t, res.Forced)
		require.Empty(t, res.Conflicts)
		requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{"refs/heads/a": oid(6)})

		// Forced deletion of an absent reference writes nothing.
		res, err = commit(ctx, s, repo.ID, force("refs/heads/none", ""))
		require.NoError(t, err)
		require.True(t, res.NoOp)
		require.Equal(t, uint64(0), res.Seq)

		// Deletion with the expected value deletes the reference.
		res, err = commit(ctx, s, repo.ID, update("refs/heads/a", oid(6), ""))
		require.NoError(t, err)
		require.Equal(t, corestore.AppliedUpdate{Name: "refs/heads/a", Old: oid(6), New: ""}, res.Updates[0])
		requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{})
	})
}

func TestForcedCommitRecordsConflicts(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("forced"))
		_, err := commit(ctx, s, repo.ID, create("refs/heads/main", oid(1)))
		require.NoError(t, err)

		res, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID:  repo.ID,
			Updates: []corestore.RefUpdate{update("refs/heads/main", oid(7), oid(2))},
			Force:   true,
		})
		require.NoError(t, err)
		require.True(t, res.Forced)
		require.Equal(t, uint64(2), res.Seq)
		require.Equal(t, []corestore.RefConflict{{Name: "refs/heads/main", Expected: oid(7), Actual: oid(1)}}, res.Conflicts)
		requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{"refs/heads/main": oid(2)})

		changesets, err := s.ListChangesets(ctx, repo.ID, 0, 2)
		require.NoError(t, err)
		require.Len(t, changesets, 2)
		require.False(t, changesets[0].Forced)
		require.True(t, changesets[1].Forced)
	})
}

func TestNoOpWritesNothing(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("noop"))
		_, err := commit(ctx, s, repo.ID, create("refs/heads/main", oid(1)))
		require.NoError(t, err)

		res, err := commit(ctx, s, repo.ID, update("refs/heads/main", oid(1), oid(1)))
		require.NoError(t, err)
		require.True(t, res.NoOp)
		require.Equal(t, uint64(0), res.Seq)

		_, head, err := s.Refs(ctx, repo.ID)
		require.NoError(t, err)
		require.Equal(t, uint64(1), head)

		changesets, err := s.ListChangesets(ctx, repo.ID, 0, 10)
		require.NoError(t, err)
		require.Len(t, changesets, 1)
	})
}

func TestInvalidChangesetsAreRejected(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("invalid"))

		_, err := commit(ctx, s, repo.ID)
		require.Error(t, err)

		_, err = commit(ctx, s, repo.ID, create("", oid(1)))
		require.Error(t, err)

		_, err = commit(ctx, s, repo.ID, create("refs/heads/a", oid(1)), create("refs/heads/a", oid(2)))
		require.Error(t, err)

		_, err = commit(ctx, s, repo.ID, corestore.RefUpdate{Name: "refs/heads/a", Expect: corestore.Expect(42), New: oid(1)})
		require.Error(t, err)

		_, err = commit(ctx, s, 987654, create("refs/heads/a", oid(1)))
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)

		// Nothing was written by the rejected changesets.
		_, head, err := s.Refs(ctx, repo.ID)
		require.NoError(t, err)
		require.Equal(t, uint64(0), head)
	})
}

func TestNonUTF8ReferenceNamesRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("utf8"))
		name := "refs/heads/bad-\xff\xfe-name"

		_, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID:    repo.ID,
			Updates:   []corestore.RefUpdate{create(name, oid(1))},
			Changelog: &changelog.Meta{Operation: "push"},
		})
		require.NoError(t, err)
		requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{name: oid(1)})

		changesets, err := s.ListChangesets(ctx, repo.ID, 0, 1)
		require.NoError(t, err)
		require.Equal(t, name, changesets[0].Updates[0].Name)

		entries, err := s.ScanChangelog(ctx, repo.ID, 0, 0)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, name, entries[0].Reference)
	})
}

func TestChangelogEntries(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("changelog"))
		members := []changelog.Member{{OriginalCommit: "a", Commit: "b", Actor: "carol"}}

		_, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID: repo.ID,
			Updates: []corestore.RefUpdate{
				create("refs/heads/main", oid(1)),
				create("refs/tags/v1", oid(1)),
				create("refs/notes/commits", oid(1)),
				force("HEAD", corestore.SymbolicTarget("refs/heads/main")),
			},
			Changelog: &changelog.Meta{Operation: "ssh_receive_pack", Actor: "alice\xff", Members: members},
		})
		require.NoError(t, err)

		// A move and a deletion. The deletion's new value is the zero OID.
		_, err = s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID: repo.ID,
			Updates: []corestore.RefUpdate{
				update("refs/heads/main", oid(1), oid(2)),
				update("refs/tags/v1", oid(1), ""),
			},
			Changelog: &changelog.Meta{Operation: "rest", Actor: "bob", Rewritten: true},
		})
		require.NoError(t, err)

		entries, err := s.ScanChangelog(ctx, repo.ID, 0, 0)
		require.NoError(t, err)
		require.Len(t, entries, 4)

		zero := strings.Repeat("0", 40)

		require.Equal(t, uint64(1), entries[0].Sequence)
		require.Equal(t, "refs/heads/main", entries[0].Reference)
		require.Equal(t, zero, entries[0].OldOID)
		require.Equal(t, string(oid(1)), entries[0].NewOID)
		require.Equal(t, "ssh_receive_pack", entries[0].Operation)
		require.Equal(t, "alice�", entries[0].Actor)
		require.Equal(t, members, entries[0].Members)
		require.False(t, entries[0].RecordedAt.IsZero())

		require.Equal(t, uint64(2), entries[1].Sequence)
		require.Equal(t, "refs/tags/v1", entries[1].Reference)

		require.Equal(t, uint64(3), entries[2].Sequence)
		require.Equal(t, "refs/heads/main", entries[2].Reference)
		require.Equal(t, string(oid(1)), entries[2].OldOID)
		require.Equal(t, string(oid(2)), entries[2].NewOID)
		require.Equal(t, "bob", entries[2].Actor)
		require.True(t, entries[2].Rewritten)

		require.Equal(t, uint64(4), entries[3].Sequence)
		require.Equal(t, "refs/tags/v1", entries[3].Reference)
		require.Equal(t, zero, entries[3].NewOID)
		require.Equal(t, string(oid(1)), entries[3].OldOID)

		fromTwo, err := s.ScanChangelog(ctx, repo.ID, 2, 1)
		require.NoError(t, err)
		require.Len(t, fromTwo, 1)
		require.Equal(t, uint64(3), fromTwo[0].Sequence)

		updated, err := s.GetRepository(ctx, testKey("changelog"))
		require.NoError(t, err)
		require.Equal(t, uint64(4), updated.LogSeq)
		require.Equal(t, uint64(2), updated.HeadSeq)
	})
}

func TestChangelogNotWrittenWithoutMeta(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("nolog"))
		_, err := commit(ctx, s, repo.ID, create("refs/heads/main", oid(1)))
		require.NoError(t, err)

		entries, err := s.ScanChangelog(ctx, repo.ID, 0, 0)
		require.NoError(t, err)
		require.Empty(t, entries)
	})
}

func TestRegisterWithHistory(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		key := testKey("history")
		recorded := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
		history := []changelog.Entry{
			{Sequence: 1, Reference: "refs/heads/main", OldOID: strings.Repeat("0", 40), NewOID: string(oid(1)), Operation: "import", Actor: "old", RecordedAt: recorded},
			{Sequence: 5, Reference: "refs/tags/v1", OldOID: strings.Repeat("0", 40), NewOID: string(oid(1)), Operation: "import", Actor: "old", RecordedAt: recorded},
		}
		reviews := []*gitalypb.Review{
			newReview("first", "main", gitalypb.Review_STATE_OPEN_UNSPECIFIED),
		}
		reviews[0].Id = "1"
		closed := newReview("closed", "main", gitalypb.Review_STATE_LANDED)
		closed.Id = "3"
		reviews = append(reviews, closed)

		repo, res, err := s.RegisterRepository(ctx, key, "sha1", corestore.InitialState{
			Changeset: &corestore.ChangesetInput{
				Operation: "create_repository",
				Actor:     "creator",
				NodeID:    "node-a",
				Updates:   []corestore.RefUpdate{create("HEAD", corestore.SymbolicTarget("refs/heads/main"))},
			},
			Changelog: history,
			Reviews:   reviews,
		})
		require.NoError(t, err)
		require.Equal(t, uint64(1), res.Seq)
		require.Equal(t, uint64(1), repo.HeadSeq)
		require.Equal(t, uint64(5), repo.LogSeq)
		require.Equal(t, "sha1", repo.ObjectFormat)

		// A later changelog entry continues after the imported history.
		later, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID:    repo.ID,
			Updates:   []corestore.RefUpdate{create("refs/heads/feature", oid(2))},
			Changelog: &changelog.Meta{Operation: "rest", Actor: "bob"},
		})
		require.NoError(t, err)
		require.Equal(t, uint64(2), later.Seq)
		require.Equal(t, uint64(6), later.Changelog[0].Sequence)

		entries, err := s.ScanChangelog(ctx, repo.ID, 0, 0)
		require.NoError(t, err)
		require.Len(t, entries, 3)
		require.Equal(t, uint64(1), entries[0].Sequence)
		require.Equal(t, uint64(5), entries[1].Sequence)
		require.Equal(t, uint64(6), entries[2].Sequence)

		// Imported reviews keep their IDs, and the next ID follows the largest.
		open, version, err := s.GetReview(ctx, repo.ID, "1")
		require.NoError(t, err)
		require.Equal(t, int64(1), version)
		require.Equal(t, "first", string(open.GetTitle()))

		inserted, err := s.InsertReview(ctx, repo.ID, func(id string) (*gitalypb.Review, error) {
			require.Equal(t, "4", id)
			return newReview("new", "main", gitalypb.Review_STATE_OPEN_UNSPECIFIED), nil
		})
		require.NoError(t, err)
		require.Equal(t, "4", inserted.GetId())

		// Registering the same key again fails.
		_, _, err = s.RegisterRepository(ctx, key, "sha1", corestore.InitialState{})
		require.ErrorIs(t, err, corestore.ErrRepositoryExists)

		// Invalid history and initial changesets are rejected without writing.
		_, _, err = s.RegisterRepository(ctx, testKey("bad-history"), "sha1", corestore.InitialState{
			Changelog: []changelog.Entry{{Sequence: 2}, {Sequence: 2}},
		})
		require.Error(t, err)
		_, err = s.GetRepository(ctx, testKey("bad-history"))
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)

		_, _, err = s.RegisterRepository(ctx, testKey("bad-initial"), "sha1", corestore.InitialState{
			Changeset: &corestore.ChangesetInput{Updates: []corestore.RefUpdate{update("HEAD", oid(1), oid(2))}},
		})
		require.Error(t, err)
		_, err = s.GetRepository(ctx, testKey("bad-initial"))
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
	})
}

func TestDeleteCascadesAndReRegisterGetsNewID(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		key := testKey("delete")
		first := register(t, ctx, s, key)
		_, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID:    first.ID,
			Updates:   []corestore.RefUpdate{create("refs/heads/main", oid(1))},
			Changelog: &changelog.Meta{Operation: "push"},
		})
		require.NoError(t, err)
		_, err = s.InsertReview(ctx, first.ID, func(id string) (*gitalypb.Review, error) {
			return newReview("doomed", "main", gitalypb.Review_STATE_OPEN_UNSPECIFIED), nil
		})
		require.NoError(t, err)

		deleted, err := s.DeleteRepository(ctx, key)
		require.NoError(t, err)
		require.True(t, deleted)

		deleted, err = s.DeleteRepository(ctx, key)
		require.NoError(t, err)
		require.False(t, deleted)

		_, err = s.GetRepository(ctx, key)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
		_, _, err = s.Refs(ctx, first.ID)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)

		entries, err := s.ScanChangelog(ctx, first.ID, 0, 0)
		require.NoError(t, err)
		require.Empty(t, entries)

		second := register(t, ctx, s, key)
		require.Greater(t, second.ID, first.ID)

		// The new repository starts empty: no reference, no changelog, no review.
		requireRefs(t, ctx, s, second.ID, map[string]corestore.Target{})
		reviews, err := s.ListReviews(ctx, second.ID, "", true, 0)
		require.NoError(t, err)
		require.Empty(t, reviews)
	})
}

func TestListChangesetsRanges(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("ranges"))
		for i := 1; i <= 5; i++ {
			_, err := commit(ctx, s, repo.ID, force("refs/heads/main", oid(i)))
			require.NoError(t, err)
		}

		seqs := func(cs []corestore.Changeset) []uint64 {
			out := make([]uint64, 0, len(cs))
			for _, c := range cs {
				out = append(out, c.Seq)
			}
			return out
		}

		all, err := s.ListChangesets(ctx, repo.ID, 0, 5)
		require.NoError(t, err)
		require.Equal(t, []uint64{1, 2, 3, 4, 5}, seqs(all))

		middle, err := s.ListChangesets(ctx, repo.ID, 1, 4)
		require.NoError(t, err)
		require.Equal(t, []uint64{2, 3, 4}, seqs(middle))
		require.Equal(t, oid(3), middle[1].Updates[0].New)
		require.Equal(t, oid(2), middle[1].Updates[0].Old)

		beyond, err := s.ListChangesets(ctx, repo.ID, 4, 100)
		require.NoError(t, err)
		require.Equal(t, []uint64{5}, seqs(beyond))

		none, err := s.ListChangesets(ctx, repo.ID, 5, 5)
		require.NoError(t, err)
		require.Empty(t, none)
	})
}

func TestReportApplied(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("applied"))
		require.NoError(t, s.ReportApplied(ctx, repo.ID, "node-a", 1))
		require.NoError(t, s.ReportApplied(ctx, repo.ID, "node-a", 3))
		require.NoError(t, s.ReportApplied(ctx, repo.ID, "node-b", 2))

		// An unknown repository violates the foreign key.
		require.Error(t, s.ReportApplied(ctx, 987654, "node-a", 1))
	})
}
