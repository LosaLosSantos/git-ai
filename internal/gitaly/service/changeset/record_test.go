package changeset

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/graft/identity"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// recordingEnv starts the services with cfg and returns a repository with two
// commits, c1 and c2, that no branch points to yet.
func recordingEnv(t *testing.T, cfg config.Cfg) (testEnv, *gitalypb.Repository, string, string, string) {
	t.Helper()

	ctx := testhelper.Context(t)
	env := setupChangesetService(t, ctx, cfg)
	repo, repoPath := gittest.CreateRepository(t, ctx, env.cfg)

	c1 := gittest.WriteCommit(t, env.cfg, repoPath)
	c2 := gittest.WriteCommit(t, env.cfg, repoPath, gittest.WithParents(c1))

	return env, repo, repoPath, c1.String(), c2.String()
}

func TestRecordedThroughUserUpdateBranch(t *testing.T) {
	t.Parallel()

	env, repo, _, c1, c2 := recordingEnv(t, testcfg.Build(t))

	_, err := env.operations.UserUpdateBranch(env.ctx, &gitalypb.UserUpdateBranchRequest{
		Repository: repo,
		BranchName: []byte("branch"),
		Oldrev:     []byte(gittest.DefaultObjectHash.ZeroOID),
		Newrev:     []byte(c1),
		User:       gittest.TestUser,
	})
	require.NoError(t, err)

	_, err = env.operations.UserUpdateBranch(env.ctx, &gitalypb.UserUpdateBranchRequest{
		Repository: repo,
		BranchName: []byte("branch"),
		Oldrev:     []byte(c1),
		Newrev:     []byte(c2),
		User:       gittest.TestUser,
	})
	require.NoError(t, err)

	changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: repo})
	require.NoError(t, err)
	require.Len(t, changesets, 2)

	require.Equal(t, uint64(1), changesets[0].GetSequence())
	require.Equal(t, []byte("refs/heads/branch"), changesets[0].GetReference())
	require.Equal(t, string(gittest.DefaultObjectHash.ZeroOID), changesets[0].GetOldOid())
	require.Equal(t, c1, changesets[0].GetNewOid())
	require.Equal(t, "update_branch", changesets[0].GetOperation())
	require.Equal(t, gittest.GlID, changesets[0].GetActor(), "the actor is the request's user ID")
	require.False(t, changesets[0].GetRewritten())

	require.Equal(t, uint64(2), changesets[1].GetSequence())
	require.Equal(t, c1, changesets[1].GetOldOid())
	require.Equal(t, c2, changesets[1].GetNewOid())
}

func TestRecordedReferenceNamesAreKeptVerbatim(t *testing.T) {
	t.Parallel()

	// Git allows any non-ASCII byte in reference names, including Unicode
	// whitespace, which must not be taken for the separator of the hook's input,
	// and bytes that are not valid UTF-8, which JSON strings cannot hold.
	for _, tc := range []struct {
		desc   string
		branch string
	}{
		{desc: "ASCII", branch: "plain"},
		{desc: "no-break space", branch: "a\u00a0b"},
		{desc: "next line", branch: "x\u0085y"},
		{desc: "en quad", branch: "p\u2000q"},
		{desc: "ideographic space at the end", branch: "end\u3000"},
		{desc: "invalid UTF-8", branch: "caf\xe9"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			env, repo, repoPath, c1, _ := recordingEnv(t, testcfg.Build(t))

			_, err := env.operations.UserUpdateBranch(env.ctx, &gitalypb.UserUpdateBranchRequest{
				Repository: repo,
				BranchName: []byte(tc.branch),
				Oldrev:     []byte(gittest.DefaultObjectHash.ZeroOID),
				Newrev:     []byte(c1),
				User:       gittest.TestUser,
			})
			require.NoError(t, err)

			reference := "refs/heads/" + tc.branch
			require.Equal(t, c1, gittest.ResolveRevision(t, env.cfg, repoPath, reference).String())

			changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: repo})
			require.NoError(t, err)
			require.Len(t, changesets, 1)
			require.Equal(t, []byte(reference), changesets[0].GetReference())
			require.Equal(t, string(gittest.DefaultObjectHash.ZeroOID), changesets[0].GetOldOid())
			require.Equal(t, c1, changesets[0].GetNewOid())
		})
	}
}

