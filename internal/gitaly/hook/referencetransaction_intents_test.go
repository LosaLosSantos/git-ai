package hook

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// TestReferenceTransactionHook_recordsLandIntents checks that the prepared phase of an update that
// lands changes records their intents before git may commit it (doc/graft/PLAN11.md, 4.2), that an
// update of another reference records none, and that a failure to record them aborts the update.
func TestReferenceTransactionHook_recordsLandIntents(t *testing.T) {
	t.Parallel()

	land := changelog.Land{Original: strings.Repeat("a", 40), Landed: strings.Repeat("b", 40)}

	// prepare runs the prepared phase of an update of reference whose payload lands land on main,
	// in a new repository, and returns the repository's path and the error of the hook.
	prepare := func(t *testing.T, cfg config.Cfg, reference string, setup func(repoPath string)) (string, error) {
		ctx := testhelper.Context(t)
		repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
		repoPath, err := config.NewLocator(cfg).GetRepoPath(ctx, repo)
		require.NoError(t, err)
		if setup != nil {
			setup(repoPath)
		}

		landCtx, err := changelog.WithLands(ctx, "refs/heads/main", []changelog.Land{land})
		require.NoError(t, err)
		env, err := gitcmd.NewHooksPayload(landCtx, cfg, repo, gittest.DefaultObjectHash, nil, nil, gitcmd.ReferenceTransactionHook, nil).Env()
		require.NoError(t, err)

		changes := fmt.Sprintf("%s %s %s\n", gittest.DefaultObjectHash.EmptyTreeOID, land.Landed, reference)
		return repoPath, newMembersTestManager(t, cfg).ReferenceTransactionHook(context.Background(), ReferenceTransactionPrepared, []string{env}, strings.NewReader(changes))
	}

	t.Run("an update of the branch of the lands", func(t *testing.T) {
		t.Parallel()

		repoPath, err := prepare(t, testcfg.Build(t), "refs/heads/main", nil)
		require.NoError(t, err)

		records, err := changelog.LookupLand(repoPath, "refs/heads/main", land.Original)
		require.NoError(t, err)
		require.Equal(t, []changelog.LandRecord{{Landed: land.Landed}}, records)
	})

	t.Run("an update of another reference", func(t *testing.T) {
		t.Parallel()

		repoPath, err := prepare(t, testcfg.Build(t), "refs/heads/other", nil)
		require.NoError(t, err)
		require.NoFileExists(t, filepath.Join(repoPath, "graft", "changelog.intents"))
	})

	t.Run("without a changelog", func(t *testing.T) {
		t.Parallel()

		cfg := testcfg.Build(t)
		cfg.Graft.Changelog.Disabled = true
		repoPath, err := prepare(t, cfg, "refs/heads/main", nil)
		require.NoError(t, err)
		require.NoFileExists(t, filepath.Join(repoPath, "graft", "changelog.intents"))
	})

	t.Run("a failure to record the intents aborts the update", func(t *testing.T) {
		t.Parallel()

		// The intents file cannot be opened for writing: a directory is in its place.
		_, err := prepare(t, testcfg.Build(t), "refs/heads/main", func(repoPath string) {
			require.NoError(t, os.MkdirAll(filepath.Join(repoPath, "graft", "changelog.intents"), 0o755))
		})
		require.ErrorContains(t, err, "recording the land intents")
	})
}
