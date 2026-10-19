package operations

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
	require.NoError(t, landCheckoutTree(ctx, fx.repo, fx.tree, dir, landCheckoutLimits{}))

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

	require.NoError(t, landCheckoutTreeThen(ctx, fx.repo, fx.tree, t.TempDir(), landCheckoutLimits{}, func() {}), "without damage it passes")
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
		err := landCheckoutTreeThen(ctx, fx.repo, fx.tree, dir, landCheckoutLimits{}, func() { damage(dir) })
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

// blobOID is the object ID git gives a blob with this content.
func blobOID(t testing.TB, content string) git.ObjectID {
	t.Helper()
	oid, err := landBlobOID(strings.NewReader(content), int64(len(content)), git.ObjectID(strings.Repeat("0", 40)))
	require.NoError(t, err)
	return oid
}

func fakeEntries(t testing.TB) ([]landTreeEntry, map[string]string) {
	t.Helper()
	contents := map[string]string{
		"a.txt":   "content\n",
		"bin/run": "#!/bin/sh\n",
		"link":    "a.txt",
	}
	return []landTreeEntry{
		{mode: 0o100644, oid: blobOID(t, contents["a.txt"]), path: "a.txt"},
		{mode: 0o100755, oid: blobOID(t, contents["bin/run"]), path: "bin/run"},
		{mode: 0o120000, oid: blobOID(t, contents["link"]), path: "link"},
		{mode: 0o160000, oid: git.ObjectID(strings.Repeat("4", 40)), path: "mod"},
	}, contents
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

	entries, contents := fakeEntries(t)
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
		fresh, _ := fakeEntries(t)
		require.Error(t, landMaterialize(fresh, bufio.NewReader(strings.NewReader(answer)), t.TempDir()), name)
	}
}

// TestLandCheckMaterialized_failsClosed: the checkout is read back and compared with the listing; any
// difference is an error.
func TestLandCheckMaterialized_failsClosed(t *testing.T) {
	t.Parallel()

	materialized := func(t *testing.T) ([]landTreeEntry, string) {
		t.Helper()
		entries, contents := fakeEntries(t)
		dir := t.TempDir()
		require.NoError(t, landMaterialize(entries, batchOf(contents, entries), dir))
		require.NoError(t, landCheckMaterialized(context.Background(), entries, dir), "an exact checkout passes")
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
		"a changed content of the same length": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("CONTENT\n"), 0o644))
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
		require.ErrorContains(t, landCheckMaterialized(context.Background(), entries, dir), "the checkout differs from the tree", name)
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

// landMaterialize writes the entries under dir without limits, reading the contents of the files and
// links from batch in their order (the answer of git-cat-file(1) --batch to the blobs of the listing,
// gitlinks left out).
func landMaterialize(entries []landTreeEntry, batch *bufio.Reader, dir string) error {
	m := newLandMaterializer(context.Background(), dir, landCheckoutLimits{})
	for i := range entries {
		if err := m.add(&entries[i], batch); err != nil {
			return err
		}
	}
	return nil
}

// fakeTreeEntry is an entry of a tree that landMaterialize writes, with the content that git-cat-file(1)
// would answer for it.
type fakeTreeEntry struct {
	mode    uint32
	path    string
	content string
}

func materializeFake(t *testing.T, dir string, in ...fakeTreeEntry) error {
	t.Helper()

	entries := make([]landTreeEntry, len(in))
	contents := map[string]string{}
	for i, f := range in {
		entries[i] = landTreeEntry{mode: f.mode, oid: blobOID(t, f.content), path: f.path}
		contents[f.path] = f.content
	}
	return landMaterialize(entries, batchOf(contents, entries), dir)
}

