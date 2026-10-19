package operations

import (
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

var lazyTreeSeed = flag.Uint64("lazy-tree-seed", 0, "seed of TestApplyActionLazyTreeMatchesRecursiveTree, random when 0")

// TestApplyActionLazyTreeMatchesRecursiveTree applies random sequences of UserCommitFiles actions
// to the tree read whole (ReadTree with WithRecursive, then Write: the path before WP96) and to the
// tree of ReadTreeLazily (then the tree writer: the path after it). Every action must give the same
// error on both, and every sequence the same tree ID. The actions reach every outcome: each action
// type succeeds, and each error the whole tree detected (an existing file or directory, a move
// onto an existing file, a missing file to update, move or delete, a file in the way of a
// directory) occurs, also below the top level. A failure names the seed, which
// -lazy-tree-seed replays.
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

	// underFile is whether a directory of path is a file or a submodule in root: Add would then
	// add path below that entry, which no tree can hold.
	underFile := func(root *localrepo.TreeEntry, path string) bool {
		for dir := filepath.Dir(path); dir != "."; dir = filepath.Dir(dir) {
			if entry, err := root.Get(dir); err == nil && entry.Type != localrepo.Tree {
				return true
			}
		}
		return false
	}

	// writeBaseTree writes a random valid tree with regular, executable, symlink and submodule
	// entries, through today's writer.
	writeBaseTree := func() git.ObjectID {
		basePaths = nil
		root := &localrepo.TreeEntry{Mode: "040000", Type: localrepo.Tree}
		for range 4 + rng.IntN(12) {
			path := randomPath()
			if underFile(root, path) {
				continue
			}
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
				require.ErrorIs(t, err, localrepo.ErrEntryExists)
				continue
			}
			basePaths = append(basePaths, path)
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

			recursiveErr := applyAction(ctx, action, recursive, repo)
			lazyErr := applyAction(ctx, action, lazy, repo)
			require.Equal(t, recursiveErr, lazyErr, "seed %d, sequence %d, base %s, actions %v", seed, sequence, baseOID, trace)

			outcomes[outcome(action, recursiveErr)]++
		}

		// A file created below a file leaves a tree that no writer accepts: both must refuse it
		// the same way, and write the same tree otherwise.
		recursiveErr := recursive.Write(ctx, repo)
		lazyErr := writeTreeEntries(ctx, repo, lazy)
		require.Equal(t, fmt.Sprint(recursiveErr), fmt.Sprint(lazyErr), "seed %d, sequence %d, base %s, actions %v", seed, sequence, baseOID, trace)
		require.Equal(t, recursiveErr == nil, lazyErr == nil, "seed %d, sequence %d", seed, sequence)
		if recursiveErr != nil {
			outcomes["write: "+recursiveErr.Error()]++
			continue
		}
		outcomes["write ok"]++
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
		"write ok",
		"write: writing tree: exit status 128",
	} {
		require.Positive(t, outcomes[expected], "seed %d: outcome %q never occurred; outcomes: %v", seed, expected, outcomes)
	}
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
