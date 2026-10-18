package blobstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// openFileStoreForTest opens a file bucket and installs syncHook for the test.
func openFileStoreForTest(t *testing.T, dir string, hook func(path string, isDir bool) error) *Store {
	t.Helper()
	s, err := Open(testhelper.Context(t), "file://"+dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	syncHook = hook
	t.Cleanup(func() { syncHook = nil })
	return s
}

func TestPutSyncsFileBeforeCommitAndSyncsDirectories(t *testing.T) {
	dir := t.TempDir()
	content := []byte("pack content, synced before the commit")
	key := corestore.NewPackKey(1, sha256.Sum256(content))
	final := filepath.Join(dir, filepath.FromSlash(key))

	var fileSyncs int
	var dirSyncs []string
	s := openFileStoreForTest(t, dir, func(path string, isDir bool) error {
		if isDir {
			dirSyncs = append(dirSyncs, path)
			return nil
		}
		fileSyncs++
		// The complete content is in the temporary file, and the key is not
		// committed yet.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(data, content) {
			return errors.New("the synced content is incomplete")
		}
		if _, err := os.Stat(final); !os.IsNotExist(err) {
			return errors.New("the key was committed before its content was synced")
		}
		return nil
	})

	require.NoError(t, s.Put(testhelper.Context(t), key, bytes.NewReader(content)))

	require.Equal(t, 1, fileSyncs)
	// The directories from the key's parent up to the bucket root are synced,
	// the parent first and the root last.
	require.NotEmpty(t, dirSyncs)
	require.Equal(t, filepath.Dir(final), dirSyncs[0])
	require.Equal(t, dir, dirSyncs[len(dirSyncs)-1])

	data, err := os.ReadFile(final)
	require.NoError(t, err)
	require.Equal(t, content, data)
}

func TestPutFailedFileSyncLeavesNoObject(t *testing.T) {
	dir := t.TempDir()
	content := []byte("pack content whose sync fails")
	key := corestore.NewPackKey(1, sha256.Sum256(content))
	final := filepath.Join(dir, filepath.FromSlash(key))

	syncErr := errors.New("disk on fire")
	s := openFileStoreForTest(t, dir, func(_ string, isDir bool) error {
		if isDir {
			return nil
		}
		return syncErr
	})

	err := s.Put(testhelper.Context(t), key, bytes.NewReader(content))
	require.ErrorIs(t, err, syncErr)

	_, statErr := os.Stat(final)
	require.True(t, os.IsNotExist(statErr), "the key must not be committed")

	// The temporary file is discarded too: only directories are left behind.
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return errors.New("left behind: " + path)
		}
		return nil
	}))
}

func TestPutWithoutFileSyncHookSucceeds(t *testing.T) {
	// The fsync steps run for real on a file bucket: the write is observable
	// afterwards. A second Put of the key is refused and leaves the object as it
	// is (PLAN7, PACK OWNERSHIP RULE 2: Put is strictly create-only; before phase 7
	// a repeated Put was a successful no-op).
	dir := t.TempDir()
	s := openFileStoreForTest(t, dir, nil)
	ctx := testhelper.Context(t)

	content := []byte("durable content")
	key := corestore.NewPackKey(2, sha256.Sum256(content))
	require.NoError(t, s.Put(ctx, key, bytes.NewReader(content)))
	require.ErrorIs(t, s.Put(ctx, key, bytes.NewReader(content)), corestore.ErrBlobExists)

	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
	require.NoError(t, err)
	require.Equal(t, content, data)
}

func TestPutMemBucketWithoutFileSync(t *testing.T) {
	// mem buckets have no *os.File: Put must not require one and must not sync.
	s, err := Open(testhelper.Context(t), "mem://")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	syncHook = func(string, bool) error { return errors.New("no sync expected for mem buckets") }
	t.Cleanup(func() { syncHook = nil })

	content := []byte("mem content")
	key := corestore.NewPackKey(3, sha256.Sum256(content))
	require.NoError(t, s.Put(testhelper.Context(t), key, bytes.NewReader(content)))
}

func TestOpenTrimsSchemePrefix(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prefixed")
	s, err := Open(testhelper.Context(t), "service+file://"+dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	require.Equal(t, dir, s.root, "the prefix is trimmed and the file driver is used")
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestWithS3DefaultChecksumCalculation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "custom endpoint without checksum setting",
			input:    "s3://bucket?endpoint=minio.example.com",
			expected: "s3://bucket?endpoint=minio.example.com&request_checksum_calculation=when_required",
		},
		{
			name:     "explicit setting is kept",
			input:    "s3://bucket?endpoint=minio.example.com&request_checksum_calculation=when_supported",
			expected: "s3://bucket?endpoint=minio.example.com&request_checksum_calculation=when_supported",
		},
		{
			name:     "no custom endpoint is left alone",
			input:    "s3://bucket?region=us-east-1",
			expected: "s3://bucket?region=us-east-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := url.Parse(tc.input)
			require.NoError(t, err)
			require.Equal(t, tc.expected, withS3DefaultChecksumCalculation(parsed))
		})
	}
}