// TestLandMaterialize_pathSafety: what the checkout refuses to write, with the reason, and that nothing
// is written outside the directory whatever the order of the entries (PLAN10 15.9): the paths that name
// .git on some file system (the operator's command may run git in the checkout, and a .git of the tree is
// its repository), an entry under a symbolic link, also when a case-insensitive file system sees the same
// name, a link or a file where a directory is needed, and a link that is already there.
func TestLandMaterialize_pathSafety(t *testing.T) {
	t.Parallel()

	reg, link, gitlink := uint32(0o100644), uint32(0o120000), uint32(0o160000)
	// The links point out of the checkout, at the directory that holds it.
	for name, tc := range map[string]struct {
		entries func(outer string) []fakeTreeEntry
		planted bool // a symbolic link "p" to outer is in the checkout before the first entry
	}{
		"a .git directory":           {entries: func(string) []fakeTreeEntry { return []fakeTreeEntry{{reg, ".git/config", "[core]\n"}} }},
		"a .GIT directory":           {entries: func(string) []fakeTreeEntry { return []fakeTreeEntry{{reg, ".GIT/hooks/pre-commit", "x"}} }},
		"a nested .Git":              {entries: func(string) []fakeTreeEntry { return []fakeTreeEntry{{reg, "a/b/.Git/config", "x"}} }},
		"a .git file":                {entries: func(string) []fakeTreeEntry { return []fakeTreeEntry{{reg, ".git", "gitdir: /elsewhere\n"}} }},
		"a .git gitlink":             {entries: func(string) []fakeTreeEntry { return []fakeTreeEntry{{gitlink, ".git", ""}} }},
		"an NTFS .git with a space":  {entries: func(string) []fakeTreeEntry { return []fakeTreeEntry{{reg, ".git /config", "x"}} }},
		"an NTFS .git with a period": {entries: func(string) []fakeTreeEntry { return []fakeTreeEntry{{reg, ".git./config", "x"}} }},
		"the NTFS short name":        {entries: func(string) []fakeTreeEntry { return []fakeTreeEntry{{reg, "GIT~1/config", "x"}} }},
		"an HFS .git":                {entries: func(string) []fakeTreeEntry { return []fakeTreeEntry{{reg, ".g\u200cit/config", "x"}} }},
		"an entry under a link": {entries: func(outer string) []fakeTreeEntry {
			return []fakeTreeEntry{{link, "l", outer}, {reg, "l/x", "planted"}}
		}},
		"an entry under a link, case folded": {entries: func(outer string) []fakeTreeEntry {
			return []fakeTreeEntry{{link, "L", outer}, {reg, "l/x", "planted"}}
		}},
		"a link over an entry's directory, case folded, reversed": {entries: func(outer string) []fakeTreeEntry {
			return []fakeTreeEntry{{reg, "l/x", "planted"}, {link, "L", outer}}
		}},
		"a link over an entry's directory, reversed": {entries: func(outer string) []fakeTreeEntry {
			return []fakeTreeEntry{{reg, "l/x", "planted"}, {link, "l", outer}}
		}},
		"a file where a directory is needed": {entries: func(string) []fakeTreeEntry {
			return []fakeTreeEntry{{reg, "a", "file"}, {reg, "a/x", "planted"}}
		}},
		"a link that is already there": {planted: true, entries: func(string) []fakeTreeEntry {
			return []fakeTreeEntry{{reg, "p/x", "planted"}}
		}},
		"a link that is already there, as a parent of a gitlink": {planted: true, entries: func(string) []fakeTreeEntry {
			return []fakeTreeEntry{{gitlink, "p/mod", ""}}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			outer := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(outer, "sentinel"), []byte("keep"), 0o644))
			dir := filepath.Join(outer, "checkout")
			require.NoError(t, os.Mkdir(dir, 0o755))
			if tc.planted {
				require.NoError(t, os.Symlink(outer, filepath.Join(dir, "p")))
			}

			err := materializeFake(t, dir, tc.entries(outer)...)
			var refusal *landCheckoutRefusal
			require.ErrorAs(t, err, &refusal, "a refusal, with its reason")
			require.NotEmpty(t, refusal.reason)

			// Nothing is written outside the checkout: only the checkout and the sentinel are in outer.
			names := []string{}
			children, err := os.ReadDir(outer)
			require.NoError(t, err)
			for _, c := range children {
				names = append(names, c.Name())
			}
			require.ElementsMatch(t, []string{"checkout", "sentinel"}, names)
			got, err := os.ReadFile(filepath.Join(outer, "sentinel"))
			require.NoError(t, err)
			require.Equal(t, "keep", string(got))
		})
	}

	t.Run("what looks like these and is not is written", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, materializeFake(t, dir,
			fakeTreeEntry{reg, ".gitignore", "x\n"},
			fakeTreeEntry{reg, ".github/workflows/ci.yml", "x\n"},
			fakeTreeEntry{reg, "gitx/a", "x\n"},
			fakeTreeEntry{reg, "git~2/a", "x\n"},
			fakeTreeEntry{reg, "README", "upper\n"},
			fakeTreeEntry{reg, "readme", "lower\n"}, // two files that differ in case only: no link in it
			fakeTreeEntry{link, "ok", "README"},
		))
	})
}

