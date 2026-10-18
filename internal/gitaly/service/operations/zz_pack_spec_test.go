package operations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// Rule: every loose object of the quarantine is packed into one pack, and the loose files are removed.
func TestSpecPackPacksAllLooseObjects(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	_, repo, objectDir := newQuarantine(t, ctx, cfg, repoProto, repoPath)

	contents := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	var ids []git.ObjectID
	for _, c := range contents {
		id, err := repo.WriteBlob(ctx, strings.NewReader(c), localrepo.WriteBlobConfig{})
		require.NoError(t, err)
		ids = append(ids, id)
	}

	n, err := packAttemptObjects(ctx, repo, objectDir)
	require.NoError(t, err)
	require.Equal(t, 5, n)
	require.Empty(t, looseObjects(t, objectDir))
	require.Len(t, packIndexes(t, objectDir), 1)

	for i, id := range ids {
		data, err := repo.ReadObject(ctx, id)
		require.NoError(t, err)
		require.Equal(t, contents[i], string(data))
	}
}

// Rule: a quarantine holding no loose object gives 0, and nothing is packed.
func TestSpecPackEmptyQuarantineIsNoOp(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	_, repo, objectDir := newQuarantine(t, ctx, cfg, repoProto, repoPath)

	n, err := packAttemptObjects(ctx, repo, objectDir)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Empty(t, packIndexes(t, objectDir))
}

// Rule: once the loose objects are packed, a second call finds nothing to pack and keeps one index.
func TestSpecPackSecondCallIsNoOp(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	_, repo, objectDir := newQuarantine(t, ctx, cfg, repoProto, repoPath)

	for _, c := range []string{"first", "second"} {
		_, err := repo.WriteBlob(ctx, strings.NewReader(c), localrepo.WriteBlobConfig{})
		require.NoError(t, err)
	}

	n, err := packAttemptObjects(ctx, repo, objectDir)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	n, err = packAttemptObjects(ctx, repo, objectDir)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, packIndexes(t, objectDir), 1)
}

// Rule: a file in a fan-out directory that is not an object ID is left in place, and its fan-out directory is kept.
func TestSpecPackKeepsStrayFileAndFanoutDir(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	_, repo, objectDir := newQuarantine(t, ctx, cfg, repoProto, repoPath)

	id, err := repo.WriteBlob(ctx, strings.NewReader("stray case"), localrepo.WriteBlobConfig{})
	require.NoError(t, err)

	fan := string(id)[:2]
	stray := filepath.Join(objectDir, fan, "tmp_obj_x")
	require.NoError(t, os.WriteFile(stray, []byte("not an object"), 0o600))

	n, err := packAttemptObjects(ctx, repo, objectDir)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	require.FileExists(t, stray)
	require.DirExists(t, filepath.Join(objectDir, fan))
	require.NotContains(t, looseObjects(t, objectDir), string(id))
}

// Rule: when only objects were present, no fan-out directory is left in the object directory after packing.
func TestSpecPackRemovesEmptyFanoutDirs(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	_, repo, objectDir := newQuarantine(t, ctx, cfg, repoProto, repoPath)

	for _, c := range []string{"one", "two", "three"} {
		_, err := repo.WriteBlob(ctx, strings.NewReader(c), localrepo.WriteBlobConfig{})
		require.NoError(t, err)
	}

	_, err := packAttemptObjects(ctx, repo, objectDir)
	require.NoError(t, err)

	entries, err := os.ReadDir(objectDir)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, isFanoutName(e.Name()), "fan-out directory %q left behind", e.Name())
	}
}

// Rule: when git pack-objects fails, an error is returned and the loose files are left untouched.
func TestSpecPackFailureKeepsLooseObjects(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	_, repo, objectDir := newQuarantine(t, ctx, cfg, repoProto, repoPath)

	id, err := repo.WriteBlob(ctx, strings.NewReader("kept on failure"), localrepo.WriteBlobConfig{})
	require.NoError(t, err)

	packDir := filepath.Join(objectDir, "pack")
	require.NoError(t, os.MkdirAll(packDir, 0o700))
	require.NoError(t, os.Chmod(packDir, 0o500))
	t.Cleanup(func() {
		require.NoError(t, os.Chmod(packDir, 0o700))
	})

	_, err = packAttemptObjects(ctx, repo, objectDir)
	require.Error(t, err)
	require.Contains(t, looseObjects(t, objectDir), string(id))
}

// Rule: packed object IDs use the repository object format (64 hex characters for SHA-256, 40 for SHA-1),
// and the pack index lists each of them.
func TestSpecPackUsesObjectFormatIDs(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	_, repo, objectDir := newQuarantine(t, ctx, cfg, repoProto, repoPath)

	wantLen := 40
	if gittest.ObjectHashIsSHA256() {
		wantLen = 64
	}

	var ids []git.ObjectID
	for _, c := range []string{"sha check one", "sha check two"} {
		id, err := repo.WriteBlob(ctx, strings.NewReader(c), localrepo.WriteBlobConfig{})
		require.NoError(t, err)
		require.Len(t, string(id), wantLen)
		ids = append(ids, id)
	}

	n, err := packAttemptObjects(ctx, repo, objectDir)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	idxs := packIndexes(t, objectDir)
	require.Len(t, idxs, 1)
	out := gittest.Exec(t, cfg, "-C", repoPath, "verify-pack", "-v", idxs[0])
	for _, id := range ids {
		require.Contains(t, string(out), string(id))
	}
}
