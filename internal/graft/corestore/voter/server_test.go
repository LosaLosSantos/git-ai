package voter_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestAuthoritativeServer runs the write path inside a Gitaly server in
// authoritative mode: the hook manager calls the voter for the reference
// transactions of the RPCs.
func TestAuthoritativeServer(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		ctx := testhelper.Context(t)
		cfg := testcfg.Build(t)
		cfg.Graft.Standalone = true
		testcfg.BuildGitalyHooks(t, cfg)

		core := coretest.Open(t, coretest.Options{Mode: corestore.ModeAuthoritative, Dialect: dialect})
		addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
			testserver.WithGraftStorage(core),
			testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))),
		)
		cfg.SocketPath = addr

		conn, err := client.New(ctx, addr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		operations := gitalypb.NewOperationServiceClient(conn)

		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		repo := localrepo.NewTestRepo(t, cfg, repoProto)
		backend, err := repo.ReferenceBackend(ctx)
		require.NoError(t, err)
		require.Equal(t, git.ReferenceBackendReftables.Name, backend.Name, "authoritative mode serves reftable repositories")
		repoID := trackRepository(t, core, repo, repoPath)

		dbRefs := func(t *testing.T) map[string]corestore.Target {
			t.Helper()
			refs, _, err := core.Meta.Refs(ctx, repoID)
			require.NoError(t, err)
			return refs
		}

		first := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
			gittest.TreeEntry{Path: "base", Mode: "100644", Content: "base\n"},
		))

		t.Run("UserCreateBranch commits the branch to the database", func(t *testing.T) {
			_, err := operations.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
				Repository: repoProto,
				BranchName: []byte("main"),
				StartPoint: []byte(first.String()),
				User:       gittest.TestUser,
			})
			require.NoError(t, err)

			require.Equal(t, corestore.Target(first.String()), dbRefs(t)["refs/heads/main"])

			repository, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repoProto))
			require.NoError(t, err)
			changesets, err := core.Meta.ListChangesets(ctx, repoID, repository.HeadSeq-1, repository.HeadSeq)
			require.NoError(t, err)
			require.Len(t, changesets, 1)
			require.NotEmpty(t, changesets[0].PackKey, "the new commit is uploaded")

			entries, err := core.Meta.ScanChangelog(ctx, repoID, 0, 0)
			require.NoError(t, err)
			require.NotEmpty(t, entries)
			last := entries[len(entries)-1]
			require.Equal(t, "refs/heads/main", last.Reference)
			require.Equal(t, first.String(), last.NewOID)
		})

		t.Run("transactions run by git report symbolic and forced updates", func(t *testing.T) {
			_, err := operations.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
				Repository: repoProto,
				BranchName: []byte("feature"),
				StartPoint: []byte(first.String()),
				User:       gittest.TestUser,
			})
			require.NoError(t, err)
			require.Equal(t, corestore.Target(first.String()), dbRefs(t)["refs/heads/feature"])

			// WriteRef of HEAD runs `git symbolic-ref`, whose hook reports
			// "<zero> ref:refs/heads/feature HEAD".
			_, err = gitalypb.NewRepositoryServiceClient(conn).WriteRef(ctx, &gitalypb.WriteRefRequest{
				Repository: repoProto,
				Ref:        []byte("HEAD"),
				Revision:   []byte("refs/heads/feature"),
			})
			require.NoError(t, err)
			require.Equal(t, corestore.SymbolicTarget("refs/heads/feature"), dbRefs(t)["HEAD"])

			_, err = gitalypb.NewRepositoryServiceClient(conn).WriteRef(ctx, &gitalypb.WriteRefRequest{
				Repository: repoProto,
				Ref:        []byte("HEAD"),
				Revision:   []byte("refs/heads/main"),
			})
			require.NoError(t, err)
			require.Equal(t, corestore.SymbolicTarget("refs/heads/main"), dbRefs(t)["HEAD"])

			// DeleteRefs deletes without an expected old value: a forced deletion.
			_, err = gitalypb.NewRefServiceClient(conn).DeleteRefs(ctx, &gitalypb.DeleteRefsRequest{
				Repository: repoProto,
				Refs:       [][]byte{[]byte("refs/heads/feature")},
			})
			require.NoError(t, err)
			require.NotContains(t, dbRefs(t), "refs/heads/feature")
			require.Equal(t, gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/main"), git.ObjectID(dbRefs(t)["refs/heads/main"]), "the other references are untouched")
		})

		t.Run("a database conflict makes UserLand retry", func(t *testing.T) {
			// Another node moves main in the database in the middle of our write: after
			// the gate caught the local repository up and after our voter uploaded its
			// pack, but before our voter commits. This is the real race of two nodes
			// landing on one branch; committing the other node's changeset before the
			// RPC would let the gate's catch-up hide it.
			theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(first), gittest.WithTreeEntries(
				gittest.TreeEntry{Path: "base", Mode: "100644", Content: "base\n"},
				gittest.TreeEntry{Path: "theirs", Mode: "100644", Content: "theirs\n"},
			))
			pack, err := gitops.BuildPack(ctx, repo, []git.ObjectID{theirs}, testhelper.TempDir(t))
			require.NoError(t, err)
			defer func() { _ = pack.Remove() }()
			require.False(t, pack.Empty())

			packFile, err := pack.Open()
			require.NoError(t, err)
			key := corestore.NewPackKey(repoID, pack.SHA256)
			require.NoError(t, core.Blobs.Put(ctx, key, packFile))
			require.NoError(t, packFile.Close())

			// The other node's commit happens once, at our first upload.
			var raced atomic.Bool
			voter.FaultHook = func(point string) error {
				if point != "after_upload" || !raced.CompareAndSwap(false, true) {
					return nil
				}
				_, err := core.Meta.CommitChangeset(ctx, corestore.ChangesetInput{
					RepoID:    repoID,
					Updates:   []corestore.RefUpdate{{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(first.String()), New: corestore.Target(theirs.String())}},
					Operation: "land",
					Actor:     "other-node",
					NodeID:    "node-b",
					PackKey:   key, PackDeadline: time.Now().Add(time.Hour),
					PackBytes: pack.Size,
				})
				return err
			}
			t.Cleanup(func() { voter.FaultHook = nil })

			ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(first), gittest.WithTreeEntries(
				gittest.TreeEntry{Path: "base", Mode: "100644", Content: "base\n"},
				gittest.TreeEntry{Path: "ours", Mode: "100644", Content: "ours\n"},
			))

			// The land path checks the GPG signing feature flag; tests must set every
			// flag they touch (as internal/graft/e2e does).
			landCtx := featureflag.OutgoingCtxWithFeatureFlag(ctx, featureflag.GPGSigning, false)
			resp, err := operations.UserLand(landCtx, &gitalypb.UserLandRequest{
				Repository: repoProto,
				User:       gittest.TestUser,
				Branch:     []byte("main"),
				CommitId:   ours.String(),
			})
			// The first attempt loses against the database: the land path classifies the
			// voter's error as a lost race, the syncer catches up, and the retry lands
			// onto the database tip.
			require.NoError(t, err)
			require.True(t, raced.Load(), "the other node committed during our first attempt")

			require.GreaterOrEqual(t, resp.GetAttempts(), int32(2), "the first attempt conflicts in the database")
			require.True(t, resp.GetRewritten())
			landed := resp.GetLandedCommitId()
			require.Equal(t, corestore.Target(landed), dbRefs(t)["refs/heads/main"])
			require.Equal(t, landed, gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/main").String())
			require.Equal(t, theirs.String(), gittest.ResolveRevision(t, cfg, repoPath, landed+"^").String(), "the change is rebased onto the database tip")
		})
	})
}