// TestIsGitDirName: the one rule of the resolver package, used by the checkout and by nobody else's copy.
func TestIsGitDirName(t *testing.T) {
	t.Parallel()

	for _, n := range []string{".git", ".GIT", ".Git", ".git ", ".git.", ".git . ", "git~1", "GIT~1", "git~1 ", ".g\u200cit", ".gi\ufeffT", ".git:stream"} {
		require.True(t, resolver.IsGitDirName(n), "%q", n)
	}
	for _, n := range []string{"", "git", ".gitignore", ".github", "gitx", "git~2", ".git~1", "x.git", "a.git ", ".gi t", "git~10"} {
		require.False(t, resolver.IsGitDirName(n), "%q", n)
	}
}

// TestLandCheckoutTree_limits: a tree with more entries or more bytes than the limits is refused, closed,
// with the reason, and exactly at the limit it is written; the limits are checked before the content is
// read, from the listing.
func TestLandCheckoutTree_limits(t *testing.T) {
	t.Parallel()

	fx := newTreeFixture(t)
	ctx := testhelper.Context(t)

	listing := gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "ls-tree", "-r", "-z", "-l", "--full-tree", fx.tree.String())
	entries, err := landParseTreeListing(listing)
	require.NoError(t, err)
	var total int64
	for _, e := range entries {
		require.True(t, e.isGitlink() || e.sized, "the listing gives the size of %q", e.path)
		if !e.isGitlink() {
			total += e.listed
		}
	}

	// The entries count, and so does every directory the checkout makes for them (a gitlink is both).
	written := t.TempDir()
	require.NoError(t, landCheckoutTree(ctx, fx.repo, fx.tree, written, landCheckoutLimits{}))
	dirs := 0
	require.NoError(t, filepath.WalkDir(written, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != written {
			dirs++
		}
		return nil
	}))
	count := len(entries) + dirs
	require.NoError(t, landCheckoutTree(ctx, fx.repo, fx.tree, t.TempDir(), landCheckoutLimits{entries: count, bytes: total}), "at the limits")

	var refusal *landCheckoutRefusal
	dir := t.TempDir()
	require.ErrorAs(t, landCheckoutTree(ctx, fx.repo, fx.tree, dir, landCheckoutLimits{entries: count - 1}), &refusal)
	require.Contains(t, refusal.reason, "more than")
	require.Contains(t, refusal.reason, "verify.max_tree_entries")

	dir = t.TempDir()
	require.ErrorAs(t, landCheckoutTree(ctx, fx.repo, fx.tree, dir, landCheckoutLimits{bytes: total - 1}), &refusal)
	require.Contains(t, refusal.reason, "verify.max_tree_bytes")
	// The bytes were refused from the listing, before the file that goes over was read: less than the tree is there.
	var bytesWritten int64
	require.NoError(t, filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			info, _ := d.Info()
			bytesWritten += info.Size()
		}
		return nil
	}))
	require.Less(t, bytesWritten, total)
}

