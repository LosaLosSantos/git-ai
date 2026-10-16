package changeset

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// authoritativeEnv is a Gitaly server in authoritative mode: its changelog
// reads come from the database that node and peer share.
type authoritativeEnv struct {
	testEnv
	// node is the storage core of the server; peer is a second node on the same
	// database, which writes without the server being notified.
	node corestore.MetaStore
	peer corestore.MetaStore
	// repo is imported into the database as id; repoPath is its local git directory.
	repo     *gitalypb.Repository
	repoPath string
	id       int64
}

// setupAuthoritative starts a standalone Gitaly with the storage core in
// authoritative mode on dialect, and imports one repository into the database.
func setupAuthoritative(t *testing.T, dialect sqlstore.Dialect) authoritativeEnv {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true

	databaseURL := coretest.DatabaseURL(t, dialect)
	bucketURL := coretest.BucketURL(t, false)
	node := coretest.Open(t, coretest.Options{Dialect: dialect, DatabaseURL: databaseURL, BucketURL: bucketURL, NodeID: "node-a"})
	peer := coretest.Open(t, coretest.Options{Dialect: dialect, DatabaseURL: databaseURL, BucketURL: bucketURL, NodeID: "node-b"})

	env := setupChangesetServiceWith(t, ctx, cfg,
		testserver.WithGraftStorage(node),
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))),
	)

	// The repository is created on disk only and then imported, so that the
	// setup does not depend on what the authoritative gate does with
	// CreateRepository. The import is a real one: it leaves the local repository
	// complete and up to date, so the gate has nothing to refuse.
	repo, repoPath := gittest.CreateRepository(t, ctx, env.cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true, ReferenceBackend: git.ReferenceBackendReftables})
	imported := coretest.Import(t, node, localrepo.NewTestRepo(t, env.cfg, repo), repo, repoPath, coretest.ImportOptions{})

	return authoritativeEnv{
		testEnv:  env,
		node:     node.Meta,
		peer:     peer.Meta,
		repo:     repo,
		repoPath: repoPath,
		id:       imported.ID,
	}
}

// writeCommit writes a commit with the message to the local repository of env
// and returns its ID. No branch is updated, so the local references do not
// move: the commit is only an object the local node has, which is what a
// changeset of this node refers to.
func writeCommit(t *testing.T, env authoritativeEnv, message string) string {
	t.Helper()

	return gittest.WriteCommit(t, env.cfg, env.repoPath, gittest.WithMessage(message)).String()
}

// commitRef records a changeset that creates ref at oid, which appends one
// changelog entry, and returns its sequence. oid must be an object of the local
// repository, as it is for a commit written by writeCommit.
func commitRef(t *testing.T, meta corestore.MetaStore, repoID int64, ref, oid, actor string) uint64 {
	t.Helper()

	res, err := meta.CommitChangeset(testhelper.Context(t), corestore.ChangesetInput{
		RepoID: repoID,
		Updates: []corestore.RefUpdate{{
			Name:   ref,
			Expect: corestore.ExpectValue,
			New:    corestore.Target(oid),
		}},
		Operation: "land",
		Actor:     actor,
		NodeID:    "node",
		Changelog: &changelog.Meta{Operation: "land", Actor: actor},
	})
	require.NoError(t, err)
	return res.Seq
}

// authListStream is a ListChangesets server stream that collects the changesets.
type authListStream struct {
	grpc.ServerStream
	ctx        context.Context
	changesets []*gitalypb.Changeset
}

func (s *authListStream) Context() context.Context { return s.ctx }

func (s *authListStream) Send(resp *gitalypb.ListChangesetsResponse) error {
	s.changesets = append(s.changesets, resp.GetChangesets()...)
	return nil
}

// listDirect calls ListChangesets on the server implementation and returns the
// changesets.
func listDirect(t *testing.T, env authoritativeEnv, req *gitalypb.ListChangesetsRequest) []*gitalypb.Changeset {
	t.Helper()

	stream := &authListStream{ctx: testhelper.Context(t)}
	require.NoError(t, env.direct.ListChangesets(req, stream))
	return stream.changesets
}

// expectSeq waits for the next changeset of the watch and checks its sequence.
func expectSeq(t *testing.T, stream *fakeWatchStream, want uint64, within time.Duration, what string) {
	t.Helper()

	select {
	case got := <-stream.sent:
		require.Equal(t, want, got.GetSequence(), what)
	case <-time.After(within):
		require.FailNow(t, "timed out waiting for a changeset", what)
	}
}

