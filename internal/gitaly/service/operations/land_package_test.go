package operations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func hunk(b0, b1, o0, o1, t0, t1 uint32) *gitalypb.LandConflictHunk {
	return &gitalypb.LandConflictHunk{
		Base:   &gitalypb.LineRange{Start: b0, End: b1},
		Ours:   &gitalypb.LineRange{Start: o0, End: o1},
		Theirs: &gitalypb.LineRange{Start: t0, End: t1},
	}
}

func TestLandConflictHunks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name               string
		base, ours, theirs string
		want               []*gitalypb.LandConflictHunk
	}{
		{
			name: "one line changed on both sides",
			base: "1\n2\n3\n", ours: "1\nO\n3\n", theirs: "1\nT\n3\n",
			want: []*gitalypb.LandConflictHunk{hunk(2, 3, 2, 3, 2, 3)},
		},
		{
			// Ours adds two lines at the top, which moves the conflict two lines down in ours only.
			name: "ranges count the lines of their own side",
			base: "1\n2\n3\n4\n", ours: "a\nb\n1\n2\nO\n4\n", theirs: "1\n2\nT\nT2\n4\n",
			want: []*gitalypb.LandConflictHunk{hunk(3, 4, 5, 6, 3, 5)},
		},
		{
			name: "non-overlapping changes merge without a hunk",
			base: "1\n2\n3\n4\n5\n", ours: "O\n2\n3\n4\n5\n", theirs: "1\n2\n3\n4\nT\n",
		},
		{
			name: "the same change on both sides is no conflict",
			base: "1\n2\n", ours: "1\nX\n", theirs: "1\nX\n",
		},
		{
			name: "both insert at the same point",
			base: "1\n2\n", ours: "1\nO\n2\n", theirs: "1\nT\n2\n",
			want: []*gitalypb.LandConflictHunk{hunk(2, 2, 2, 3, 2, 3)},
		},
		{
			name: "add/add: no base",
			base: "", ours: "o1\no2\n", theirs: "t1\n",
			want: []*gitalypb.LandConflictHunk{hunk(1, 1, 1, 3, 1, 2)},
		},
		{
			name: "two conflicts",
			base: "1\n2\n3\n4\n5\n", ours: "O1\n2\n3\n4\nO5\n", theirs: "T1\n2\n3\n4\nT5\n",
			want: []*gitalypb.LandConflictHunk{hunk(1, 2, 1, 2, 1, 2), hunk(5, 6, 5, 6, 5, 6)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := landConflictHunks([]byte(tc.base), []byte(tc.ours), []byte(tc.theirs))
			require.Len(t, got, len(tc.want))
			for i := range tc.want {
				require.Equal(t, tc.want[i].String(), got[i].String(), "hunk %d", i)
			}
		})
	}
}

// landConflictDetail returns the LandConflict detail of err, or nil.
func landConflictDetail(err error) *gitalypb.LandConflict {
	for _, detail := range status.Convert(err).Details() {
		if pkg, ok := detail.(*gitalypb.LandConflict); ok {
			return pkg
		}
	}
	return nil
}

// writeRawAnswer sets any JSON as the answer the resolver script of dir gives.
func writeRawAnswer(t *testing.T, dir string, answer any) {
	t.Helper()
	raw, err := json.Marshal(answer)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "answer.json"), raw, 0o600))
}

func packageFile(t *testing.T, pkg *gitalypb.LandConflict, path string) *gitalypb.LandConflictFile {
	t.Helper()
	for _, f := range pkg.GetFiles() {
		if string(f.GetPath()) == path {
			return f
		}
	}
	require.FailNow(t, "no file in the package", "path %q", path)
	return nil
}