// TestLandCheckoutTree_streamsALargeTree: a tree whose listing and blobs are far over the buffers between
// the commands (the pipe of the object IDs, the batch reader, the entries in flight) is written exactly:
// nothing waits for the whole listing, and nothing is lost or reordered.
func TestLandCheckoutTree_streamsALargeTree(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	var in []gittest.TreeEntry
	want := map[string]string{}
	for i := 0; i < 1700; i++ {
		name := fmt.Sprintf("d%02d/f%04d.txt", i%50, i)
		content := strings.Repeat(fmt.Sprintf("%d\n", i), 400)
		in = append(in, gittest.TreeEntry{Mode: "100644", Path: name, Content: content})
		want[name] = content
	}
	tree := gittest.WriteTree(t, cfg, repoPath, nestTreeEntries(t, cfg, repoPath, in))

	dir := t.TempDir()
	require.NoError(t, landCheckoutTree(ctx, localrepo.NewTestRepo(t, cfg, repoProto), tree, dir, landCheckoutLimits{entries: 5000, bytes: 1 << 30}))
	for name, content := range want {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		require.NoError(t, err, name)
		require.Equal(t, content, string(got), name)
	}
}

// TestLandVerify_aTreeThatCannotBeCheckedOutIsNotVerified: a tree that holds a .git, or more than the
// limits, makes the verification fail with the reason, and the operator's command does not run.
func TestLandVerify_aTreeThatCannotBeCheckedOutIsNotVerified(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	hostile := gittest.WriteTree(t, cfg, repoPath, nestTreeEntries(t, cfg, repoPath, []gittest.TreeEntry{
		{Mode: "100644", Path: "a", Content: "x\n"},
		{Mode: "100644", Path: ".git/config", Content: "[core]\n\tfsmonitor = touch /tmp/pwned\n"},
	}))
	plain := gittest.WriteTree(t, cfg, repoPath, nestTreeEntries(t, cfg, repoPath, []gittest.TreeEntry{
		{Mode: "100644", Path: "a", Content: "x\n"},
		{Mode: "100644", Path: "b", Content: "y\n"},
	}))

	for name, tc := range map[string]struct {
		tree   git.ObjectID
		tune   func(*config.GraftResolverVerify)
		reason string
	}{
		"a .git":              {hostile, func(*config.GraftResolverVerify) {}, "names .git"},
		"too many entries":    {plain, func(v *config.GraftResolverVerify) { v.MaxTreeEntries = 1 }, "verify.max_tree_entries"},
		"too many bytes":      {plain, func(v *config.GraftResolverVerify) { v.MaxTreeBytes = 3 }, "verify.max_tree_bytes"},
		"no time to check it": {plain, func(v *config.GraftResolverVerify) { v.Timeout = "1ns" }, "did not finish within 1ns"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			vdir, vargv := countingVerify(t, "exit 0")
			rcfg := withVerify(config.GraftResolver{}, vargv, "")
			tc.tune(&rcfg.Verify)
			s := newResolveFixture(t, rcfg).server(t, rcfg)

			err := s.landVerify(ctx, repo, tc.tree)
			var verifyErr *landVerifyError
			require.ErrorAs(t, err, &verifyErr, "a rejection with its reason, not an error of the server")
			require.Contains(t, verifyErr.reason, tc.reason)
			require.Equal(t, 0, verifyRuns(t, vdir), "the command did not run")
		})
	}
}

// shared builds the tree of the attack: a subtree of the given depth, and a root that holds it once per
// name. A few hundred bytes of objects, that git-ls-tree(1) -r expands to one path per name, each as
// deep as the subtree.
func sharedSubtree(t *testing.T, cfg config.Cfg, repoPath string, depth int, names []string) git.ObjectID {
	t.Helper()

	subtree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{{Mode: "100644", Path: "leaf", Content: "leaf\n"}})
	for i := 1; i < depth; i++ {
		subtree = gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{{Mode: "040000", Path: "d", OID: subtree}})
	}
	var root []gittest.TreeEntry
	for _, name := range names {
		root = append(root, gittest.TreeEntry{Mode: "040000", Path: name, OID: subtree})
	}
	return gittest.WriteTree(t, cfg, repoPath, root)
}

// inodes counts what a checkout made under dir.
func inodes(t *testing.T, dir string) int {
	t.Helper()

	n := 0
	require.NoError(t, filepath.WalkDir(dir, func(string, os.DirEntry, error) error { n++; return nil }))
	return n - 1
}