func TestAuthoritativeListChangesets(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			env := setupAuthoritative(t, dialect)

			c1 := writeCommit(t, env, "c1")
			c2 := writeCommit(t, env, "c2")
			c3 := writeCommit(t, env, "c3")

			commitRef(t, env.node, env.id, "refs/heads/a", c1, "agent-a")
			commitRef(t, env.node, env.id, "refs/heads/b", c2, "agent-b")
			commitRef(t, env.node, env.id, "refs/tags/v1", c3, "agent-a")

			changesets := listDirect(t, env, &gitalypb.ListChangesetsRequest{Repository: env.repo})
			require.Len(t, changesets, 3)

			for i, c := range changesets {
				require.Equal(t, uint64(i+1), c.GetSequence())
			}

			first := changesets[0]
			require.Equal(t, []byte("refs/heads/a"), first.GetReference())
			require.Equal(t, string(gittest.DefaultObjectHash.ZeroOID), first.GetOldOid(), "a created reference has the zero old OID")
			require.Equal(t, c1, first.GetNewOid())
			require.Equal(t, "land", first.GetOperation())
			require.Equal(t, "agent-a", first.GetActor())
			require.NotNil(t, first.GetRecordedAt())
			require.Equal(t, "agent-b", changesets[1].GetActor())
			require.Equal(t, []byte("refs/tags/v1"), changesets[2].GetReference())

			// The changelog lives in the database: the file log is not written.
			_, err := os.Stat(changelog.Path(env.repoPath))
			require.True(t, os.IsNotExist(err), "authoritative mode writes no file changelog: %v", err)
		})
	}
}

func TestAuthoritativeListChangesetsThroughGRPC(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			env := setupAuthoritative(t, dialect)

			c1 := writeCommit(t, env, "c1")
			c2 := writeCommit(t, env, "c2")

			commitRef(t, env.node, env.id, "refs/heads/a", c1, "agent-a")
			commitRef(t, env.node, env.id, "refs/heads/b", c2, "agent-a")

			changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: env.repo})
			require.NoError(t, err)
			require.Len(t, changesets, 2)
			require.Equal(t, uint64(1), changesets[0].GetSequence())
			require.Equal(t, uint64(2), changesets[1].GetSequence())
		})
	}
}

func TestAuthoritativeListChangesetsAfterAndLimit(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			env := setupAuthoritative(t, dialect)

			for i := 1; i <= 3; i++ {
				commitRef(t, env.node, env.id, fmt.Sprintf("refs/heads/b%d", i), writeCommit(t, env, fmt.Sprintf("b%d", i)), "agent-a")
			}

			changesets := listDirect(t, env, &gitalypb.ListChangesetsRequest{
				Repository:    env.repo,
				AfterSequence: 1,
			})
			require.Len(t, changesets, 2)
			require.Equal(t, uint64(2), changesets[0].GetSequence())

			changesets = listDirect(t, env, &gitalypb.ListChangesetsRequest{
				Repository:    env.repo,
				AfterSequence: 1,
				Limit:         1,
			})
			require.Len(t, changesets, 1)
			require.Equal(t, uint64(2), changesets[0].GetSequence())

			require.Empty(t, listDirect(t, env, &gitalypb.ListChangesetsRequest{
				Repository:    env.repo,
				AfterSequence: 3,
			}))
		})
	}
}

