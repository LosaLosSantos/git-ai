package operations

import (
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// TestLandMaterialize_anEmptyLinkTargetIsRefused: git takes a symbolic link whose blob is empty, and
// no file system does (symlink(2) says ENOENT). It is the tree's doing, so it is a refusal with the
// reason, up front, and never a fault of the server: a pusher cannot make every verification of its
// change an internal error.
func TestLandMaterialize_anEmptyLinkTargetIsRefused(t *testing.T) {
	t.Parallel()

	var refusal *landCheckoutRefusal
	require.ErrorAs(t, materializeFake(t, t.TempDir(), fakeTreeEntry{0o120000, "l", ""}), &refusal)
	require.Contains(t, refusal.reason, "empty target")

	// From the listing, before the blob is read.
	m := newLandMaterializer(testhelper.Context(t), t.TempDir(), landCheckoutLimits{})
	e := landTreeEntry{mode: 0o120000, oid: git.ObjectID("0123456789012345678901234567890123456789"), path: "l", sized: true, listed: 0}
	require.ErrorAs(t, m.add(&e, nil), &refusal)
	require.Contains(t, refusal.reason, "empty target")

	// Through the verification of a real tree: the verdict says so and the command does not run.
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	tree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
		{Mode: "100644", Path: "a", Content: "x\n"},
		{Mode: "120000", Path: "empty-link", Content: ""},
	})
	vdir, vargv := countingVerify(t, "exit 0")
	rcfg := withVerify(config.GraftResolver{}, vargv, "")
	s := newResolveFixture(t, rcfg).server(t, rcfg)
	err := s.landVerify(ctx, localrepo.NewTestRepo(t, cfg, repoProto), tree)
	var verifyErr *landVerifyError
	require.ErrorAs(t, err, &verifyErr)
	require.Contains(t, verifyErr.reason, "empty target")
	require.Equal(t, 0, verifyRuns(t, vdir))
}

// TestLandMaterializer_sortsTheErrnos: what the tree causes is a refusal, what the server's file system
// causes is a fault.
func TestLandMaterializer_sortsTheErrnos(t *testing.T) {
	t.Parallel()

	m := newLandMaterializer(testhelper.Context(t), "/srv/checkout", landCheckoutLimits{})
	for _, errno := range []syscall.Errno{syscall.EEXIST, syscall.ENAMETOOLONG, syscall.EILSEQ, syscall.ELOOP, syscall.ENOTDIR, syscall.EISDIR, syscall.ENOTEMPTY, syscall.EINVAL, syscall.EMLINK} {
		err := m.osError(&fs.PathError{Op: "mkdir", Path: "/srv/checkout/a/b", Err: errno})
		var refusal *landCheckoutRefusal
		require.ErrorAs(t, err, &refusal, errno.Error())
		require.NotContains(t, refusal.reason, "/srv/checkout", "relative paths only")
		require.Contains(t, refusal.reason, "mkdir a/b")
	}
	for _, errno := range []syscall.Errno{syscall.ENOSPC, syscall.EMFILE, syscall.ENFILE, syscall.EIO, syscall.EACCES, syscall.EPERM, syscall.EROFS, syscall.EDQUOT, syscall.ENOMEM, syscall.ENOENT} {
		var fault *landCheckoutFault
		require.ErrorAs(t, m.osError(&fs.PathError{Op: "mkdir", Path: "/srv/checkout/a", Err: errno}), &fault, errno.Error())
	}
}

// TestUserLand_aTreeIsNotVerifiedForATipTheBranchLeft: the wait for a verify slot can take as long as the
// verification before it. A tree built from a tip that the branch left meanwhile is not verified: the command
// runs once, on the tree of the tip the land lands on.
func TestUserLand_aTreeIsNotVerifiedForATipTheBranchLeft(t *testing.T) {
	t.Parallel()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, validAnswer())
	vdir, vargv := countingVerify(t, `test -f e`)
	rcfg := withVerify(enabledResolver(rargv), vargv, "")
	fx, s := heldPool(t, rcfg)

	slot, err := s.verifyPool.Acquire(fx.ctx, landVerifyRepoKey(fx.repoProto))
	require.NoError(t, err)
	done := landAsync(fx, fx.commit, "Land")
	// The land has resolved and waits for the slot of its repository.
	require.Eventually(t, func() bool { return resolverCalls(t, rdir) == 1 }, 30*time.Second, 10*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	writer := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip), gittest.WithBranch("main"),
		gittest.WithTreeEntries(entries("a", "ours\n", "b", "ours\n", "c", "c\n", "e", "e\n")...))
	slot.Release()

	resp := requireLanded(t, done)
	commit, landed := landedCommit(t, fx, resp)
	require.Equal(t, []string{writer.String(), fx.commit.String()}, commit.GetParentIds())
	require.Equal(t, "e\n", fx.blobAt(t, landed, "e"))
	require.Equal(t, 1, verifyRuns(t, vdir), "the tree of the tip the branch left was never verified: the command ran once, on the new tree")
	require.Equal(t, 1, resolverCalls(t, rdir), "the verdicts were kept")
}