func TestRecordedThroughUserFFBranch(t *testing.T) {
	t.Parallel()

	env, repo, _, c1, c2 := recordingEnv(t, testcfg.Build(t))

	_, err := env.operations.UserUpdateBranch(env.ctx, &gitalypb.UserUpdateBranchRequest{
		Repository: repo,
		BranchName: []byte("branch"),
		Oldrev:     []byte(gittest.DefaultObjectHash.ZeroOID),
		Newrev:     []byte(c1),
		User:       gittest.TestUser,
	})
	require.NoError(t, err)

	_, err = env.operations.UserFFBranch(env.ctx, &gitalypb.UserFFBranchRequest{
		Repository: repo,
		User:       gittest.TestUser,
		Branch:     []byte("branch"),
		CommitId:   c2,
	})
	require.NoError(t, err)

	changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: repo})
	require.NoError(t, err)
	require.Len(t, changesets, 2)
	require.Equal(t, "ff_branch", changesets[1].GetOperation())
	require.Equal(t, c1, changesets[1].GetOldOid())
	require.Equal(t, c2, changesets[1].GetNewOid())
}

func TestFailedUpdateIsNotRecorded(t *testing.T) {
	t.Parallel()

	env, repo, _, c1, c2 := recordingEnv(t, testcfg.Build(t))

	_, err := env.operations.UserUpdateBranch(env.ctx, &gitalypb.UserUpdateBranchRequest{
		Repository: repo,
		BranchName: []byte("branch"),
		Oldrev:     []byte(gittest.DefaultObjectHash.ZeroOID),
		Newrev:     []byte(c1),
		User:       gittest.TestUser,
	})
	require.NoError(t, err)

	// The expected old value is wrong, so the reference is not updated.
	_, err = env.operations.UserUpdateBranch(env.ctx, &gitalypb.UserUpdateBranchRequest{
		Repository: repo,
		BranchName: []byte("branch"),
		Oldrev:     []byte(c2),
		Newrev:     []byte(c2),
		User:       gittest.TestUser,
	})
	require.Error(t, err)

	changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: repo})
	require.NoError(t, err)
	require.Len(t, changesets, 1, "only the successful update must be recorded")
}

func TestRecordingDisabled(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	cfg.Graft.Changelog.Disabled = true
	env, repo, _, c1, _ := recordingEnv(t, cfg)

	_, err := env.operations.UserUpdateBranch(env.ctx, &gitalypb.UserUpdateBranchRequest{
		Repository: repo,
		BranchName: []byte("branch"),
		Oldrev:     []byte(gittest.DefaultObjectHash.ZeroOID),
		Newrev:     []byte(c1),
		User:       gittest.TestUser,
	})
	require.NoError(t, err)

	changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: repo})
	require.NoError(t, err)
	require.Empty(t, changesets)
}

func TestRecordingFailureDoesNotFailUpdate(t *testing.T) {
	t.Parallel()

	env, repo, repoPath, c1, _ := recordingEnv(t, testcfg.Build(t))

	// A regular file where the changelog directory must go makes recording fail.
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "graft"), []byte("in the way"), 0o644))

	_, err := env.operations.UserUpdateBranch(env.ctx, &gitalypb.UserUpdateBranchRequest{
		Repository: repo,
		BranchName: []byte("branch"),
		Oldrev:     []byte(gittest.DefaultObjectHash.ZeroOID),
		Newrev:     []byte(c1),
		User:       gittest.TestUser,
	})
	require.NoError(t, err, "a failure to record must not fail the reference update")

	ref := strings.TrimSpace(string(gittest.Exec(t, env.cfg, "-C", repoPath, "rev-parse", "refs/heads/branch")))
	require.Equal(t, c1, ref)
}

// callCommittedReferenceTransaction calls HookService.ReferenceTransactionHook
// the way Git does for a committed reference transaction: the hooks payload is
// in the environment and the updates are on stdin.
func callCommittedReferenceTransaction(t *testing.T, env testEnv, repo *gitalypb.Repository, hooksPayload, updates string) {
	t.Helper()

	stream, err := env.hooks.ReferenceTransactionHook(env.ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&gitalypb.ReferenceTransactionHookRequest{
		Repository:           repo,
		State:                gitalypb.ReferenceTransactionHookRequest_COMMITTED,
		EnvironmentVariables: []string{hooksPayload},
	}))
	require.NoError(t, stream.Send(&gitalypb.ReferenceTransactionHookRequest{Stdin: []byte(updates)}))
	require.NoError(t, stream.CloseSend())

	_, err = stream.Recv()
	require.NoError(t, err)
}

