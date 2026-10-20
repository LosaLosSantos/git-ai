package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
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
	prepare := func(t *testing.T, cfg config.Cfg, reference string, setup func(repoPath string), startCompaction func(func())) (string, error) {
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
		manager := newMembersTestManager(t, cfg)
		manager.startLandIntentsCompaction = startCompaction
		return repoPath, manager.ReferenceTransactionHook(context.Background(), ReferenceTransactionPrepared, []string{env}, strings.NewReader(changes))
	}

	t.Run("an update of the branch of the lands", func(t *testing.T) {
		t.Parallel()

		repoPath, err := prepare(t, testcfg.Build(t), "refs/heads/main", nil, nil)
		require.NoError(t, err)

		records, err := changelog.LookupLand(repoPath, "refs/heads/main", land.Original)
		require.NoError(t, err)
		require.Equal(t, []changelog.LandRecord{{Landed: land.Landed}}, records)
	})

	t.Run("an update of another reference", func(t *testing.T) {
		t.Parallel()

		repoPath, err := prepare(t, testcfg.Build(t), "refs/heads/other", nil, nil)
		require.NoError(t, err)
		require.NoFileExists(t, filepath.Join(repoPath, "graft", "changelog.intents"))
	})

	t.Run("without a changelog", func(t *testing.T) {
		t.Parallel()

		cfg := testcfg.Build(t)
		cfg.Graft.Changelog.Disabled = true
		repoPath, err := prepare(t, cfg, "refs/heads/main", nil, nil)
		require.NoError(t, err)
		require.NoFileExists(t, filepath.Join(repoPath, "graft", "changelog.intents"))
	})

	t.Run("a grown intents file is compacted after the update", func(t *testing.T) {
		t.Parallel()

		// The file holds enough intents whose updates the changelog confirms: the next intent asks
		// for a compaction, which the hook starts after the update.
		var lines strings.Builder
		var records []changelog.Record
		for i := 0; i < 600; i++ {
			confirmed := changelog.Land{Original: fmt.Sprintf("%040x", i+1), Landed: fmt.Sprintf("%040x", i+1)}
			line, err := json.Marshal(map[string]any{"ref": []byte("refs/heads/main"), "original": confirmed.Original, "landed": confirmed.Landed, "invocation": fmt.Sprint("inv-", i)})
			require.NoError(t, err)
			lines.Write(line)
			lines.WriteByte('\n')
			records = append(records, changelog.Record{
				Meta:    changelog.Meta{Operation: "land", LandReference: "refs/heads/main", LandInvocation: fmt.Sprint("inv-", i), Lands: []changelog.Land{confirmed}},
				Updates: []changelog.Update{{Reference: "refs/heads/main", OldOID: gittest.DefaultObjectHash.ZeroOID.String(), NewOID: confirmed.Landed}},
			})
		}

		started := 0
		repoPath, err := prepare(t, testcfg.Build(t), "refs/heads/main", func(repoPath string) {
			require.NoError(t, os.MkdirAll(filepath.Join(repoPath, "graft"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(repoPath, "graft", "changelog.intents"), []byte(lines.String()), 0o644))
			_, err := changelog.AppendGroup(repoPath, records)
			require.NoError(t, err)
		}, func(compact func()) {
			started++
			compact()
		})
		require.NoError(t, err)
		require.Equal(t, 1, started)

		info, err := os.Stat(filepath.Join(repoPath, "graft", "changelog.intents"))
		require.NoError(t, err)
		require.Less(t, info.Size(), int64(len(lines.String())), "the confirmed intents went")
		records2, err := changelog.LookupLand(repoPath, "refs/heads/main", land.Original)
		require.NoError(t, err)
		require.Equal(t, []changelog.LandRecord{{Landed: land.Landed}}, records2, "the intent of the update stays")
	})

	t.Run("a failed compaction is counted", func(t *testing.T) {
		t.Parallel()

		// The intents file asks for a compaction, which fails: the land index cannot be opened, a
		// directory is in its place.
		var lines strings.Builder
		for i := 0; i < 600; i++ {
			line, err := json.Marshal(map[string]any{"ref": []byte("refs/heads/main"), "original": fmt.Sprintf("%040x", i+1), "landed": fmt.Sprintf("%040x", i+1), "invocation": fmt.Sprint("inv-", i)})
			require.NoError(t, err)
			lines.Write(line)
			lines.WriteByte('\n')
		}

		failures := testutil.ToFloat64(landIntentsCompactionFailuresTotal)
		_, err := prepare(t, testcfg.Build(t), "refs/heads/main", func(repoPath string) {
			require.NoError(t, os.MkdirAll(filepath.Join(repoPath, "graft", "changelog.lands"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(repoPath, "graft", "changelog.intents"), []byte(lines.String()), 0o644))
			_, err := changelog.Append(repoPath, changelog.Meta{Operation: "push"}, []changelog.Update{{Reference: "refs/heads/other", OldOID: gittest.DefaultObjectHash.ZeroOID.String(), NewOID: strings.Repeat("c", 40)}})
			require.NoError(t, err)
		}, func(compact func()) { compact() })
		require.NoError(t, err, "the update does not wait for its compaction, nor fail with it")
		require.GreaterOrEqual(t, testutil.ToFloat64(landIntentsCompactionFailuresTotal), failures+1)
	})

	t.Run("a failure to record the intents aborts the update", func(t *testing.T) {
		t.Parallel()

		// The intents file cannot be opened for writing: a directory is in its place.
		_, err := prepare(t, testcfg.Build(t), "refs/heads/main", func(repoPath string) {
			require.NoError(t, os.MkdirAll(filepath.Join(repoPath, "graft", "changelog.intents"), 0o755))
		}, nil)
		require.ErrorContains(t, err, "recording the land intents")
	})
}