// TestAuthoritativeConcurrentCommitFiles runs 10 concurrent UserCommitFiles
// onto 10 different branches of one repository in authoritative mode. Every
// reference transaction of a reftable repository takes the repository-wide
// lock, so the writes contend for it: none may fail with "reference is already
// locked" (doc/graft/PLAN6.md, LOCK AND HISTORY RULE: the pack is uploaded in
// the Preparing phase, outside the lock, and git waits for the lock like for
// packed-refs). The database ends with all 10 branches.
func TestAuthoritativeConcurrentCommitFiles(t *testing.T) {
	const writers = 10

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		ctx := testhelper.Context(t)
		cfg := testcfg.Build(t)
		cfg.Graft.Standalone = true
		testcfg.BuildGitalyHooks(t, cfg)

		core := coretest.Open(t, coretest.Options{Mode: corestore.ModeAuthoritative, Dialect: dialect})
		addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
			testserver.WithGraftStorage(core),
			testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))),
		)
		cfg.SocketPath = addr

		conn, err := client.New(ctx, addr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		operations := gitalypb.NewOperationServiceClient(conn)

		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		repo := localrepo.NewTestRepo(t, cfg, repoProto)
		repoID := trackRepository(t, core, repo, repoPath)

		base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
			gittest.TreeEntry{Path: "base", Mode: "100644", Content: "base\n"},
		))
		_, err = operations.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
			Repository: repoProto,
			BranchName: []byte("main"),
			StartPoint: []byte(base.String()),
			User:       gittest.TestUser,
		})
		require.NoError(t, err)

		// The commit path checks the GPG signing feature flag; tests must set every
		// flag they touch.
		commitCtx := featureflag.OutgoingCtxWithFeatureFlag(ctx, featureflag.GPGSigning, false)
		commitFile := func(i int) (string, error) {
			stream, err := operations.UserCommitFiles(commitCtx)
			if err != nil {
				return "", err
			}
			requests := []*gitalypb.UserCommitFilesRequest{
				{UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{
					Header: &gitalypb.UserCommitFilesRequestHeader{
						Repository:      repoProto,
						User:            gittest.TestUser,
						BranchName:      []byte(fmt.Sprintf("writer-%d", i)),
						StartBranchName: []byte("main"),
						CommitMessage:   []byte(fmt.Sprintf("writer %d", i)),
					},
				}},
				{UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{
					Action: &gitalypb.UserCommitFilesAction{
						UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Header{
							Header: &gitalypb.UserCommitFilesActionHeader{
								Action:   gitalypb.UserCommitFilesActionHeader_CREATE,
								FilePath: []byte(fmt.Sprintf("file-%d", i)),
							},
						},
					},
				}},
				{UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{
					Action: &gitalypb.UserCommitFilesAction{
						UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Content{
							Content: []byte(fmt.Sprintf("content %d\n", i)),
						},
					},
				}},
			}
			for _, request := range requests {
				if err := stream.Send(request); err != nil {
					return "", err
				}
			}
			resp, err := stream.CloseAndRecv()
			if err != nil {
				return "", err
			}
			return resp.GetBranchUpdate().GetCommitId(), nil
		}

		commits := make([]string, writers)
		errs := make([]error, writers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				commits[i], errs[i] = commitFile(i)
			}(i)
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			require.NoError(t, err, "writer %d", i)
		}

		refs, _, err := core.Meta.Refs(ctx, repoID)
		require.NoError(t, err)
		for i, commit := range commits {
			branch := fmt.Sprintf("refs/heads/writer-%d", i)
			require.NotEmpty(t, commit, "writer %d", i)
			require.Equal(t, corestore.Target(commit), refs[branch], "the database has %s", branch)
			require.Equal(t, commit, gittest.ResolveRevision(t, cfg, repoPath, branch).String(), "the local repository has %s", branch)
		}
	})
}