// TestUserLand_contendedRulesLandsWithVerification: N lands of rules on one branch, with a verification
// configured and a deadline short enough to bite. Each ends landed or with the package and its reason; none
// hangs or fails otherwise; at least one lands.
func TestUserLand_contendedRulesLandsWithVerification(t *testing.T) {
	t.Parallel()

	const n = 5
	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, answerOf("a"))
	vdir, vargv := countingVerify(t, "sleep 0.3")
	rcfg := withVerify(enabledResolver(rargv), vargv, "2s")
	rcfg.Timeout = "4s"
	fx := newResolveFixtureWith(t, rcfg,
		entries("a", "base\n", "f0", "base\n"),
		entries("a", "ours\n", "f0", "base\n"),
		entries("a", "theirs-0\n", "f0", "base\n"),
	)
	commits := []git.ObjectID{fx.commit}
	for i := 1; i < n; i++ {
		commits = append(commits, gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base),
			gittest.WithTreeEntries(entries("a", fmt.Sprintf("theirs-%d\n", i), "f0", "base\n", fmt.Sprintf("n%d", i), "x\n")...)))
	}

	results := make([]landResult, n)
	var wg sync.WaitGroup
	start := time.Now()
	for i, c := range commits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := fx.request(fmt.Sprintf("Land %d", i))
			req.CommitId = c.String()
			resp, err := fx.client.UserLand(fx.ctx, req)
			results[i] = landResult{resp: resp, err: err}
		}()
	}
	wg.Wait()
	require.Less(t, time.Since(start), 90*time.Second, "none hangs")

	landed := 0
	for i, r := range results {
		if r.err == nil {
			landed++
			continue
		}
		requireMergeConflict(t, r.err)
		pkg := landConflictDetail(r.err)
		require.NotNil(t, pkg, "land %d ends with the package", i)
		require.NotEmpty(t, packageFile(t, pkg, "a").GetReason(), "land %d says why", i)
	}
	require.GreaterOrEqual(t, landed, 1, "at least one lands")
	require.GreaterOrEqual(t, verifyRuns(t, vdir), landed)
	t.Logf("%d of %d landed, %d verifications", landed, n, verifyRuns(t, vdir))
}

// TestLandWriteResolution_wantsTheVerifiedTree: the lane's last guard compares the tree itself, not a bool.
func TestLandWriteResolution_wantsTheVerifiedTree(t *testing.T) {
	t.Parallel()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, validAnswer())
	_, vargv := countingVerify(t, "exit 0")
	rcfg := withVerify(enabledResolver(rargv), vargv, "")
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)
	s.locator = config.NewLocator(fx.cfg)
	in := fx.request("Land")

	pre := newLandPreResolution()
	l, answer, err := s.landResolveAt(fx.ctx, in, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
	require.NoError(t, err)
	tree, err := s.landResolvedTree(fx.ctx, fx.repo, l.mergedTree, answer)
	require.NoError(t, err)
	require.True(t, pre.isVerified(tree))

	other := git.ObjectID("0000000000000000000000000000000000000000")
	for name, verified := range map[string]git.ObjectID{"nothing": "", "another tree": other} {
		_, err := s.landWriteResolution(fx.ctx, in, fx.repo, fx.tip, fx.commit, l.mergedTree, fx.committer(), "actor", answer, "", tree, verified)
		require.ErrorContains(t, err, "without the verification of its tree", name)
	}
	res, err := s.landWriteResolution(fx.ctx, in, fx.repo, fx.tip, fx.commit, l.mergedTree, fx.committer(), "actor", answer, "", tree, tree)
	require.NoError(t, err)
	require.NotEmpty(t, res.commit)
}

// TestLandPrecheck_noVerificationWhenTheResolverCannotBeBuilt: the lane refuses the resolution of a
// configured resolver that cannot be built, before anything else; the precheck does not spend a
// verification on it.
func TestLandPrecheck_noVerificationWhenTheResolverCannotBeBuilt(t *testing.T) {
	t.Parallel()

	vdir, vargv := countingVerify(t, "exit 0")
	rcfg := withVerify(config.GraftResolver{}, vargv, "")
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)
	s.locator = config.NewLocator(fx.cfg)
	s.resolverErr = errors.New("the token file is gone")
	in := fx.request("Land")
	in.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

	pc, err := s.landPrecheckResolution(fx.ctx, in, fx.repo)
	require.NoError(t, err)
	require.Nil(t, pc)
	require.Equal(t, 0, verifyRuns(t, vdir))
}