func TestRecordedThroughReferenceTransactionHook(t *testing.T) {
	t.Parallel()

	env, repo, _, c1, _ := recordingEnv(t, testcfg.Build(t))

	// The metadata of the request's context travels in the hooks payload. The
	// authenticated identity is the actor, and the operation and rewrite flag
	// are the ones set on the context. The payload is built in process here, as
	// it is for a push, and only the hook call goes through gRPC.
	ctx := changelog.WithRewritten(changelog.WithOperation(env.ctx, "land"))
	ctx = identity.NewContext(ctx, identity.Identity{Name: "agent-a", Source: identity.SourceAgent})
	payload, err := gitcmd.NewHooksPayload(ctx, env.cfg, repo, gittest.DefaultObjectHash, nil, &gitcmd.UserDetails{UserID: "user-1"}, gitcmd.ReferenceTransactionHook, nil).Env()
	require.NoError(t, err)

	zero := string(gittest.DefaultObjectHash.ZeroOID)
	callCommittedReferenceTransaction(t, env, repo, payload, zero+" "+c1+" refs/heads/hooked\n")

	// Without an identity or an operation on the context, the user ID is the
	// actor and the operation is "unknown".
	plainPayload, err := gitcmd.NewHooksPayload(env.ctx, env.cfg, repo, gittest.DefaultObjectHash, nil, &gitcmd.UserDetails{UserID: "user-1"}, gitcmd.ReferenceTransactionHook, nil).Env()
	require.NoError(t, err)

	callCommittedReferenceTransaction(t, env, repo, plainPayload, c1+" "+c1+" refs/heads/unchanged\n"+zero+" "+c1+" refs/heads/plain\n")

	changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: repo})
	require.NoError(t, err)
	require.Len(t, changesets, 2, "the unchanged reference is not recorded")

	require.Equal(t, []byte("refs/heads/hooked"), changesets[0].GetReference())
	require.Equal(t, zero, changesets[0].GetOldOid())
	require.Equal(t, c1, changesets[0].GetNewOid())
	require.Equal(t, "land", changesets[0].GetOperation())
	require.Equal(t, "agent-a", changesets[0].GetActor())
	require.True(t, changesets[0].GetRewritten())

	require.Equal(t, []byte("refs/heads/plain"), changesets[1].GetReference())
	require.Equal(t, "unknown", changesets[1].GetOperation())
	require.Equal(t, "user-1", changesets[1].GetActor(), "the actor falls back to the user ID")
	require.False(t, changesets[1].GetRewritten())
}

func TestReferenceTransactionHookDoesNotCreateRepository(t *testing.T) {
	t.Parallel()

	env, repo, _, c1, _ := recordingEnv(t, testcfg.Build(t))

	// The payload names a repository that does not exist. Recording must not
	// create its directory, or the path would be taken for later creations.
	ghost := &gitalypb.Repository{StorageName: repo.GetStorageName(), RelativePath: "ghost/created-by-hook.git"}
	payload, err := gitcmd.NewHooksPayload(env.ctx, env.cfg, ghost, gittest.DefaultObjectHash, nil, nil, gitcmd.ReferenceTransactionHook, nil).Env()
	require.NoError(t, err)

	callCommittedReferenceTransaction(t, env, repo, payload, string(gittest.DefaultObjectHash.ZeroOID)+" "+c1+" refs/heads/x\n")

	_, err = os.Stat(filepath.Join(env.cfg.Storages[0].Path, "ghost", "created-by-hook.git"))
	require.ErrorIs(t, err, fs.ErrNotExist, "recording must not create a repository directory")
}

// updateBranch sets the branch from oldrev to newrev with UserUpdateBranch.
func updateBranch(t *testing.T, env testEnv, repo *gitalypb.Repository, branch, oldrev, newrev string) {
	t.Helper()

	_, err := env.operations.UserUpdateBranch(env.ctx, &gitalypb.UserUpdateBranchRequest{
		Repository: repo,
		BranchName: []byte(branch),
		Oldrev:     []byte(oldrev),
		Newrev:     []byte(newrev),
		User:       gittest.TestUser,
	})
	require.NoError(t, err)
}

// deleteRefs deletes the references with DeleteRefs, which does not pass an
// expected old value.
func deleteRefs(t *testing.T, env testEnv, repo *gitalypb.Repository, refs ...string) {
	t.Helper()

	var names [][]byte
	for _, ref := range refs {
		names = append(names, []byte(ref))
	}

	_, err := env.refs.DeleteRefs(env.ctx, &gitalypb.DeleteRefsRequest{Repository: repo, Refs: names})
	require.NoError(t, err)
}

