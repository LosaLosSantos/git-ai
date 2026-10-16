package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// TestApplyNamesRetriesDeletionsFirst proves that the deletion and the creation
// of conflicting names cannot share one transaction, so that the retry in
// applyNames is what makes ApplyRefs succeed. The external tests only see the
// final state, which is the same either way.
func TestApplyNamesRetriesDeletionsFirst(t *testing.T) {
	t.Parallel()

	for _, reftable := range []bool{false, true} {
		name := "files"
		if reftable {
			name = "reftable"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			repo, path := newInternalRepo(t, ctx, cfg, reftable)

			c1 := commitInRepo(t, cfg, path, "one")
			gittest.WriteRef(t, cfg, path, "refs/heads/d", c1)

			names := []string{"refs/heads/d", "refs/heads/d/e"}
			refs := map[string]corestore.Target{
				"refs/heads/d":   "",
				"refs/heads/d/e": corestore.Target(c1.String()),
			}

			err := applyTransaction(ctx, repo, names, refs)
			require.Error(t, err, "one transaction must be refused")
			require.True(t, isFileDirectoryConflict(err), "error: %v", err)

			require.NoError(t, applyNames(ctx, repo, names, refs))

			out := gittest.Exec(t, cfg, "-C", path, "for-each-ref", "--format=%(refname)", "refs/heads")
			require.Equal(t, "refs/heads/d/e", strings.TrimSpace(string(out)))
		})
	}
}

// newInternalRepo creates an empty repository with the files or the reftable
// backend, as the external test helpers do.
func newInternalRepo(t *testing.T, ctx context.Context, cfg config.Cfg, reftable bool) (*localrepo.Repo, string) {
	t.Helper()

	if !reftable {
		protoRepo, path := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
			ObjectFormat:           "sha1",
			SkipCreationViaService: true,
		})
		return localrepo.NewTestRepo(t, cfg, protoRepo), path
	}

	storagePath := cfg.Storages[0].Path
	parent, err := os.MkdirTemp(storagePath, "reftable-")
	require.NoError(t, err)
	path := filepath.Join(parent, "repo.git")
	require.NoError(t, InitBare(ctx, gittest.NewCommandFactory(t, cfg), path, "sha1"))

	relativePath, err := filepath.Rel(storagePath, path)
	require.NoError(t, err)
	protoRepo := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: relativePath}

	return localrepo.NewTestRepo(t, cfg, protoRepo), path
}

// commitInRepo writes a commit with one file in the repository at path. The
// commit is not referenced.
func commitInRepo(t *testing.T, cfg config.Cfg, path, content string) git.ObjectID {
	t.Helper()

	blob := gittest.WriteBlob(t, cfg, path, []byte(content))

	mktree := gittest.NewCommand(t, cfg, "-C", path, "mktree")
	mktree.Stdin = strings.NewReader(fmt.Sprintf("100644 blob %s\tfile\n", blob))
	tree, err := mktree.Output()
	require.NoError(t, err)

	out, err := gittest.NewCommand(t, cfg, "-C", path, "commit-tree", strings.TrimSpace(string(tree)), "-m", content).Output()
	require.NoError(t, err)

	return git.ObjectID(strings.TrimSpace(string(out)))
}
