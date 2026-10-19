package operations

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// verifyScript writes a verification command that runs body in the checkout.
func verifyScript(t *testing.T, body string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "verify.sh")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	return []string{p}
}

func withVerify(rcfg config.GraftResolver, argv []string, timeout string) config.GraftResolver {
	rcfg.Verify = config.GraftResolverVerify{Enabled: true, Command: argv, Timeout: timeout}
	return rcfg
}

func TestUserLand_verify(t *testing.T) {
	t.Parallel()

	t.Run("a resolution that passes lands", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		// The checkout holds the merged tree with the resolution: a and b resolved, d from the change.
		fx := newResolveFixture(t, withVerify(enabledResolver(argv), verifyScript(t, `grep -qx merged-a a && grep -qx merged-b b && grep -qx d d`), ""))

		resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		require.NoError(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, git.ObjectID(resp.GetLandedCommitId()))
	})

	t.Run("a failing command rejects every file", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		fx := newResolveFixture(t, withVerify(enabledResolver(argv), verifyScript(t, `echo "2 tests failed"; exit 3`), ""))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		for _, f := range pkg.GetFiles() {
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, f.GetOutcome())
			require.Equal(t, "verification failed: exit status 3: 2 tests failed", f.GetReason())
		}
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("a command that does not finish is rejected", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		fx := newResolveFixture(t, withVerify(enabledResolver(argv), verifyScript(t, `sleep 30`), "300ms"))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Contains(t, packageFile(t, pkg, "a").GetReason(), "did not finish within 300ms")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("an agent's resolution is verified too", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, withVerify(config.GraftResolver{}, verifyScript(t, `grep -qx agent-a a`), ""))
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "wrong\n", "agent-b\n")
		_, err := fx.client.UserLand(fx.ctx, req)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, packageFile(t, pkg, "a").GetOutcome())

		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
		_, err = fx.client.UserLand(fx.ctx, req)
		require.NoError(t, err)
	})
}

func TestLandExtractTar(t *testing.T) {
	t.Parallel()

	archive := func(entries ...*tar.Header) *bytes.Buffer {
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		for _, h := range entries {
			require.NoError(t, w.WriteHeader(h))
			if h.Typeflag == tar.TypeReg {
				_, err := w.Write(make([]byte, h.Size))
				require.NoError(t, err)
			}
		}
		require.NoError(t, w.Close())
		return &b
	}
	file := func(name string) *tar.Header {
		return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}
	}

	dir := t.TempDir()
	require.NoError(t, landExtractTar(archive(
		&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755},
		file("d/f"),
		&tar.Header{Name: "x", Typeflag: tar.TypeReg, Mode: 0o755, Size: 1},
		&tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
	), dir))
	info, err := os.Stat(filepath.Join(dir, "x"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	target, err := os.Readlink(filepath.Join(dir, "l"))
	require.NoError(t, err)
	require.Equal(t, "/etc", target)

	for name, a := range map[string]*bytes.Buffer{
		"under a link":   archive(&tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}, file("l/x")),
		"leaves the dir": archive(file("../x")),
		"absolute":       archive(file("/x")),
	} {
		require.Error(t, landExtractTar(a, t.TempDir()), name)
	}
}
