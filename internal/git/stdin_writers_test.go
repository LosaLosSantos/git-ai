package git

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Where a git command reads revisions from its stdin one per line, a line break in a revision is
// another revision. This test lists every non-test file that gives a git command the flag --stdin,
// and how each is safe, so that a new writer is classified when it is added.
const (
	// stdinRevisionsOfRequests: revisions that callers send, written one per line; the file calls
	// ValidateStdinRevision for each.
	stdinRevisionsOfRequests = "revisions of requests, through ValidateStdinRevision"
	// stdinNULDelimited: the input is NUL-delimited (-z), so a line break is data.
	stdinNULDelimited = "NUL-delimited (-z)"
	// stdinServerInput: the input is built by the server from object IDs, packs or content, never
	// from a caller's revision or pattern.
	stdinServerInput = "built by the server from object IDs, packs or content"
	// stdinNamesTheFlag: not a writer; the file only compares the flag.
	stdinNamesTheFlag = "not a writer"
)

var stdinWriters = map[string]string{
	"internal/git/gitpipe/revision.go":                                    stdinRevisionsOfRequests,
	"internal/gitaly/service/repository/objects_size.go":                  stdinRevisionsOfRequests,
	"internal/gitaly/service/diff/find_changed_paths.go":                  stdinNULDelimited,
	"internal/gitaly/service/operations/land_package.go":                  stdinNULDelimited,
	"internal/git/localrepo/merge_tree_stream.go":                         stdinNULDelimited,
	"internal/graft/corestore/syncer/syncer.go":                           stdinNULDelimited,
	"internal/git/gitattributes/check_attr.go":                            stdinNULDelimited,
	"internal/git/updateref/updateref.go":                                 stdinNULDelimited,
	"internal/git/localrepo/bundle.go":                                    stdinServerInput,
	"internal/git/localrepo/objects.go":                                   stdinServerInput,
	"internal/git/localrepo/tree.go":                                      stdinServerInput,
	"internal/git/localrepo/rebase.go":                                    stdinServerInput,
	"internal/git/gitcmd/blob.go":                                         stdinServerInput,
	"internal/graft/corestore/gitops/gitops.go":                           stdinServerInput,
	"internal/gitaly/service/storagecore/verify_objects.go":               stdinServerInput,
	"internal/gitaly/storage/storagemgr/partition/transaction_manager.go": stdinServerInput,
	"internal/git/gitcmd/agent_walk.go":                                   stdinNamesTheFlag,
}

func TestStdinWriters_areAllClassified(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok)
	moduleRoot := filepath.Join(filepath.Dir(self), "..", "..")

	found := map[string]bool{}
	require.NoError(t, filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "_build", ".git", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		if strings.Contains(string(raw), `"--stdin"`) {
			rel, err := filepath.Rel(moduleRoot, path)
			require.NoError(t, err)
			found[filepath.ToSlash(rel)] = true

			if stdinWriters[filepath.ToSlash(rel)] == stdinRevisionsOfRequests {
				require.Contains(t, string(raw), "ValidateStdinRevision", "%s writes revisions of requests to a git stdin without the guard", rel)
			}
		}
		return nil
	}))

	for file := range found {
		require.Contains(t, stdinWriters, file,
			"%s gives a git command --stdin: classify it in stdinWriters (are its revisions written one per line? then call ValidateStdinRevision for each)", file)
	}
	for file := range stdinWriters {
		require.Contains(t, found, file, "%s is classified but no longer uses --stdin: remove it from stdinWriters", file)
	}
}

func TestValidateStdinRevision(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateStdinRevision("refs/heads/main"))
	require.NoError(t, ValidateStdinRevision("--glob=refs/heads/*"))
	for _, revision := range []string{"main\nrefs/graft/green/main", "main\r", "--all\x00", "\n"} {
		require.ErrorContains(t, ValidateStdinRevision(revision), "a revision can't contain a line break or NUL", "%q", revision)
	}
}
