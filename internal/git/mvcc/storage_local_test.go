package mvcc_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func Test_localStorage_ReadManifestPointer(t *testing.T) {
	ctx := testhelper.Context(t)
	ctx = gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC)

	cfg := testcfg.Build(t)
	_, repoPath := gittest.CreateMVCCRepository(t, ctx, cfg)

	backend, err := gitcmd.DetectReferenceBackend(ctx, repoPath)
	require.NoError(t, err)
	require.Equal(t, git.ReferenceBackendMVCC, backend)

	repoDir := cfg.Storages[0].Path
	relativePath, ok := strings.CutPrefix(repoPath, repoDir)
	require.True(t, ok)

	rs := mvcc.NewLocalStorage(repoDir)
	pointer, err := rs.ReadManifestPointer(ctx, relativePath)
	require.NoError(t, err)
	require.NotEmpty(t, pointer.Hash)

	manifestReader, err := rs.ReadManifest(ctx, relativePath, pointer.Hash)
	require.NoError(t, err)

	manifestContent := bytes.Buffer{}
	_, err = io.Copy(&manifestContent, manifestReader)
	require.NoError(t, manifestReader.Close())

	require.NoError(t, err)
	require.Greater(t, manifestContent.Len(), 0)

	firstManifest, err := mvcc.ParseManifest(manifestContent.Bytes())
	require.NoError(t, err)
	require.Len(t, firstManifest.RefPaths(), 1)

	firstCommitOid := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch("main"),
		gittest.WithMessage("Olivier message"),
		gittest.WithTreeEntries(
			gittest.TreeEntry{
				Mode:    "100644",
				Path:    "README.md",
				Content: "This is a README file",
			},
		),
	)
	require.NotEmptyf(t, firstCommitOid, "firstCommitOid should not be empty")

	pointer, err = rs.ReadManifestPointer(ctx, relativePath)
	require.NoError(t, err)
	require.NotEmpty(t, pointer.Hash)

	manifestReader, err = rs.ReadManifest(ctx, relativePath, pointer.Hash)
	require.NoError(t, err)

	manifestContent.Reset()
	_, err = io.Copy(&manifestContent, manifestReader)
	require.NoError(t, err)
	require.NoError(t, manifestReader.Close())

	secondManifest, err := mvcc.ParseManifest(manifestContent.Bytes())
	require.NoError(t, err)
	require.Len(t, secondManifest.RefPaths(), 2)
}

func Test_localStorage_Commit(t *testing.T) {
	ctx := testhelper.Context(t)

	// LocalStorage.Commit only reads, compares and writes the manifest pointer file, so we
	// can drive it with placeholder hashes without a real repository.
	root := t.TempDir()
	const repoID = "repo"
	pointerPath := filepath.Join(root, repoID, "mvcc", "manifest")
	require.NoError(t, os.MkdirAll(filepath.Dir(pointerPath), mode.Directory))
	require.NoError(t, os.WriteFile(pointerPath, []byte("AAAA"), mode.File))

	rs := mvcc.NewLocalStorage(root)

	// A CAS whose base matches the current pointer advances it.
	require.NoError(t, rs.Commit(ctx, repoID, mvcc.ManifestPointer{Hash: "AAAA"}, "BBBB"))
	pointer, err := rs.ReadManifestPointer(ctx, repoID)
	require.NoError(t, err)
	require.Equal(t, "BBBB", pointer.Hash)

	// A CAS whose base no longer matches (a concurrent writer advanced it) conflicts and
	// leaves the pointer untouched.
	err = rs.Commit(ctx, repoID, mvcc.ManifestPointer{Hash: "AAAA"}, "CCCC")
	require.ErrorIs(t, err, mvcc.ErrManifestPointerConflict)
	pointer, err = rs.ReadManifestPointer(ctx, repoID)
	require.NoError(t, err)
	require.Equal(t, "BBBB", pointer.Hash)
}

func Test_localStorage_Commit_pointerReplacedBeforeLock(t *testing.T) {
	ctx := testhelper.Context(t)

	root := t.TempDir()
	const repoID = "repo"
	pointerPath := filepath.Join(root, repoID, "mvcc", "manifest")
	require.NoError(t, os.MkdirAll(filepath.Dir(pointerPath), mode.Directory))
	require.NoError(t, os.WriteFile(pointerPath, []byte("AAAA"), mode.File))

	rs := mvcc.NewLocalStorage(root)

	// A concurrent committer with the same base wins after this commit opened the pointer
	// and before it locks it. Locking then fails with "file concurrently modified", which is a
	// lost race like any other and must be reported as a conflict, not as an internal error.
	winner := mvcc.NewLocalStorage(root)
	rs.SetBeforePointerLock(func() {
		require.NoError(t, winner.Commit(ctx, repoID, mvcc.ManifestPointer{Hash: "AAAA"}, "BBBBBBBB"))
	})

	err := rs.Commit(ctx, repoID, mvcc.ManifestPointer{Hash: "AAAA"}, "CCCC")
	require.ErrorIs(t, err, mvcc.ErrManifestPointerConflict)

	pointer, err := rs.ReadManifestPointer(ctx, repoID)
	require.NoError(t, err)
	require.Equal(t, "BBBBBBBB", pointer.Hash)
}
