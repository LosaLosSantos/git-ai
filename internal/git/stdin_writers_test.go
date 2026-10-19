package git

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Where a git command reads revisions from its stdin one per line, a line break in a revision is
// another revision. This test lists every non-test Go file that gives a git command a flag that makes
// it read its stdin (--stdin and the flags that begin with it, --revs, --batch and the flags that begin
// with it), and how each is safe, so that a new writer is classified when it is added. The files are
// parsed, so a flag in a comment is not one, and a guard is a call of ValidateStdinRevision, not a word.

// stdinSafety is how the writer of a git stdin is safe.
type stdinSafety string

const (
	// stdinRevisionsOfRequests: revisions or patterns that callers send, written one per line; the
	// guard file calls ValidateStdinRevision for each.
	stdinRevisionsOfRequests stdinSafety = "revisions of requests, through ValidateStdinRevision"
	// stdinNULDelimited: git reads the input NUL-delimited (-z, -Z), so a line break is data.
	stdinNULDelimited stdinSafety = "NUL-delimited (-z, -Z)"
	// stdinServerInput: the server builds the input, whatever line it writes: object IDs (those of a
	// request are resolved to object IDs first), packs, content, or paths of its own.
	stdinServerInput stdinSafety = "built by the server from object IDs, packs, content or paths of its own"
	// stdinNamesTheFlag: not a writer; the file only compares the flag.
	stdinNamesTheFlag stdinSafety = "not a writer"
)

type stdinWriter struct {
	safety stdinSafety
	// guard is the file that calls ValidateStdinRevision, when it is not the writer's own: the file
	// that joins the patterns of a request is not the one that names the flag.
	guard string
}

var stdinWriters = map[string]stdinWriter{
	"internal/git/gitpipe/revision.go":                                    {safety: stdinRevisionsOfRequests},
	"internal/gitaly/service/repository/objects_size.go":                  {safety: stdinRevisionsOfRequests},
	"internal/git/localrepo/bundle.go":                                    {safety: stdinRevisionsOfRequests, guard: "internal/gitaly/service/repository/create_bundle_from_ref_list.go"},
	"internal/git/updateref/updateref.go":                                 {safety: stdinNULDelimited},
	"internal/graft/corestore/syncer/syncer.go":                           {safety: stdinNULDelimited},
	"internal/git/gitattributes/check_attr.go":                            {safety: stdinNULDelimited},
	"internal/gitaly/service/operations/land_package.go":                  {safety: stdinNULDelimited},
	"internal/gitaly/service/operations/land_verify.go":                   {safety: stdinServerInput},
	"internal/git/catfile/object_reader.go":                               {safety: stdinNULDelimited},
	"internal/gitaly/service/diff/find_changed_paths.go":                  {safety: stdinServerInput},
	"internal/git/localrepo/merge_tree_stream.go":                         {safety: stdinServerInput},
	"internal/git/localrepo/objects.go":                                   {safety: stdinServerInput},
	"internal/git/localrepo/tree.go":                                      {safety: stdinServerInput},
	"internal/git/localrepo/rebase.go":                                    {safety: stdinServerInput},
	"internal/git/localrepo/commit_writer.go":                             {safety: stdinServerInput},
	"internal/git/gitcmd/blob.go":                                         {safety: stdinServerInput},
	"internal/git/gitpipe/catfile_info.go":                                {safety: stdinServerInput},
	"internal/graft/corestore/gitops/gitops.go":                           {safety: stdinServerInput},
	"internal/graft/corestore/voter/shadowpack.go":                        {safety: stdinServerInput},
	"internal/gitaly/service/storagecore/verify_objects.go":               {safety: stdinServerInput},
	"internal/gitaly/service/storagecore/import_pack.go":                  {safety: stdinServerInput},
	"internal/gitaly/storage/storagemgr/partition/check_objects.go":       {safety: stdinServerInput},
	"internal/gitaly/storage/storagemgr/partition/transaction_manager.go": {safety: stdinServerInput},
	"internal/git/gitcmd/agent_walk.go":                                   {safety: stdinNamesTheFlag},
}

