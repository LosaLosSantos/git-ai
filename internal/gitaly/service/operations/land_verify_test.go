package operations

import (
	"archive/tar"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
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

// treeFixture is a repository and the tree in it that the tests of the checkout write out: files that
// the attributes of git-archive(1) would drop or change, modes, links, a gitlink, odd names, binary
// content.
type treeFixture struct {
	cfg      config.Cfg
	repoPath string
	repo     *localrepo.Repo
	tree     git.ObjectID
}

func newTreeFixture(t *testing.T) treeFixture {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	entries := []gittest.TreeEntry{
		{Mode: "100644", Path: ".gitattributes", Content: "export-ignore.log export-ignore\nsubst.txt export-subst\ncrlf.txt text eol=crlf\n*.bin -text\n"},
		{Mode: "100644", Path: "export-ignore.log", Content: "hidden from git archive\n"},
		{Mode: "100644", Path: "subst.txt", Content: "$Format:%H$\n"},
		{Mode: "100644", Path: "crlf.txt", Content: "one\ntwo\n"},
		{Mode: "100644", Path: "bin.bin", Content: "\x00\x01\xff\xfe\r\n\x00"},
		{Mode: "100644", Path: "empty", Content: ""},
		{Mode: "100755", Path: "run.sh", Content: "#!/bin/sh\nexit 0\n"},
		{Mode: "120000", Path: "link", Content: "../../etc/passwd"},
		{Mode: "100644", Path: "dir/sub dir/file with space.txt", Content: "space\n"},
		{Mode: "100644", Path: "tab\tname.txt", Content: "tab\n"},
		{Mode: "100644", Path: "line\nbreak.txt", Content: "line\n"},
		{Mode: "100644", Path: "é/日本語.txt", Content: "unicode\n"},
		{Mode: "160000", Path: "sub", OID: commit},
	}
	tree := gittest.WriteTree(t, cfg, repoPath, nestTreeEntries(t, cfg, repoPath, entries))
	return treeFixture{cfg: cfg, repoPath: repoPath, repo: localrepo.NewTestRepo(t, cfg, repoProto), tree: tree}
}

// TestLandCheckoutTree: the checkout is the tree byte for byte, whatever the attributes of the tree say
// (PLAN10 15.9). git-archive(1), which it replaces, drops the file marked export-ignore, and would expand
// export-subst for a commit: a .gitattributes of an agent, or of a model, would have decided what the operator's
// command verifies.
func TestLandCheckoutTree(t *testing.T) {
	t.Parallel()

	fx := newTreeFixture(t)
	ctx := testhelper.Context(t)

	// The repro: what git archive makes of this very tree.
	archived := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "archive", "--format=tar", fx.tree.String())))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Typeflag == tar.TypeReg {
			content, err := io.ReadAll(tr)
			require.NoError(t, err)
			archived[h.Name] = string(content)
		}
	}
	require.NotContains(t, archived, "export-ignore.log", "git archive drops the file marked export-ignore")
	// export-subst expands only when a commit is archived, and a tree is: so it changed nothing of what
	// the verification saw, and the checkout still applies neither.
	require.Equal(t, "$Format:%H$\n", archived["subst.txt"])

	dir := t.TempDir()
	require.NoError(t, landCheckoutTree(ctx, fx.repo, fx.tree, dir))

	for path, want := range map[string]string{
		".gitattributes":                  "export-ignore.log export-ignore\nsubst.txt export-subst\ncrlf.txt text eol=crlf\n*.bin -text\n",
		"export-ignore.log":               "hidden from git archive\n",
		"subst.txt":                       "$Format:%H$\n",
		"crlf.txt":                        "one\ntwo\n",
		"bin.bin":                         "\x00\x01\xff\xfe\r\n\x00",
		"empty":                           "",
		"run.sh":                          "#!/bin/sh\nexit 0\n",
		"dir/sub dir/file with space.txt": "space\n",
		"tab\tname.txt":                   "tab\n",
		"line\nbreak.txt":                 "line\n",
		"é/日本語.txt":                       "unicode\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		require.NoError(t, err, path)
		require.Equal(t, want, string(got), path)
	}
	info, err := os.Stat(filepath.Join(dir, "run.sh"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	info, err = os.Stat(filepath.Join(dir, "crlf.txt"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	target, err := os.Readlink(filepath.Join(dir, "link"))
	require.NoError(t, err)
	require.Equal(t, "../../etc/passwd", target, "a link is written as a link, and not followed")

	gitlink, err := os.Stat(filepath.Join(dir, "sub"))
	require.NoError(t, err)
	require.True(t, gitlink.IsDir())
	children, err := os.ReadDir(filepath.Join(dir, "sub"))
	require.NoError(t, err)
	require.Empty(t, children, "a gitlink is an empty directory")
}

// TestLandCheckoutTree_theCheckIsPartOfIt: a checkout that does not equal the tree fails, closed, through
// landCheckoutTree itself, whatever the damage and wherever it came from.
func TestLandCheckoutTree_theCheckIsPartOfIt(t *testing.T) {
	t.Parallel()

	fx := newTreeFixture(t)
	ctx := testhelper.Context(t)

	require.NoError(t, landCheckoutTreeThen(ctx, fx.repo, fx.tree, t.TempDir(), func() {}), "without damage it passes")
	for name, damage := range map[string]func(dir string){
		"a file written in": func(dir string) { _ = os.WriteFile(filepath.Join(dir, "planted"), []byte("x"), 0o644) },
		"a file of the tree gone": func(dir string) {
			_ = os.Remove(filepath.Join(dir, "export-ignore.log"))
		},
		"a file rewritten": func(dir string) {
			_ = os.WriteFile(filepath.Join(dir, "subst.txt"), []byte("expanded content, longer than the tree has\n"), 0o644)
		},
	} {
		dir := t.TempDir()
		err := landCheckoutTreeThen(ctx, fx.repo, fx.tree, dir, func() { damage(dir) })
		require.ErrorContains(t, err, "the checkout differs from the tree", name)
	}
}

// batchOf is the answer of git-cat-file(1) --batch to the blobs of entries, gitlinks left out.
func batchOf(contents map[string]string, entries []landTreeEntry) *bufio.Reader {
	var b bytes.Buffer
	for _, e := range entries {
		if e.isGitlink() {
			continue
		}
		c := contents[e.path]
		fmt.Fprintf(&b, "%s blob %d\n%s\n", e.oid, len(c), c)
	}
	return bufio.NewReader(&b)
}

func fakeEntries() ([]landTreeEntry, map[string]string) {
	oid := func(c string) git.ObjectID { return git.ObjectID(strings.Repeat(c, 40)) }
	return []landTreeEntry{
			{mode: 0o100644, oid: oid("1"), path: "a.txt"},
			{mode: 0o100755, oid: oid("2"), path: "bin/run"},
			{mode: 0o120000, oid: oid("3"), path: "link"},
			{mode: 0o160000, oid: oid("4"), path: "mod"},
		}, map[string]string{
			"a.txt":   "content\n",
			"bin/run": "#!/bin/sh\n",
			"link":    "a.txt",
		}
}

// TestLandMaterialize_refusesWhatLeavesTheDirectory: a path that is not clean, that is absolute or that
// goes up, and an entry under a symbolic link that was written, are refused.
func TestLandMaterialize_refusesWhatLeavesTheDirectory(t *testing.T) {
	t.Parallel()

	oid := git.ObjectID(strings.Repeat("a", 40))
	for name, entries := range map[string][]landTreeEntry{
		"up":           {{mode: 0o100644, oid: oid, path: "../x"}},
		"absolute":     {{mode: 0o100644, oid: oid, path: "/x"}},
		"not clean":    {{mode: 0o100644, oid: oid, path: "a/../x"}},
		"a dot":        {{mode: 0o100644, oid: oid, path: "./x"}},
		"empty":        {{mode: 0o100644, oid: oid, path: ""}},
		"under a link": {{mode: 0o120000, oid: oid, path: "l"}, {mode: 0o100644, oid: oid, path: "l/x"}},
	} {
		contents := map[string]string{"../x": "x", "/x": "x", "a/../x": "x", "./x": "x", "": "x", "l": "/tmp", "l/x": "x"}
		require.Error(t, landMaterialize(entries, batchOf(contents, entries), t.TempDir()), name)
	}
}

// TestLandMaterialize_refusesAnswersItDidNotAskFor: the header of each blob must be the object asked for.
func TestLandMaterialize_refusesAnswersItDidNotAskFor(t *testing.T) {
	t.Parallel()

	entries, contents := fakeEntries()
	good := batchOf(contents, entries)
	require.NoError(t, landMaterialize(entries, good, t.TempDir()))

	for name, answer := range map[string]string{
		"another object":      strings.Repeat("9", 40) + " blob 3\nabc\n",
		"a tree":              entries[0].oid.String() + " tree 3\nabc\n",
		"missing":             entries[0].oid.String() + " missing\n",
		"a negative size":     entries[0].oid.String() + " blob -1\n",
		"a short blob":        entries[0].oid.String() + " blob 30\nabc\n",
		"no line feed behind": entries[0].oid.String() + " blob 3\nabcX",
	} {
		fresh, _ := fakeEntries()
		require.Error(t, landMaterialize(fresh, bufio.NewReader(strings.NewReader(answer)), t.TempDir()), name)
	}
}

// TestLandCheckMaterialized_failsClosed: the checkout is read back and compared with the listing; any
// difference is an error.
func TestLandCheckMaterialized_failsClosed(t *testing.T) {
	t.Parallel()

	materialized := func(t *testing.T) ([]landTreeEntry, string) {
		t.Helper()
		entries, contents := fakeEntries()
		dir := t.TempDir()
		require.NoError(t, landMaterialize(entries, batchOf(contents, entries), dir))
		require.NoError(t, landCheckMaterialized(entries, dir), "an exact checkout passes")
		return entries, dir
	}

	for name, damage := range map[string]func(t *testing.T, dir string){
		"an extra file": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "extra"), nil, 0o644))
		},
		"an extra empty directory": func(t *testing.T, dir string) {
			require.NoError(t, os.Mkdir(filepath.Join(dir, "extra"), 0o755))
		},
		"a file in a gitlink": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "mod", "x"), nil, 0o644))
		},
		"a missing file": func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, "a.txt")))
		},
		"a changed length": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("longer than before\n"), 0o644))
		},
		"an executable file that is not": func(t *testing.T, dir string) {
			require.NoError(t, os.Chmod(filepath.Join(dir, "a.txt"), 0o755))
		},
		"a file that lost its executable bit": func(t *testing.T, dir string) {
			require.NoError(t, os.Chmod(filepath.Join(dir, "bin", "run"), 0o644))
		},
		"another target of a link": func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, "link")))
			require.NoError(t, os.Symlink("elsewhere", filepath.Join(dir, "link")))
		},
		"a link that became a file": func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, "link")))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "link"), []byte("a.txt"), 0o644))
		},
		"a gitlink that became a file": func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, "mod")))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "mod"), nil, 0o644))
		},
	} {
		entries, dir := materialized(t)
		damage(t, dir)
		require.ErrorContains(t, landCheckMaterialized(entries, dir), "the checkout differs from the tree", name)
	}
}