// TestLandCheckoutTree_aSmallPushCannotBecomeALargeCheckout: a tree of a few hundred bytes that git expands
// (many names for one deep subtree, or many directories) is refused, closed and early, by the depth, the
// entries and directories together, and the memory of the paths; what was made under the directory is
// bounded by the limit, not by the expansion.
func TestLandCheckoutTree_aSmallPushCannotBecomeALargeCheckout(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	names := make([]string, 300)
	for i := range names {
		names[i] = fmt.Sprintf("n%03d", i)
	}
	deep := sharedSubtree(t, cfg, repoPath, 200, names[:40])
	medium := sharedSubtree(t, cfg, repoPath, 30, names)

	var wide []gittest.TreeEntry
	for i := 0; i < 600; i++ {
		wide = append(wide, gittest.TreeEntry{Mode: "100644", Path: fmt.Sprintf("w%03d/f", i), Content: "x\n"})
	}
	wideTree := gittest.WriteTree(t, cfg, repoPath, nestTreeEntries(t, cfg, repoPath, wide))

	defaults := landCheckoutLimits{entries: config.DefaultGraftResolverVerifyMaxTreeEntries, bytes: config.DefaultGraftResolverVerifyMaxTreeBytes, depth: config.DefaultGraftResolverVerifyMaxTreeDepth, pathBytes: landMaxPathBytes}
	for name, tc := range map[string]struct {
		tree     git.ObjectID
		limits   landCheckoutLimits
		reason   string
		maxMaker int
	}{
		"the depth, with the defaults": {deep, defaults, "verify.max_tree_depth", 70},
		"directories count as entries": {medium, landCheckoutLimits{entries: 500, depth: 64}, "verify.max_tree_entries", 500},
		"the shallow and wide variant": {wideTree, landCheckoutLimits{entries: 1000, depth: 64}, "verify.max_tree_entries", 1000},
		"the memory of the paths":      {medium, landCheckoutLimits{depth: 64, pathBytes: 20000}, "bytes of memory", 400},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			start := time.Now()
			err := landCheckoutTree(ctx, repo, tc.tree, dir, tc.limits)
			var refusal *landCheckoutRefusal
			require.ErrorAs(t, err, &refusal)
			require.Contains(t, refusal.reason, tc.reason)
			require.Less(t, inodes(t, dir), tc.maxMaker+1, "what the checkout made is bounded by the limit")
			require.Less(t, time.Since(start), 30*time.Second)
		})
	}

	t.Run("the same trees within the limits are written", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, landCheckoutTree(ctx, repo, medium, t.TempDir(), landCheckoutLimits{entries: 100000, depth: 64, pathBytes: landMaxPathBytes}))
	})
}