// TestAuthoritativeListChangesetsMembers records what the write path records
// for a batched land (one changeset with several updates and the members of
// the batch) and checks that the entries are dense, carry the members, and
// leave out references that are not branches or tags.
func TestAuthoritativeListChangesetsMembers(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			env := setupAuthoritative(t, dialect)
			ctx := testhelper.Context(t)

			c10 := writeCommit(t, env, "c10")
			c11 := writeCommit(t, env, "c11")
			c12 := writeCommit(t, env, "c12")

			members := []changelog.Member{
				{OriginalCommit: c10, Commit: c11, Actor: "agent-1", Rewritten: true, ResolvedPaths: []string{"a.txt"}},
				{OriginalCommit: c12, Commit: c12, Actor: "agent-2"},
			}

			_, err := env.node.CommitChangeset(ctx, corestore.ChangesetInput{
				RepoID: env.id,
				Updates: []corestore.RefUpdate{
					{Name: "refs/heads/main", Expect: corestore.ExpectValue, New: corestore.Target(c11)},
					{Name: "refs/merge-requests/1/head", Expect: corestore.ExpectValue, New: corestore.Target(c11)},
					{Name: "refs/heads/release", Expect: corestore.ExpectValue, New: corestore.Target(c12)},
				},
				Operation: changelog.OperationLandBatch,
				Actor:     "agent-1",
				NodeID:    "node-a",
				Changelog: &changelog.Meta{
					Operation: changelog.OperationLandBatch,
					Actor:     "agent-1",
					Rewritten: true,
					Members:   members,
				},
			})
			require.NoError(t, err)

			changesets := listDirect(t, env, &gitalypb.ListChangesetsRequest{Repository: env.repo})
			require.Len(t, changesets, 2, "only branches and tags are logged")

			require.Equal(t, uint64(1), changesets[0].GetSequence())
			require.Equal(t, []byte("refs/heads/main"), changesets[0].GetReference())
			require.Equal(t, uint64(2), changesets[1].GetSequence())
			require.Equal(t, []byte("refs/heads/release"), changesets[1].GetReference())

			wantMembers := []*gitalypb.ChangesetMember{
				{OriginalCommitId: c10, CommitId: c11, Actor: "agent-1", Rewritten: true, ResolvedPaths: [][]byte{[]byte("a.txt")}},
				{OriginalCommitId: c12, CommitId: c12, Actor: "agent-2"},
			}
			for _, c := range changesets {
				require.Equal(t, changelog.OperationLandBatch, c.GetOperation())
				require.True(t, c.GetRewritten())
				testhelper.ProtoEqual(t, wantMembers, c.GetMembers())
			}
		})
	}
}

func TestAuthoritativeListChangesetsUnregistered(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			env := setupAuthoritative(t, dialect)

			// A repository that exists locally but was never imported.
			untracked, _ := gittest.CreateRepository(t, testhelper.Context(t), env.cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true, ReferenceBackend: git.ReferenceBackendReftables})

			stream := &authListStream{ctx: testhelper.Context(t)}
			err := env.direct.ListChangesets(&gitalypb.ListChangesetsRequest{Repository: untracked}, stream)
			require.Equal(t, codes.FailedPrecondition, status.Code(err), "error: %v", err)

			watch := &fakeWatchStream{ctx: testhelper.Context(t), sent: make(chan *gitalypb.Changeset, 1)}
			err = env.direct.WatchChangesets(&gitalypb.WatchChangesetsRequest{Repository: untracked}, watch)
			require.Equal(t, codes.FailedPrecondition, status.Code(err), "error: %v", err)
		})
	}
}

func TestAuthoritativeListChangesetsInvalidRepository(t *testing.T) {
	env := setupAuthoritative(t, coretest.Dialects()[0])

	stream := &authListStream{ctx: testhelper.Context(t)}
	err := env.direct.ListChangesets(&gitalypb.ListChangesetsRequest{
		Repository: &gitalypb.Repository{StorageName: env.repo.GetStorageName(), RelativePath: "does/not/exist.git"},
	}, stream)
	require.Error(t, err)
	require.NotEqual(t, codes.FailedPrecondition, status.Code(err), "the repository is validated before the database is asked: %v", err)
}

func TestAuthoritativeWatchChangesets(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			env := setupAuthoritative(t, dialect)

			c1 := writeCommit(t, env, "c1")
			c2 := writeCommit(t, env, "c2")
			c3 := writeCommit(t, env, "c3")

			commitRef(t, env.node, env.id, "refs/heads/a", c1, "agent-a")

			watchCtx, cancel := context.WithCancel(testhelper.Context(t))
			defer cancel()

			// The handler is called directly so that its return can be observed.
			stream := &fakeWatchStream{ctx: watchCtx, sent: make(chan *gitalypb.Changeset, 10)}
			done := make(chan error, 1)
			go func() {
				done <- env.direct.WatchChangesets(&gitalypb.WatchChangesetsRequest{Repository: env.repo}, stream)
			}()

			expectSeq(t, stream, 1, 10*time.Second, "backlog")

			// Written by this node: the write path notifies the watchers, and the
			// entry arrives within a second.
			commitRef(t, env.node, env.id, "refs/heads/b", c2, "agent-a")
			corestore.NotifyChangelog(env.id)
			expectSeq(t, stream, 2, time.Second, "live entry of this node")

			// Written by another node without any notification: it is seen by polling.
			commitRef(t, env.peer, env.id, "refs/heads/c", c3, "agent-b")
			expectSeq(t, stream, 3, 10*time.Second, "entry of another node")

			cancel()
			select {
			case err := <-done:
				require.NoError(t, err, "a cancelled watch ends without an error")
			case <-time.After(10 * time.Second):
				t.Fatal("the handler did not return after the context was cancelled")
			}
		})
	}
}