func TestLandParseTreeListing(t *testing.T) {
	t.Parallel()

	oid := strings.Repeat("a", 40)
	entries, err := landParseTreeListing([]byte(
		"100644 blob " + oid + "\tplain\x00" +
			"100755 blob " + oid + "\twith space/and\ttab\nand line break\x00" +
			"120000 blob " + oid + "\tlink\x00" +
			"160000 commit " + oid + "\tmod\x00"))
	require.NoError(t, err)
	require.Len(t, entries, 4)
	require.Equal(t, "with space/and\ttab\nand line break", entries[1].path)
	require.True(t, entries[1].isRegular())
	require.True(t, entries[2].isSymlink())
	require.True(t, entries[3].isGitlink())

	for name, raw := range map[string]string{
		"no tab":                   "100644 blob " + oid + " plain\x00",
		"a tree":                   "040000 tree " + oid + "\tdir\x00",
		"a blob that is a gitlink": "160000 blob " + oid + "\tmod\x00",
		"a commit that is a file":  "100644 commit " + oid + "\tx\x00",
		"a mode nobody has":        "777777 blob " + oid + "\tx\x00",
		"a mode that is no number": "10064z blob " + oid + "\tx\x00",
		"too few fields":           "100644 blob\tx\x00",
	} {
		_, err := landParseTreeListing([]byte(raw))
		require.Error(t, err, name)
	}
}