// deletionCounts returns how often each reference was recorded as deleted
// without an expected old value.
func deletionCounts(t *testing.T, env testEnv, repo *gitalypb.Repository) map[string]int {
	t.Helper()

	changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: repo})
	require.NoError(t, err)

	zero := string(gittest.DefaultObjectHash.ZeroOID)
	counts := map[string]int{}
	for _, c := range changesets {
		if c.GetOldOid() == zero && c.GetNewOid() == zero {
			counts[string(c.GetReference())]++
		}
	}

	return counts
}

func TestRecordedThroughDeleteRefs(t *testing.T) {
	t.Parallel()

	env, repo, _, c1, _ := recordingEnv(t, testcfg.Build(t))
	zero := string(gittest.DefaultObjectHash.ZeroOID)

	updateBranch(t, env, repo, "branch", zero, c1)
	deleteRefs(t, env, repo, "refs/heads/branch")

	changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: repo})
	require.NoError(t, err)
	require.Len(t, changesets, 2, "the deletion must be recorded, not only the creation")

	deletion := changesets[1]
	require.Equal(t, uint64(2), deletion.GetSequence())
	require.Equal(t, []byte("refs/heads/branch"), deletion.GetReference())
	require.Equal(t, zero, deletion.GetOldOid(), "the previous value is not known to the hook")
	require.Equal(t, zero, deletion.GetNewOid())
	require.Equal(t, "delete_refs", deletion.GetOperation())
}

func TestRecordedDeleteRefsRecordsEachDeletionOnce(t *testing.T) {
	t.Parallel()

	env, repo, repoPath, c1, c2 := recordingEnv(t, testcfg.Build(t))
	zero := string(gittest.DefaultObjectHash.ZeroOID)

	updateBranch(t, env, repo, "packed", zero, c1)
	updateBranch(t, env, repo, "both", zero, c1)
	gittest.Exec(t, env.cfg, "-C", repoPath, "pack-refs", "--all")

	// Packed-refs only, and in both backends: git runs the hook for the
	// packed-refs part and for the loose part of each of these deletions.
	updateBranch(t, env, repo, "both", c1, c2)
	updateBranch(t, env, repo, "fresh", zero, c1)

	deleteRefs(t, env, repo, "refs/heads/packed", "refs/heads/both", "refs/heads/fresh")

	require.Equal(t, map[string]int{
		"refs/heads/packed": 1,
		"refs/heads/both":   1,
		"refs/heads/fresh":  1,
	}, deletionCounts(t, env, repo), "each deletion must be recorded once")
}

func TestRecordedDeleteRefsAfterRecreation(t *testing.T) {
	t.Parallel()

	env, repo, _, c1, c2 := recordingEnv(t, testcfg.Build(t))
	zero := string(gittest.DefaultObjectHash.ZeroOID)

	updateBranch(t, env, repo, "branch", zero, c1)
	deleteRefs(t, env, repo, "refs/heads/branch")
	updateBranch(t, env, repo, "branch", zero, c2)
	deleteRefs(t, env, repo, "refs/heads/branch")

	require.Equal(t, map[string]int{"refs/heads/branch": 2}, deletionCounts(t, env, repo),
		"a deletion after the branch was recreated must be recorded")
}

// recordedDeletions returns the old OIDs recorded for the deletions of each
// reference, whether or not the deletion had an expected old value.
func recordedDeletions(t *testing.T, env testEnv, repo *gitalypb.Repository) map[string][]string {
	t.Helper()

	changesets, _, err := listAll(t, env.ctx, env.changesets, &gitalypb.ListChangesetsRequest{Repository: repo, Limit: 10000})
	require.NoError(t, err)

	zero := string(gittest.DefaultObjectHash.ZeroOID)
	deletions := map[string][]string{}
	for _, c := range changesets {
		if c.GetNewOid() == zero {
			deletions[string(c.GetReference())] = append(deletions[string(c.GetReference())], c.GetOldOid())
		}
	}

	return deletions
}

