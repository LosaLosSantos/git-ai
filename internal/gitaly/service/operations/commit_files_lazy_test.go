package operations

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

var lazyTreeSeed = flag.Uint64("lazy-tree-seed", 0, "seed of TestApplyActionLazyTreeMatchesRecursiveTree, random when 0")

// TestApplyActionLazyTreeMatchesRecursiveTree applies random sequences of UserCommitFiles actions
// to the tree read whole (ReadTree with WithRecursive, then Write: the path before WP96) and to the
// tree of ReadTreeLazily (then the tree writer: the path after it). Every action must give the same
// error on both, and every sequence the same tree ID: no action leaves a tree that a writer
// refuses. The actions reach every outcome: each action type succeeds, and each error the whole
// tree detected (an existing file or directory, a move onto an existing file, a missing file to
// update, move or delete) occurs below the top level, as does a file in the way of the path a
// file, a directory or a move creates. A failure names the seed, which -lazy-tree-seed replays.
func TestApplyActionLazyTreeMatchesRecursiveTree(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	seed := *lazyTreeSeed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	t.Logf("seed %d (replay with -lazy-tree-seed=%d)", seed, seed)
	rng := rand.New(rand.NewPCG(seed, 0))

	blobs := gittest.WriteBlobs(t, cfg, repoPath, 4)
	submodule := gittest.WriteCommit(t, cfg, repoPath)
	randomBlob := func() git.ObjectID { return blobs[rng.IntN(len(blobs))] }

	// Few names, so that paths collide: a file where a directory is wanted, and the reverse.
	names := []string{"a", "b", "a.b", "a b", "é"}
	// basePaths are the paths of the base tree's entries, which half of the actions address.
	var basePaths []string
	randomPath := func() string {
		if len(basePaths) > 0 && rng.IntN(2) == 0 {
			return basePaths[rng.IntN(len(basePaths))]
		}
		components := make([]string, 1+rng.IntN(3))
		for i := range components {
			components[i] = names[rng.IntN(len(names))]
		}
		return strings.Join(components, "/")
	}

	randomAction := func() commitAction {
		switch rng.IntN(6) {
		case 0:
			return createFile{Path: randomPath(), OID: randomBlob().String(), ExecutableMode: rng.IntN(2) == 0}
		case 1:
			return updateFile{Path: randomPath(), OID: randomBlob().String()}
		case 2:
			action := moveFile{Path: randomPath(), NewPath: randomPath()}
			if rng.IntN(2) == 0 {
				action.OID = randomBlob().String()
			}
			return action
		case 3:
			return deleteFile{Path: randomPath()}
		case 4:
			return changeFileMode{Path: randomPath(), ExecutableMode: rng.IntN(2) == 0}
		default:
			return createDirectory{Path: randomPath()}
		}
	}

	// underFile is whether a directory of path is a file or a submodule in root, so that nothing
	// can be created at path. Get clears the ID of the tree it finds an entry in, as an action
	// does, so it is asked of both trees alike.
	underFile := func(root *localrepo.TreeEntry, path string) bool {
		for dir := filepath.Dir(path); dir != "."; dir = filepath.Dir(dir) {
			if entry, err := root.Get(dir); err == nil && entry.Type != localrepo.Tree {
				return true
			}
		}
		return false
	}

	// The legacy modes that git's canon_mode reads as each canonical mode.
	legacyModes := map[string][]string{
		"100644": {"100644", "100664", "100600"},
		"100755": {"100755", "100775", "100700"},
		"120000": {"120000", "120777"},
		"040000": {"40000", "40755"},
		"160000": {"160000"},
	}
	legacyMode := func(entry *localrepo.TreeEntry) string {
		modes := legacyModes[entry.Mode]
		return modes[rng.IntN(len(modes))]
	}

	// writeBaseTree writes a random valid tree with regular, executable, symlink and submodule
	// entries: through today's writer, or, for half of the trees, with legacy modes that git no
	// longer writes but still reads.
	writeBaseTree := func() git.ObjectID {
		basePaths = nil
		root := &localrepo.TreeEntry{Mode: "040000", Type: localrepo.Tree}
		for range 4 + rng.IntN(12) {
			path := randomPath()
			entry := localrepo.TreeEntry{Path: filepath.Base(path), Type: localrepo.Blob, OID: randomBlob()}
			switch rng.IntN(5) {
			case 0:
				entry.Mode = "100755"
			case 1:
				entry.Mode = "120000"
			case 2:
				entry.Mode, entry.Type, entry.OID = "160000", localrepo.Submodule, submodule
			default:
				entry.Mode = "100644"
			}
			if err := root.Add(path, entry); err != nil {
				if !errors.As(err, &localrepo.FileInPathError{}) {
					require.ErrorIs(t, err, localrepo.ErrEntryExists)
				}
				continue
			}
			basePaths = append(basePaths, path)
		}
		if rng.IntN(2) == 0 {
			return writeLiteralTree(t, cfg, repoPath, root.Entries, legacyMode)
		}
		require.NoError(t, root.Write(ctx, repo))
		return root.OID
	}

	reader, cancel, err := repo.RequestObjectReader(ctx)
	require.NoError(t, err)
	defer cancel()

	outcomes := map[string]int{}
	for sequence := range 120 {
		baseOID := writeBaseTree()

		recursive, err := repo.ReadTree(ctx, baseOID.Revision(), localrepo.WithRecursive())
		require.NoError(t, err)
		lazy, err := repo.ReadTreeLazily(ctx, reader, baseOID)
		require.NoError(t, err)

		var trace []string
		for range 12 {
			action := randomAction()
			trace = append(trace, fmt.Sprintf("%T%+v", action, action))

			// The path an action creates, when a file may be in its way.
			var created string
			switch action := action.(type) {
			case createFile:
				created = action.Path
			case createDirectory:
				created = action.Path
			case moveFile:
				created = action.NewPath
			}
			belowFile := created != "" && underFile(recursive, created)
			require.Equal(t, belowFile, created != "" && underFile(lazy, created))
			// Whether a chmod addresses a directory or a submodule.
			var chmodNonFile bool
			if action, ok := action.(changeFileMode); ok {
				recursiveEntry, recursiveErr := recursive.Get(action.Path)
				lazyEntry, lazyErr := lazy.Get(action.Path)
				require.Equal(t, recursiveErr, lazyErr)
				chmodNonFile = recursiveErr == nil && recursiveEntry.Type != localrepo.Blob
				require.Equal(t, chmodNonFile, lazyErr == nil && lazyEntry.Type != localrepo.Blob)
			}

			recursiveErr := applyAction(ctx, action, recursive, repo)
			lazyErr := applyAction(ctx, action, lazy, repo)
			require.Equal(t, recursiveErr, lazyErr, "seed %d, sequence %d, base %s, actions %v", seed, sequence, baseOID, trace)

			switch {
			case belowFile && recursiveErr != nil:
				outcomes[fmt.Sprintf("%T below a file: %s", action, recursiveErr)]++
			case chmodNonFile:
				outcomes[fmt.Sprintf("%T of a directory: %s", action, recursiveErr)]++
			default:
				outcomes[outcome(action, recursiveErr)]++
			}
		}

		// No action leaves a tree that a writer refuses: both write the same tree.
		require.NoError(t, recursive.Write(ctx, repo), "seed %d, sequence %d, base %s, actions %v", seed, sequence, baseOID, trace)
		require.NoError(t, writeTreeEntries(ctx, repo, lazy), "seed %d, sequence %d, base %s, actions %v", seed, sequence, baseOID, trace)
		require.Equal(t, recursive.OID, lazy.OID, "seed %d, sequence %d, base %s, actions %v", seed, sequence, baseOID, trace)
	}

	for _, expected := range []string{
		"operations.createFile ok",
		"operations.updateFile ok",
		"operations.moveFile ok",
		"operations.deleteFile ok",
		"operations.changeFileMode ok",
		"operations.createDirectory ok",
		"operations.createFile at depth: A file with this name already exists",
		"operations.createDirectory at depth: A file with this name already exists",
		"operations.createDirectory at depth: A directory with this name already exists",
		"operations.moveFile at depth: A file with this name already exists",
		"operations.moveFile at depth: A file with this name doesn't exist",
		"operations.updateFile at depth: A file with this name doesn't exist",
		"operations.deleteFile at depth: A file with this name doesn't exist",
		"operations.changeFileMode at depth: A file with this name doesn't exist",
		"operations.changeFileMode of a directory: A file with this name doesn't exist",
		"operations.createFile below a file: A file with this name already exists",
		"operations.createDirectory below a file: A file with this name already exists",
		"operations.moveFile below a file: A file with this name already exists",
	} {
		require.Positive(t, outcomes[expected], "seed %d: outcome %q never occurred; outcomes: %v", seed, expected, outcomes)
	}
}

