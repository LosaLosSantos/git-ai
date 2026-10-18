package blobstore_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/blobstore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// testBucket is a bucket under test. dir is the root of a fileblob bucket and
// empty for memblob.
type testBucket struct {
	name string
	url  string
	dir  string
}

func testBuckets(t *testing.T) []testBucket {
	t.Helper()
	dir := t.TempDir()
	return []testBucket{
		{name: "file", url: "file://" + dir, dir: dir},
		{name: "mem", url: "mem://"},
	}
}

func openStore(t *testing.T, bucketURL string) *blobstore.Store {
	t.Helper()
	ctx := testhelper.Context(t)
	s, err := blobstore.Open(ctx, bucketURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func packKeyOf(content []byte) string {
	return corestore.NewPackKey(1, sha256.Sum256(content))
}

// failingReader fails the test when it is read.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func readAll(t *testing.T, s *blobstore.Store, key string) ([]byte, error) {
	t.Helper()
	ctx := testhelper.Context(t)
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { require.NoError(t, rc.Close()) }()
	return io.ReadAll(rc)
}

func TestPutGetRoundTrip(t *testing.T) {
	for _, tb := range testBuckets(t) {
		t.Run(tb.name, func(t *testing.T) {
			ctx := testhelper.Context(t)
			s := openStore(t, tb.url)

			for _, key := range []string{"plain.txt", "a/b/c/nested.txt", "x/y/z/w/v/deep.bin"} {
				require.NoError(t, s.Put(ctx, key, strings.NewReader("content of "+key)))
				data, err := readAll(t, s, key)
				require.NoError(t, err)
				require.Equal(t, "content of "+key, string(data))
			}

			content := []byte("pack bytes of a changeset")
			pk := packKeyOf(content)
			require.NoError(t, s.Put(ctx, pk, bytes.NewReader(content)))

			exists, err := s.Exists(ctx, pk)
			require.NoError(t, err)
			require.True(t, exists)

			data, err := readAll(t, s, pk)
			require.NoError(t, err)
			require.Equal(t, content, data)
		})
	}
}

func TestPutIsCreateOnly(t *testing.T) {
	for _, tb := range testBuckets(t) {
		t.Run(tb.name, func(t *testing.T) {
			ctx := testhelper.Context(t)
			s := openStore(t, tb.url)

			require.NoError(t, s.Put(ctx, "k/first", strings.NewReader("first")))
			// An existing key is success and its content is not read.
			require.NoError(t, s.Put(ctx, "k/first", failingReader{err: errors.New("must not be read")}))

			data, err := readAll(t, s, "k/first")
			require.NoError(t, err)
			require.Equal(t, "first", string(data))

			content := []byte("pack content, written twice")
			pk := packKeyOf(content)
			require.NoError(t, s.Put(ctx, pk, bytes.NewReader(content)))
			require.NoError(t, s.Put(ctx, pk, failingReader{err: errors.New("must not be read")}))
		})
	}
}

func TestPutConcurrentSameKey(t *testing.T) {
	for _, tb := range testBuckets(t) {
		t.Run(tb.name, func(t *testing.T) {
			ctx := testhelper.Context(t)
			s := openStore(t, tb.url)

			content := bytes.Repeat([]byte("0123456789abcdef"), 1<<14)
			pk := packKeyOf(content)

			var g errgroup.Group
			for range 16 {
				g.Go(func() error {
					return s.Put(ctx, pk, bytes.NewReader(content))
				})
			}
			require.NoError(t, g.Wait())

			data, err := readAll(t, s, pk)
			require.NoError(t, err)
			require.Equal(t, content, data)
		})
	}
}

func TestPutRejectsCorruptPack(t *testing.T) {
	for _, tb := range testBuckets(t) {
		t.Run(tb.name, func(t *testing.T) {
			ctx := testhelper.Context(t)
			s := openStore(t, tb.url)

			pk := packKeyOf([]byte("the real pack"))
			err := s.Put(ctx, pk, strings.NewReader("forged pack"))
			require.ErrorIs(t, err, corestore.ErrBlobCorrupt)

			// Nothing was committed: the key does not exist.
			exists, err := s.Exists(ctx, pk)
			require.NoError(t, err)
			require.False(t, exists)

			_, err = readAll(t, s, pk)
			require.ErrorIs(t, err, corestore.ErrBlobNotFound)
		})
	}
}

func TestPutReadErrorLeavesNoObject(t *testing.T) {
	for _, tb := range testBuckets(t) {
		t.Run(tb.name, func(t *testing.T) {
			ctx := testhelper.Context(t)
			s := openStore(t, tb.url)

			content := []byte("a pack that fails while uploading")
			pk := packKeyOf(content)
			readErr := errors.New("connection reset")
			err := s.Put(ctx, pk, io.MultiReader(bytes.NewReader(content[:5]), failingReader{err: readErr}))
			require.ErrorIs(t, err, readErr)

			exists, err := s.Exists(ctx, pk)
			require.NoError(t, err)
			require.False(t, exists)
		})
	}
}

func TestGetMissingKey(t *testing.T) {
	for _, tb := range testBuckets(t) {
		t.Run(tb.name, func(t *testing.T) {
			ctx := testhelper.Context(t)
			s := openStore(t, tb.url)

			_, err := s.Get(ctx, "does/not/exist")
			require.ErrorIs(t, err, corestore.ErrBlobNotFound)

			exists, err := s.Exists(ctx, "does/not/exist")
			require.NoError(t, err)
			require.False(t, exists)

			_, err = s.Get(ctx, corestore.NewPackKey(7, sha256.Sum256([]byte("absent"))))
			require.ErrorIs(t, err, corestore.ErrBlobNotFound)
		})
	}
}

func TestGetNonPackKeyIsNotVerified(t *testing.T) {
	for _, tb := range testBuckets(t) {
		t.Run(tb.name, func(t *testing.T) {
			ctx := testhelper.Context(t)
			s := openStore(t, tb.url)

			// Only pack keys are content-addressed: any other content is returned as stored.
			require.NoError(t, s.Put(ctx, "notes/any-content", strings.NewReader("arbitrary")))
			data, err := readAll(t, s, "notes/any-content")
			require.NoError(t, err)
			require.Equal(t, "arbitrary", string(data))
		})
	}
}

func TestGetRejectsCorruptPackOnDisk(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, "file://"+dir)
	ctx := testhelper.Context(t)

	content := []byte("the content that the key names")
	pk := packKeyOf(content)
	require.NoError(t, s.Put(ctx, pk, bytes.NewReader(content)))

	path := filepath.Join(dir, filepath.FromSlash(pk))
	flipped := bytes.Clone(content)
	flipped[0] ^= 0xff

	for _, tc := range []struct {
		name   string
		onDisk []byte
	}{
		{name: "flipped byte", onDisk: flipped},
		{name: "truncated", onDisk: content[:len(content)-1]},
		{name: "appended", onDisk: append(bytes.Clone(content), '!')},
		{name: "empty", onDisk: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, tc.onDisk, 0o600))

			_, err := readAll(t, s, pk)
			require.ErrorIs(t, err, corestore.ErrBlobCorrupt)
			require.NotErrorIs(t, err, io.EOF)
		})
	}

	// The intact content still verifies once restored.
	require.NoError(t, os.WriteFile(path, content, 0o600))
	data, err := readAll(t, s, pk)
	require.NoError(t, err)
	require.Equal(t, content, data)
}

func TestOpenFileCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "objects")
	openStore(t, "file://"+dir)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestOpenRejectsFileURLWithoutPath(t *testing.T) {
	_, err := blobstore.Open(testhelper.Context(t), "file://")
	require.Error(t, err)
}

func TestS3Bucket(t *testing.T) {
	bucketURL := os.Getenv("GRAFT_TEST_S3_URL")
	if bucketURL == "" {
		t.Skip("GRAFT_TEST_S3_URL is not set")
	}

	ctx := testhelper.Context(t)
	s := openStore(t, bucketURL)

	// Unique content keeps the test independent of earlier runs in the bucket.
	content := []byte(t.Name() + time.Now().UTC().Format(time.RFC3339Nano))
	pk := packKeyOf(content)
	require.NoError(t, s.Put(ctx, pk, bytes.NewReader(content)))
	require.NoError(t, s.Put(ctx, pk, failingReader{err: errors.New("must not be read")}))

	data, err := readAll(t, s, pk)
	require.NoError(t, err)
	require.Equal(t, content, data)
}
