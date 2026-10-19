package quarantine

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQuarantineWritersKeepTheDurabilityContract checks the source of the server for the places
// that decide how the writers of a quarantine fsync their objects, so that a new one is noticed and
// checked against the contract of Dir (every file of a quarantine is durable before Migrate):
//
//   - core.fsync is set only by the command factory, for every git command, with objects in it;
//     by WithoutObjectFsync, whose helpers' objects the batch attempts pack into one fsynced pack
//     before their update; by the custom hooks' environment, which repeats the factory's value;
//     and, to FsyncComponentsWithoutReferences (objects still in it), by the git update-ref of
//     the storage core's deferred reference durability: updateref.go (the hook's update, run on
//     the repository after Migrate) and the syncer (its catch-up, on the local repository or a
//     build directory, never a quarantine). update-ref writes references only.
//   - WithoutObjectFsync is used only by the rebase engine's deferred durability, which only the
//     pack mode of the batch attempts asks for.
//
// It cannot see a configuration key that is not written as a literal, such as the configuration
// options of a client's push, which git receive-pack applies to its own quarantine and not to one
// that Migrate moves.
func TestQuarantineWritersKeepTheDurabilityContract(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(root, "go.mod"))

	uses := map[string][]string{}
	for _, dir := range []string{"internal", "cmd"} {
		require.NoError(t, filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			for _, needle := range []string{`"core.fsync"`, `"core.fsyncMethod"`, `WithoutObjectFsync()`} {
				if strings.Contains(string(data), needle) {
					uses[needle] = append(uses[needle], rel)
				}
			}
			return nil
		}))
	}
	for _, files := range uses {
		sort.Strings(files)
	}

	require.Equal(t, map[string][]string{
		`"core.fsync"`: {
			"internal/git/gitcmd/command_factory.go",
			"internal/git/localrepo/merge_tree_stream.go",
			"internal/git/updateref/updateref.go",
			"internal/gitaly/hook/custom.go",
			"internal/graft/corestore/syncer/syncer.go",
		},
		`"core.fsyncMethod"`: {
			"internal/git/gitcmd/command_factory.go",
			"internal/gitaly/hook/custom.go",
		},
		`WithoutObjectFsync()`: {
			"internal/git/localrepo/merge_tree_stream.go",
			"internal/graft/rebase/engine.go",
		},
	}, uses, "a new place decides how objects are fsynced: check it against the durability contract of quarantine.Dir and add it here")

	factory, err := os.ReadFile(filepath.Join(root, "internal/git/gitcmd/command_factory.go"))
	require.NoError(t, err)
	require.Contains(t, string(factory), `FsyncComponents = "objects,derived-metadata,reference"`,
		"the command factory fsyncs the objects of every git command")
	require.Contains(t, string(factory), `ConfigPair{Key: "core.fsync", Value: FsyncComponents}`,
		"the command factory fsyncs the objects of every git command")
	require.Contains(t, string(factory), `FsyncComponentsWithoutReferences = "objects,derived-metadata"`,
		"the deferred reference durability keeps the objects fsynced")
	for _, file := range []string{"internal/git/updateref/updateref.go", "internal/graft/corestore/syncer/syncer.go"} {
		data, err := os.ReadFile(filepath.Join(root, file))
		require.NoError(t, err)
		require.Contains(t, string(data), `ConfigPair{Key: "core.fsync", Value: gitcmd.FsyncComponentsWithoutReferences}`,
			"%s sets core.fsync only to the components without references", file)
		require.Equal(t, 1, strings.Count(string(data), `"core.fsync"`), "%s sets core.fsync once", file)
	}
}