// stdinFlagPrefixes are the beginnings of the flags that make a git command read its stdin.
var stdinFlagPrefixes = []string{"--stdin", "--revs", "--batch"}

func moduleRoot(t *testing.T) string {
	t.Helper()

	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(self), "..", "..")
}

// stdinFlagFiles returns the non-test Go files of the module that hold a string literal beginning
// like a flag that makes git read its stdin.
func stdinFlagFiles(t *testing.T, root string) map[string]bool {
	t.Helper()

	found := map[string]bool{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch {
			case d.Name() == "_build", d.Name() == ".git", d.Name() == "node_modules", d.Name() == "testdata":
				return filepath.SkipDir
			case rel == "internal/git/gittest", rel == "internal/testhelper":
				return filepath.SkipDir // helpers of tests
			case rel != ".":
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir // another module
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		require.NoError(t, err, rel)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, prefix := range stdinFlagPrefixes {
				if strings.HasPrefix(value, prefix) {
					found[rel] = true
				}
			}
			return true
		})
		return nil
	}))
	return found
}

// callsValidateStdinRevision reports whether the file calls ValidateStdinRevision: a call, as the
// syntax tree has it, and not a comment or a string that says so.
func callsValidateStdinRevision(t *testing.T, path string) bool {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	require.NoError(t, err, path)
	called := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			called = called || fun.Sel.Name == "ValidateStdinRevision"
		case *ast.Ident:
			called = called || fun.Name == "ValidateStdinRevision"
		}
		return true
	})
	return called
}

func TestStdinWriters_areAllClassified(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	found := stdinFlagFiles(t, root)

	for file := range found {
		require.Contains(t, stdinWriters, file,
			"%s gives a git command a flag that makes it read its stdin: classify it in stdinWriters (are its revisions written one per line? then call ValidateStdinRevision for each)", file)
	}
	for file, writer := range stdinWriters {
		require.Contains(t, found, file, "%s is classified but no longer has such a flag: remove it from stdinWriters", file)
		if writer.safety != stdinRevisionsOfRequests {
			continue
		}
		guard := writer.guard
		if guard == "" {
			guard = file
		}
		require.True(t, callsValidateStdinRevision(t, filepath.Join(root, filepath.FromSlash(guard))),
			"%s writes revisions of requests to a git stdin, and %s does not call ValidateStdinRevision", file, guard)
	}
}

// TestStdinWriters_theGuardIsACallNotAWord: a comment or a string that names the helper is no guard.
func TestStdinWriters_theGuardIsACallNotAWord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	require.True(t, callsValidateStdinRevision(t, write("call.go", "package x\nfunc f() { _ = git.ValidateStdinRevision(\"a\") }\n")))
	require.True(t, callsValidateStdinRevision(t, write("bare.go", "package x\nfunc f() { _ = ValidateStdinRevision(\"a\") }\n")))
	require.False(t, callsValidateStdinRevision(t, write("comment.go", "package x\n// ValidateStdinRevision is called here (it is not)\nfunc f() {}\n")))
	require.False(t, callsValidateStdinRevision(t, write("string.go", "package x\nvar s = \"git.ValidateStdinRevision(x)\"\n")))
	require.False(t, callsValidateStdinRevision(t, write("reference.go", "package x\nvar f = git.ValidateStdinRevision\n")), "a reference is not a call")
}

func TestValidateStdinRevision(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateStdinRevision("refs/heads/main"))
	require.NoError(t, ValidateStdinRevision("--glob=refs/heads/*"))
	for _, revision := range []string{"main\nrefs/graft/green/main", "main\r", "--all\x00", "\n"} {
		require.ErrorContains(t, ValidateStdinRevision(revision), "a revision can't contain a line break or NUL", "%q", revision)
	}
}