// TestLandMaterialize_refusesWhatTheFileSystemWouldNot: the limits of the file system are refused up front, or
// turned into a refusal with the reason when the system call fails: a land with such a tree still gets its
// package, and not an error of the server.
func TestLandMaterialize_refusesWhatTheFileSystemWouldNot(t *testing.T) {
	t.Parallel()

	reg, link := uint32(0o100644), uint32(0o120000)
	longName := strings.Repeat("n", 256)
	var longPath []string
	for i := 0; i < 20; i++ {
		longPath = append(longPath, strings.Repeat("p", 250))
	}
	for name, tc := range map[string]struct {
		dir     func(t *testing.T) string
		prepare func(t *testing.T, dir string)
		entries []fakeTreeEntry
		reason  string
	}{
		"a name of more than 255 bytes": {entries: []fakeTreeEntry{{reg, longName, "x"}}, reason: "more than 255 bytes"},
		"a path that reaches PATH_MAX":  {entries: []fakeTreeEntry{{reg, strings.Join(longPath, "/"), "x"}}, reason: "too long"},
		"a NUL in the target of a link": {entries: []fakeTreeEntry{{link, "l", "a\x00b"}}, reason: "NUL"},
		"a name that exists already": {
			prepare: func(t *testing.T, dir string) { require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), nil, 0o644)) },
			entries: []fakeTreeEntry{{reg, "a", "x"}},
			reason:  "open a: file exists",
		},
		"a link target over 4095 bytes": {entries: []fakeTreeEntry{{link, "l", strings.Repeat("t", 4096)}}, reason: "more than the 4095 that Linux takes"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			if tc.dir != nil {
				dir = tc.dir(t)
			}
			if tc.prepare != nil {
				tc.prepare(t, dir)
			}
			var refusal *landCheckoutRefusal
			require.ErrorAs(t, materializeFake(t, dir, tc.entries...), &refusal)
			require.Contains(t, refusal.reason, tc.reason)
			require.NotContains(t, refusal.reason, dir, "a reason names the path inside the checkout, not the directory of the server")
		})
	}

	// What the server's own file system does is no refusal: a fault, for the log and a metric, never a
	// proposal or a rejection of the agent's files.
	t.Run("a failure of the server's file system is a fault", func(t *testing.T) {
		t.Parallel()

		err := materializeFake(t, filepath.Join(t.TempDir(), "missing"), fakeTreeEntry{reg, "a/b", "x"})
		var fault *landCheckoutFault
		var refusal *landCheckoutRefusal
		require.ErrorAs(t, err, &fault)
		require.False(t, errors.As(err, &refusal))
	})

	t.Run("the link target at the limit is written", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, materializeFake(t, dir, fakeTreeEntry{link, "l", strings.Repeat("t", 4095)}))
		got, err := os.Readlink(filepath.Join(dir, "l"))
		require.NoError(t, err)
		require.Len(t, got, 4095)
	})

	t.Run("a context that ended stops the next entry", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		m := newLandMaterializer(ctx, t.TempDir(), landCheckoutLimits{})
		entries, contents := fakeEntries(t)
		require.ErrorIs(t, m.add(&entries[0], batchOf(contents, entries)), context.Canceled)
	})
}

// TestLandMaterialize_theDirectoryClauseOfPathMax: a path that is short itself but with the directory
// of the checkout reaches PATH_MAX is refused up front with its own reason, not left to fail on the
// system call.
func TestLandMaterialize_theDirectoryClauseOfPathMax(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for i := 0; i < 15; i++ {
		dir = filepath.Join(dir, strings.Repeat("d", 250))
		require.NoError(t, os.Mkdir(dir, 0o755))
	}
	// The path is 300 bytes in names of at most 100: far from the limits of its own, over PATH_MAX with the directory.
	long := strings.Join([]string{strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 98)}, "/")
	require.Less(t, len(long), landMaxTreePath)
	require.GreaterOrEqual(t, len(dir)+1+len(long), landPathMax)

	var refusal *landCheckoutRefusal
	require.ErrorAs(t, materializeFake(t, dir, fakeTreeEntry{0o100644, long, "x"}), &refusal)
	require.Contains(t, refusal.reason, "too long to write", "refused by the clause of the directory, not by the system call")
}