// TestShadowServerFilesBackend deletes packed references of a files-backend
// repository through a Gitaly server in shadow mode. git deletes a packed
// reference in two transactions, the packed-refs half and then the loose half;
// the database must follow the deletion and no divergence is reported.
func TestShadowServerFilesBackend(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		ctx := testhelper.Context(t)
		cfg := testcfg.Build(t)
		testcfg.BuildGitalyHooks(t, cfg)

		logger := testhelper.NewLogger(t)
		logs := testhelper.AddLoggerHook(logger)

		core := coretest.Open(t, coretest.Options{Mode: corestore.ModeShadow, Dialect: dialect})
		addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
			testserver.WithGraftStorage(core),
			testserver.WithLogger(logger),
		)
		cfg.SocketPath = addr

		conn, err := client.New(ctx, addr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		refs := gitalypb.NewRefServiceClient(conn)

		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
		useFilesBackend(t, cfg, repoPath)
		repo := localrepo.NewTestRepo(t, cfg, repoProto)
		repoID := trackRepository(t, core, repo, repoPath)

		commit := gittest.WriteCommit(t, cfg, repoPath)
		names := []string{"refs/heads/forced", "refs/heads/checked", "refs/heads/kept"}
		updates := make([]corestore.RefUpdate, 0, len(names))
		for _, name := range names {
			gittest.WriteRef(t, cfg, repoPath, git.ReferenceName(name), commit)
			updates = append(updates, corestore.RefUpdate{Name: name, Expect: corestore.ExpectAny, New: corestore.Target(commit.String())})
		}
		// The references are packed, so that deleting one runs the packed-refs
		// transaction before the loose one.
		gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")
		_, err = core.Meta.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID: repoID, Updates: updates, Operation: "seed", Actor: "test", NodeID: "node-a",
		})
		require.NoError(t, err)

		dbRefs := func(t *testing.T) map[string]corestore.Target {
			t.Helper()
			current, _, err := core.Meta.Refs(ctx, repoID)
			require.NoError(t, err)
			return current
		}

		requireNoDivergence := func(t *testing.T) {
			t.Helper()
			for _, entry := range logs.AllEntries() {
				require.NotContains(t, entry.Message, "diverges from the database")
				require.NotContains(t, entry.Message, "shadow write failed")
			}
		}

		t.Run("DeleteRefs without old values", func(t *testing.T) {
			_, err := refs.DeleteRefs(ctx, &gitalypb.DeleteRefsRequest{
				Repository: repoProto,
				Refs:       [][]byte{[]byte("refs/heads/forced")},
			})
			require.NoError(t, err)

			require.NotContains(t, dbRefs(t), "refs/heads/forced")
			require.Equal(t, corestore.Target(commit.String()), dbRefs(t)["refs/heads/kept"])
			requireNoDivergence(t)
		})

		t.Run("UpdateReferences deletion with an old value", func(t *testing.T) {
			stream, err := refs.UpdateReferences(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UpdateReferencesRequest{
				Repository: repoProto,
				Updates: []*gitalypb.UpdateReferencesRequest_Update{{
					Reference:   []byte("refs/heads/checked"),
					OldObjectId: []byte(commit.String()),
					NewObjectId: []byte(gittest.DefaultObjectHash.ZeroOID.String()),
				}},
			}))
			_, err = stream.CloseAndRecv()
			require.NoError(t, err)

			require.NotContains(t, dbRefs(t), "refs/heads/checked")
			require.Equal(t, corestore.Target(commit.String()), dbRefs(t)["refs/heads/kept"])
			requireNoDivergence(t)
		})

		record, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repoProto))
		require.NoError(t, err)
		changesets, err := core.Meta.ListChangesets(ctx, repoID, 0, record.HeadSeq)
		require.NoError(t, err)
		// create_repository, the seed and one changeset per deletion: the second
		// transaction of each deletion is a no-op.
		require.Len(t, changesets, 4)
		for _, changeset := range changesets {
			require.False(t, changeset.Forced, "changeset %d is not a divergence", changeset.Seq)
		}
	})
}