func TestAuthoritativeWatchChangesetsAfterSequence(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			env := setupAuthoritative(t, dialect)

			c1 := writeCommit(t, env, "c1")
			c2 := writeCommit(t, env, "c2")
			c3 := writeCommit(t, env, "c3")

			commitRef(t, env.node, env.id, "refs/heads/a", c1, "agent-a")
			commitRef(t, env.node, env.id, "refs/heads/b", c2, "agent-a")

			watchCtx, cancel := context.WithCancel(testhelper.Context(t))
			defer cancel()

			stream := &fakeWatchStream{ctx: watchCtx, sent: make(chan *gitalypb.Changeset, 10)}
			done := make(chan error, 1)
			go func() {
				done <- env.direct.WatchChangesets(&gitalypb.WatchChangesetsRequest{Repository: env.repo, AfterSequence: 1}, stream)
			}()

			expectSeq(t, stream, 2, 10*time.Second, "backlog after the cursor")

			commitRef(t, env.peer, env.id, "refs/heads/c", c3, "agent-b")
			expectSeq(t, stream, 3, 10*time.Second, "entry of another node")

			cancel()
			require.NoError(t, <-done)
		})
	}
}

// TestAuthoritativeWritesAreLogged writes through the RPCs, so the changelog
// entries come from the storage core's write path in the reference-transaction
// hook. It skips itself while the write path (WP54) or the catch-up (WP55) are
// not implemented in this tree.
func TestAuthoritativeWritesAreLogged(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			env := setupAuthoritative(t, dialect)

			c1 := gittest.WriteCommit(t, env.cfg, env.repoPath, gittest.WithMessage("c1"))
			c2 := gittest.WriteCommit(t, env.cfg, env.repoPath, gittest.WithParents(c1), gittest.WithMessage("c2"))
			c3 := gittest.WriteCommit(t, env.cfg, env.repoPath, gittest.WithParents(c2), gittest.WithMessage("c3"))

			_, err := env.operations.UserCreateBranch(env.ctx, &gitalypb.UserCreateBranchRequest{
				Repository: env.repo,
				BranchName: []byte("feature"),
				StartPoint: []byte(c1),
				User:       gittest.TestUser,
			})
			if err != nil && strings.Contains(err.Error(), "not implemented (WP5") {
				t.Skipf("the storage core write path is not implemented in this tree: %v", err)
			}
			require.NoError(t, err)

			for _, commit := range []git.ObjectID{c2, c3} {
				_, err := env.operations.UserLand(env.ctx, &gitalypb.UserLandRequest{
					Repository: env.repo,
					User:       gittest.TestUser,
					Branch:     []byte("feature"),
					CommitId:   commit.String(),
				})
				require.NoError(t, err)
			}

			changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: env.repo})
			require.NoError(t, err)
			require.Len(t, changesets, 3)

			wantNew := []git.ObjectID{c1, c2, c3}
			for i, c := range changesets {
				require.Equal(t, uint64(i+1), c.GetSequence(), "the entries are dense")
				require.Equal(t, []byte("refs/heads/feature"), c.GetReference())
				require.Equal(t, wantNew[i].String(), c.GetNewOid())
				require.Equal(t, gittest.GlID, c.GetActor())
			}
			require.Equal(t, string(gittest.DefaultObjectHash.ZeroOID), changesets[0].GetOldOid())
			require.Equal(t, c1.String(), changesets[1].GetOldOid())

			// The file changelog is not written in authoritative mode.
			_, err = os.Stat(changelog.Path(env.repoPath))
			require.True(t, os.IsNotExist(err), "authoritative mode writes no file changelog: %v", err)
		})
	}
}