func TestUserLand_conflictPackage(t *testing.T) {
	t.Parallel()

	t.Run("a refusal gives every file its outcome", func(t *testing.T) {
		t.Parallel()

		// The resolver refuses with the outcome of each file on stdout, as graft-resolve does.
		dir, argv := resolverScript(t, `cat "$DIR/answer.json"; exit 1`)
		writeRawAnswer(t, dir, resolver.ErrorResponse{
			Error: "unresolved",
			Files: []resolver.FileOutcome{
				{Path: "a", Outcome: resolver.OutcomeResolved},
				{Path: "b", Outcome: resolver.OutcomeRejected, Reason: "syntax check: b does not parse"},
			},
		})
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg, "the merge conflict carries the package")
		require.Equal(t, fx.tip.String(), pkg.GetTip())
		require.Equal(t, fx.commit.String(), pkg.GetCommit())
		require.Equal(t, fx.base.String(), pkg.GetMergeBase())
		require.Empty(t, pkg.GetReason())

		a, b := packageFile(t, pkg, "a"), packageFile(t, pkg, "b")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, a.GetOutcome())
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, b.GetOutcome())
		require.Equal(t, "syntax check: b does not parse", b.GetReason())
		require.Equal(t, gitalypb.LandConflictFile_KIND_CONTENT, b.GetKind())
		require.NotEmpty(t, b.GetOurs().GetOid())
		require.Len(t, b.GetHunks(), 1)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("a malformed outcome list is not trusted in part", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, `cat "$DIR/answer.json"; exit 1`)
		writeRawAnswer(t, dir, resolver.ErrorResponse{
			Error: "unresolved",
			Files: []resolver.FileOutcome{{Path: "a", Outcome: resolver.OutcomeResolved}}, // b is missing
		})
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_FAILED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "unresolved", p)
		}
	})

	t.Run("a lockfile is not sent, the other file is", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, resolver.Response{
			Version:  resolver.ProtocolVersion,
			Resolver: "graft-resolve/fake",
			Files:    []resolver.ResponseFile{{Path: "a", Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged-a\n")}},
		})
		fx := newResolveFixtureWith(t, enabledResolver(argv),
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "base\n"}, {Mode: "100644", Path: "go.sum", Content: "base\n"}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "ours\n"}, {Mode: "100644", Path: "go.sum", Content: "ours\n"}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "theirs\n"}, {Mode: "100644", Path: "go.sum", Content: "theirs\n"}},
		)

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, packageFile(t, pkg, "a").GetOutcome())
		lock := packageFile(t, pkg, "go.sum")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_SENT, lock.GetOutcome())
		require.Equal(t, resolver.NotSentLockfile, lock.GetReason())

		req := resolverRequest(t, dir, 1)
		require.Len(t, req.Files, 1, "the lockfile never reaches the resolver")
		require.Equal(t, "a", req.Files[0].Path)
	})

	t.Run("a disabled resolver gives a package that was not attempted", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Contains(t, pkg.GetReason(), "not enabled")
		for _, f := range pkg.GetFiles() {
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, f.GetOutcome())
		}
	})

	t.Run("without resolve_conflicts there is no package", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.ResolveConflicts = false
		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		require.Nil(t, landConflictDetail(err))
	})
}

// resolutionOf returns the resolution of the fixture's conflicts a and b with the given contents.
func resolutionOf(fx resolveFixture, a, b string) *gitalypb.LandResolution {
	return &gitalypb.LandResolution{
		Tip:    fx.tip.String(),
		Commit: fx.commit.String(),
		Files: []*gitalypb.LandResolvedFile{
			{Path: []byte("a"), Action: gitalypb.LandResolvedFile_ACTION_WRITE, Mode: "100644", Content: []byte(a)},
			{Path: []byte("b"), Action: gitalypb.LandResolvedFile_ACTION_WRITE, Mode: "100644", Content: []byte(b)},
		},
	}
}