// trackRepository returns the database ID of a repository created through the
// server. The authoritative gate (WP56) registers a created repository; when
// it did not, the repository is registered here as the gate would do it.
func trackRepository(t *testing.T, core *corestore.Core, repo *localrepo.Repo, repoPath string) int64 {
	t.Helper()

	ctx := testhelper.Context(t)
	key := corestore.KeyOf(repo)

	record, err := core.Meta.GetRepository(ctx, key)
	if err == nil {
		return record.ID
	}
	require.True(t, errors.Is(err, corestore.ErrRepositoryNotFound), "error %v", err)

	hash, err := repo.ObjectHash(ctx)
	require.NoError(t, err)
	refs, err := gitops.ReadRefs(ctx, repo)
	require.NoError(t, err)
	head, ok := refs["HEAD"]
	require.True(t, ok, "a new repository has HEAD")

	record, _, err = core.Meta.RegisterRepository(ctx, key, hash.Format, corestore.InitialState{
		Changeset: &corestore.ChangesetInput{
			Updates:   []corestore.RefUpdate{{Name: "HEAD", Expect: corestore.ExpectValue, New: head}},
			Operation: "create_repository",
			Actor:     "test",
			NodeID:    core.NodeID,
		},
	})
	require.NoError(t, err)
	require.NoError(t, gitops.WriteWatermark(repoPath, corestore.Watermark{RepoID: record.ID, Seq: 1}))

	return record.ID
}