// TestLandCheckoutTree_aSymlinkHeavyTreeStaysSmall: the targets of the links are not kept. A tree of
// 20000 links to one 4095-byte blob would hold 80 MB of targets in the entries; what is kept is the
// paths, which is a fraction of it. The heap is read while the checkout is complete and not yet
// checked, when everything it keeps is live. Not parallel: the heap is the process's.
func TestLandCheckoutTree_aSymlinkHeavyTreeStaysSmall(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	blob := gittest.WriteBlob(t, cfg, repoPath, []byte(strings.Repeat("a", 4095)))
	var links []gittest.TreeEntry
	for i := 0; i < 1000; i++ {
		links = append(links, gittest.TreeEntry{Mode: "120000", Path: fmt.Sprintf("l%04d", i), OID: blob})
	}
	sub := gittest.WriteTree(t, cfg, repoPath, links)
	var root []gittest.TreeEntry
	for i := 0; i < 20; i++ {
		root = append(root, gittest.TreeEntry{Mode: "040000", Path: fmt.Sprintf("n%02d", i), OID: sub})
	}
	tree := gittest.WriteTree(t, cfg, repoPath, root)

	heap := func() uint64 {
		var m runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	before := heap()
	var during uint64
	dir := t.TempDir()
	require.NoError(t, landCheckoutTreeThen(ctx, repo, tree, dir, landCheckoutLimits{entries: 100000, depth: 64, pathBytes: landMaxPathBytes}, func() { during = heap() }))
	grew := int64(during) - int64(before)
	t.Logf("heap while the checkout is complete: +%d KiB for 20000 links of 4095 bytes", grew/1024)
	require.Less(t, grew, int64(24<<20), "the targets of the links are not kept")
	got, err := os.Readlink(filepath.Join(dir, "n19", "l0999"))
	require.NoError(t, err)
	require.Len(t, got, 4095)
}

// TestLandCheckMaterialized_stopsWithItsContext: the read-back looks at the context at every entry and
// while it hashes a file, and an ended context is the error, not a difference of the checkout.
func TestLandCheckMaterialized_stopsWithItsContext(t *testing.T) {
	t.Parallel()

	entries, contents := fakeEntries(t)
	dir := t.TempDir()
	require.NoError(t, landMaterialize(entries, batchOf(contents, entries), dir))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := landCheckMaterialized(ctx, entries, dir)
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, err.Error(), "differs from the tree")

	ctx, cancel = context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	require.ErrorIs(t, landCheckMaterialized(ctx, entries, dir), context.DeadlineExceeded)

	// Through the checkout: a context that ends between the writing and the read-back ends it with the
	// context's error, which the verification turns into the timeout of its verdict.
	fx := newTreeFixture(t)
	hookCtx, hookCancel := context.WithCancel(testhelper.Context(t))
	defer hookCancel()
	require.ErrorIs(t, landCheckoutTreeThen(hookCtx, fx.repo, fx.tree, t.TempDir(), landCheckoutLimits{}, hookCancel), context.Canceled)
}

// TestLandVerify_cleansUpWhatTheCommandLeaves: a command that makes directories unreadable and
// read-only leaves nothing behind, and a directory that cannot be removed is counted.
func TestLandVerify_cleansUpWhatTheCommandLeaves(t *testing.T) {
	t.Parallel()

	fx := newTreeFixture(t)
	vdir, vargv := countingVerify(t, `mkdir -p locked/inner
echo x > locked/inner/f
chmod 000 locked/inner
chmod 555 locked
chmod 555 dir
exit 0`)
	rcfg := withVerify(config.GraftResolver{}, vargv, "")
	s := newResolveFixture(t, rcfg).server(t, rcfg)
	s.verifyTmpDir = t.TempDir()

	require.NoError(t, s.landVerify(testhelper.Context(t), fx.repo, fx.tree))
	require.Equal(t, 1, verifyRuns(t, vdir))
	left, err := os.ReadDir(s.verifyTmpDir)
	require.NoError(t, err)
	require.Empty(t, left, "the verification directory is gone, whatever the command made of its permissions")
}

// TestLandVerify_aDeadlineAfterTheCheckoutIsTheTimeoutOfTheVerdict: the timeout covers the read-back too. A
// verification whose deadline passes between the writing of the checkout and its read-back ends as the
// rejection that says it did not finish (PROPOSED for a model, with the reason), never as an error of
// the server, and the command does not run.
func TestLandVerify_aDeadlineAfterTheCheckoutIsTheTimeoutOfTheVerdict(t *testing.T) {
	t.Parallel()

	fx := newTreeFixture(t)
	vdir, vargv := countingVerify(t, "exit 0")
	rcfg := withVerify(config.GraftResolver{}, vargv, "200ms")
	s := newResolveFixture(t, rcfg).server(t, rcfg)
	s.verifyAfterWriteHook = func() { time.Sleep(600 * time.Millisecond) }

	err := s.landVerify(testhelper.Context(t), fx.repo, fx.tree)
	var verifyErr *landVerifyError
	require.ErrorAs(t, err, &verifyErr)
	require.Contains(t, verifyErr.reason, "did not finish within 200ms")
	require.Equal(t, 0, verifyRuns(t, vdir))
}