// writeLiteralTree writes the tree of entries and the trees below it with `hash-object
// --literally`, each entry with the mode that mode gives it, and returns the tree's ID. The tree
// can so hold modes that git no longer writes (100664, 120777, 40755): those of legacy trees,
// which git still reads, through canon_mode.
func writeLiteralTree(tb testing.TB, cfg config.Cfg, repoPath string, entries []*localrepo.TreeEntry, mode func(*localrepo.TreeEntry) string) git.ObjectID {
	tb.Helper()

	sorted := append([]*localrepo.TreeEntry(nil), entries...)
	sort.Stable(localrepo.TreeEntriesByPath(sorted))

	var content bytes.Buffer
	for _, entry := range sorted {
		oid := entry.OID
		if entry.Type == localrepo.Tree && oid == "" {
			oid = writeLiteralTree(tb, cfg, repoPath, entry.Entries, mode)
		}
		raw, err := oid.Bytes()
		require.NoError(tb, err)
		fmt.Fprintf(&content, "%s %s\x00", mode(entry), entry.Path)
		content.Write(raw)
	}

	return git.ObjectID(text.ChompBytes(gittest.ExecOpts(tb, cfg, gittest.ExecConfig{Stdin: &content},
		"-C", repoPath, "hash-object", "-t", "tree", "-w", "--literally", "--stdin")))
}

// outcome names the outcome of an action: its type, and either ok or its error, with "at depth"
// when the path that failed is below the top level.
func outcome(action commitAction, err error) string {
	if err == nil {
		return fmt.Sprintf("%T ok", action)
	}

	var indexErr indexError
	if errors.As(err, &indexErr) && strings.Contains(indexErr.path, "/") {
		return fmt.Sprintf("%T at depth: %s", action, err)
	}
	return fmt.Sprintf("%T: %s", action, err)
}