func TestRecordedWriteRefDeletionOfPackedRefs(t *testing.T) {
	t.Parallel()

	env, repo, repoPath, c1, c2 := recordingEnv(t, testcfg.Build(t))
	zero := string(gittest.DefaultObjectHash.ZeroOID)

	// Created outside of Gitaly, so only the deletions are recorded.
	gittest.Exec(t, env.cfg, "-C", repoPath, "update-ref", "refs/heads/packed", c1)
	gittest.Exec(t, env.cfg, "-C", repoPath, "update-ref", "refs/heads/both", c1)
	gittest.Exec(t, env.cfg, "-C", repoPath, "pack-refs", "--all")
	gittest.Exec(t, env.cfg, "-C", repoPath, "update-ref", "refs/heads/both", c2)
	gittest.Exec(t, env.cfg, "-C", repoPath, "update-ref", "refs/heads/loose", c1)

	// WriteRef deletes with an expected old value and lets git run the hook.
	// For a packed reference, git runs it for the packed-refs cleanup, which
	// reports a force deletion, and then for the loose deletion with the old
	// value. Only the loose deletion must be recorded.
	for ref, old := range map[string]string{
		"refs/heads/packed": c1,
		"refs/heads/both":   c2,
		"refs/heads/loose":  c1,
	} {
		_, err := env.repository.WriteRef(env.ctx, &gitalypb.WriteRefRequest{
			Repository:  repo,
			Ref:         []byte(ref),
			Revision:    []byte(zero),
			OldRevision: []byte(old),
		})
		require.NoError(t, err)
	}

	require.Equal(t, map[string][]string{
		"refs/heads/packed": {c1},
		"refs/heads/both":   {c2},
		"refs/heads/loose":  {c1},
	}, recordedDeletions(t, env, repo), "each deletion must be recorded once, with its old value")
}

func TestRecordedDeleteRefsOfManyPackedRefs(t *testing.T) {
	t.Parallel()

	env, repo, repoPath, c1, _ := recordingEnv(t, testcfg.Build(t))

	// More references than any bound on remembered deletions, all packed, so
	// git runs the hook for the packed-refs cleanup of all of them first.
	const count = 1100
	var stdin strings.Builder
	var refs []string
	for i := 0; i < count; i++ {
		ref := fmt.Sprintf("refs/heads/b%04d", i)
		refs = append(refs, ref)
		fmt.Fprintf(&stdin, "create %s %s\n", ref, c1)
	}
	gittest.ExecOpts(t, env.cfg, gittest.ExecConfig{Stdin: strings.NewReader(stdin.String())}, "-C", repoPath, "update-ref", "--stdin")
	gittest.Exec(t, env.cfg, "-C", repoPath, "pack-refs", "--all")

	deleteRefs(t, env, repo, refs...)

	deletions := recordedDeletions(t, env, repo)
	require.Len(t, deletions, count)
	zero := string(gittest.DefaultObjectHash.ZeroOID)
	for _, ref := range refs {
		require.Equal(t, []string{zero}, deletions[ref], "the deletion of %s must be recorded once", ref)
	}
}

func TestRecordedDeleteRefsAfterRepositoryRecreation(t *testing.T) {
	t.Parallel()

	env, repo, _, c1, _ := recordingEnv(t, testcfg.Build(t))
	zero := string(gittest.DefaultObjectHash.ZeroOID)

	updateBranch(t, env, repo, "x", zero, c1)
	deleteRefs(t, env, repo, "refs/heads/x")
	require.Equal(t, map[string]int{"refs/heads/x": 1}, deletionCounts(t, env, repo))

	_, err := env.repository.RemoveRepository(env.ctx, &gitalypb.RemoveRepositoryRequest{Repository: repo})
	require.NoError(t, err)

	// The bundle's references are created while cloning, which the changeset
	// log does not record.
	_, bundleRepoPath := gittest.CreateRepository(t, env.ctx, env.cfg)
	gittest.WriteCommit(t, env.cfg, bundleRepoPath, gittest.WithBranch("x"))
	gittest.WriteCommit(t, env.cfg, bundleRepoPath, gittest.WithBranch("y"))
	bundlePath := filepath.Join(testhelper.TempDir(t), "repo.bundle")
	gittest.Exec(t, env.cfg, "-C", bundleRepoPath, "bundle", "create", bundlePath, "--all")
	bundle, err := os.ReadFile(bundlePath)
	require.NoError(t, err)

	stream, err := env.repository.CreateRepositoryFromBundle(env.ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&gitalypb.CreateRepositoryFromBundleRequest{Repository: repo}))
	require.NoError(t, stream.Send(&gitalypb.CreateRepositoryFromBundleRequest{Data: bundle}))
	_, err = stream.CloseAndRecv()
	require.NoError(t, err)

	deleteRefs(t, env, repo, "refs/heads/x")
	deleteRefs(t, env, repo, "refs/heads/y")

	require.Equal(t, map[string]int{"refs/heads/x": 1, "refs/heads/y": 1}, deletionCounts(t, env, repo),
		"the deletions in the recreated repository must all be recorded")
}