// TestUserLand_verifySeesTheWholeTree: the operator's command is run on the tree that lands, and a
// .gitattributes in it, written by an agent or by a model, hides nothing from it: the file marked
// export-ignore is there, and the file marked export-subst has its own content. A verification that
// did not see them would pass a tree it had not seen.
func TestUserLand_verifySeesTheWholeTree(t *testing.T) {
	t.Parallel()

	for desc, answer := range map[string]resolver.Response{"a model's resolution": modelAnswer(), "a rules' resolution": validAnswer()} {
		t.Run(desc, func(t *testing.T) {
			t.Parallel()

			dir, argv := resolverScript(t, "")
			writeAnswer(t, dir, answer)
			attributes := []gittest.TreeEntry{
				{Mode: "100644", Path: ".gitattributes", Content: "hidden.txt export-ignore\nsubst.txt export-subst\n"},
				{Mode: "100644", Path: "hidden.txt", Content: "must be seen\n"},
				{Mode: "100644", Path: "subst.txt", Content: "$Format:%H$\n"},
			}
			verify := verifyScript(t, `test -f .gitattributes && grep -qx 'must be seen' hidden.txt && grep -qxF '$Format:%H$' subst.txt && grep -qx merged-a a`)
			fx := newResolveFixtureWith(t, withVerify(enabledResolver(argv), verify, ""),
				[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "base\n"}, {Mode: "100644", Path: "b", Content: "base\n"}},
				[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "ours\n"}, {Mode: "100644", Path: "b", Content: "ours\n"}},
				append([]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "theirs\n"}, {Mode: "100644", Path: "b", Content: "theirs\n"}}, attributes...),
			)

			resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
			require.NoError(t, err, "the verification saw the hidden file and the unexpanded content")
			landed := git.ObjectID(resp.GetLandedCommitId())
			require.Equal(t, "must be seen\n", fx.blobAt(t, landed, "hidden.txt"))
		})
	}
}
