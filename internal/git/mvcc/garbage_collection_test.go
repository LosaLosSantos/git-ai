package mvcc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// manifestNameFor turns a readable label into a name shaped like the content-addressed one a
// manifest is written under. Anything else is not recognized as a manifest at all.
func manifestNameFor(label string) string {
	sum := sha256.Sum256([]byte(label))

	return hex.EncodeToString(sum[:])
}

// counts strips the timings from a result, so it can be compared for what it did.
func counts(result ManifestPruningResult) ManifestPruningResult {
	result.AcquireDuration = 0
	result.LockDuration = 0

	return result
}

// reftable returns a well-formed reftable path built from a repeated character.
func reftable(char string) string {
	return "refs/" + strings.Repeat(char, 64) + ".ref"
}

// numberedReftable returns a distinct well-formed reftable path per index.
func numberedReftable(i int) string {
	return fmt.Sprintf("refs/%064x.ref", i)
}

// mvccRepo is a repository with an MVCC directory that tests populate directly.
type mvccRepo struct {
	tb       testing.TB
	path     string
	mvccPath string
}

func newMVCCRepo(tb testing.TB) mvccRepo {
	tb.Helper()

	// The repository path is also the lock identity, so it has to be unique per test to keep
	// parallel tests from contending with one another.
	path := testhelper.TempDir(tb)
	repo := mvccRepo{tb: tb, path: path, mvccPath: filepath.Join(path, mvccCacheDir)}

	for _, dir := range []string{manifestDirectory, "refs", "pack"} {
		require.NoError(tb, os.MkdirAll(filepath.Join(repo.mvccPath, dir), mode.Directory))
	}

	return repo
}

// writeManifest writes a manifest referencing the given paths under the name the label maps to,
// and returns that name.
func (r mvccRepo) writeManifest(label string, age time.Duration, paths ...string) string {
	r.tb.Helper()

	const width = 80
	chunks := []chunkSpec{{chunkIDPath, pathChunkData(width, paths...)}}
	r.writeFile(r.manifestPath(label), buildManifest(r.tb, manifestVersion, width, hashAlgoSHA1, chunks), age)

	return manifestNameFor(label)
}

// manifestPath returns the path of the manifest a label maps to, relative to the MVCC directory.
func (r mvccRepo) manifestPath(label string) string {
	return manifestDirectory + "/" + manifestNameFor(label)
}

// writeFile writes a file below the MVCC directory with the given age.
func (r mvccRepo) writeFile(relativePath string, content []byte, age time.Duration) {
	r.tb.Helper()

	path := filepath.Join(r.mvccPath, relativePath)

	// Files are written read-only, so writing one again means replacing it.
	require.NoError(r.tb, os.RemoveAll(path))
	require.NoError(r.tb, os.WriteFile(path, content, mode.File))

	modTime := time.Now().Add(-age)
	require.NoError(r.tb, os.Chtimes(path, modTime, modTime))
}

// writeArtifact writes an artifact with the given age and returns its path.
func (r mvccRepo) writeArtifact(relativePath string, age time.Duration) string {
	r.tb.Helper()

	r.writeFile(relativePath, []byte("artifact"), age)

	return relativePath
}

// writeArtifactUnder writes an artifact in a subdirectory of the MVCC directory, creating the
// directory if needed, and returns its path.
func (r mvccRepo) writeArtifactUnder(directory, name string, age time.Duration) string {
	r.tb.Helper()

	require.NoError(r.tb, os.MkdirAll(filepath.Join(r.mvccPath, directory), mode.Directory))

	return r.writeArtifact(directory+"/"+name, age)
}

// age sets the modification time of an entry below the MVCC directory.
func (r mvccRepo) age(relativePath string, age time.Duration) {
	r.tb.Helper()

	modTime := time.Now().Add(-age)
	require.NoError(r.tb, os.Chtimes(filepath.Join(r.mvccPath, relativePath), modTime, modTime))
}

// setPointer points the repository at the given manifest.
func (r mvccRepo) setPointer(name string) {
	r.tb.Helper()

	r.writeFile(manifestPointer, []byte(name), 0)
}

func (r mvccRepo) requireExists(relativePath string, expected bool) {
	r.tb.Helper()

	_, err := os.Stat(filepath.Join(r.mvccPath, relativePath))
	if expected {
		require.NoError(r.tb, err, "%s should still exist", relativePath)
	} else {
		require.ErrorIs(r.tb, err, os.ErrNotExist, "%s should have been removed", relativePath)
	}
}