func TestUserLand_reland(t *testing.T) {
	t.Parallel()

	t.Run("without a resolver the resolution lands with the server's rules only", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

		resp, err := fx.client.UserLand(fx.ctx, req)
		require.NoError(t, err)
		landed := git.ObjectID(resp.GetLandedCommitId())
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, landed)
		require.Equal(t, "agent-a\n", fx.blobAt(t, landed, "a"))
		require.Equal(t, "agent-b\n", fx.blobAt(t, landed, "b"))
		require.Equal(t, "d\n", fx.blobAt(t, landed, "d"))

		commit, err := fx.repo.ReadCommit(fx.ctx, landed.Revision())
		require.NoError(t, err)
		require.Equal(t, []string{fx.tip.String(), fx.commit.String()}, commit.GetParentIds())
		body := string(commit.GetBody())
		require.Contains(t, body, "Graft-Resolved-By: agent\n")
		require.NotContains(t, body, "Graft-Checked-By:")
		require.Equal(t, "agent", resp.GetResolver())
	})

	t.Run("with a resolver the resolution is checked and the trailer says so", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeRawAnswer(t, dir, resolver.CheckResponse{
			Version: resolver.ProtocolVersion, Operation: resolver.OperationCheck, Resolver: "graft-resolve",
			Files: []resolver.CheckFile{{Path: "a", OK: true, Syntax: resolver.SyntaxNone}, {Path: "b", OK: true, Syntax: resolver.SyntaxNone}},
		})
		fx := newResolveFixture(t, enabledResolver(argv))
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

		resp, err := fx.client.UserLand(fx.ctx, req)
		require.NoError(t, err)
		commit, err := fx.repo.ReadCommit(fx.ctx, git.ObjectID(resp.GetLandedCommitId()).Revision())
		require.NoError(t, err)
		require.Contains(t, string(commit.GetBody()), "Graft-Checked-By: graft-resolve\n")

		check := resolverRequest(t, dir, 1)
		require.Equal(t, resolver.OperationCheck, check.Operation)
		require.Len(t, check.Resolution, 2)
		require.Equal(t, "agent-a\n", string(check.Resolution[0].Content))
	})

	t.Run("a failed check rejects the file", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeRawAnswer(t, dir, resolver.CheckResponse{
			Version: resolver.ProtocolVersion, Operation: resolver.OperationCheck, Resolver: "graft-resolve",
			Files: []resolver.CheckFile{{Path: "a", OK: true, Syntax: resolver.SyntaxNone}, {Path: "b", OK: false, Reason: "check failed: tests", Syntax: resolver.SyntaxNone}},
		})
		fx := newResolveFixture(t, enabledResolver(argv))
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, packageFile(t, pkg, "a").GetOutcome())
		b := packageFile(t, pkg, "b")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, b.GetOutcome())
		require.Equal(t, "check failed: tests", b.GetReason())
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("a resolver that does not know the check fails closed", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer()) // an old resolver answers a resolve response
		fx := newResolveFixture(t, enabledResolver(argv))
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Contains(t, pkg.GetReason(), "could not be checked")
		require.Contains(t, pkg.GetReason(), "does not support the check")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("conflict markers are rejected", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "<<<<<<< ours\nx\n=======\ny\n>>>>>>> theirs\n")

		_, err := fx.client.UserLand(fx.ctx, req)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		b := packageFile(t, pkg, "b")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, b.GetOutcome())
		require.Contains(t, b.GetReason(), "conflict marker")
	})

	t.Run("a moved tip gives a fresh package", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
		req.Resolution.Tip = fx.base.String() // made against another tip

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, fx.tip.String(), pkg.GetTip())
		require.Contains(t, pkg.GetReason(), "tip moved")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("the resolution must resolve exactly the conflicting paths", func(t *testing.T) {
		t.Parallel()

		for name, mutate := range map[string]func(*gitalypb.LandResolution){
			"a path missing":         func(r *gitalypb.LandResolution) { r.Files = r.Files[:1] },
			"a path twice":           func(r *gitalypb.LandResolution) { r.Files[1].Path = []byte("a") },
			"a path not in conflict": func(r *gitalypb.LandResolution) { r.Files[1].Path = []byte("c") },
		} {
			fx := newResolveFixture(t, config.GraftResolver{})
			req := fx.request("Land")
			req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
			mutate(req.Resolution)

			_, err := fx.client.UserLand(fx.ctx, req)
			pkg := landConflictDetail(err)
			require.NotNil(t, pkg, name)
			require.True(t, strings.HasPrefix(pkg.GetReason(), "the resolution "), "%s: reason %q", name, pkg.GetReason())
		}
	})

	t.Run("a resolution for another commit is an invalid argument", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
		req.Resolution.Commit = fx.tip.String()

		_, err := fx.client.UserLand(fx.ctx, req)
		require.Contains(t, err.Error(), "resolution is for commit")
	})
}
