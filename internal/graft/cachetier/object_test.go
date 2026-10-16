package cachetier

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestValidOID(t *testing.T) {
	t.Parallel()

	valid := gitObjectID("blob", []byte("x"), false)
	validSHA256 := gitObjectID("blob", []byte("x"), true)

	for name, tc := range map[string]struct {
		oid   string
		valid bool
	}{
		"sha1":           {oid: valid, valid: true},
		"sha256":         {oid: validSHA256, valid: true},
		"empty":          {oid: "", valid: false},
		"short":          {oid: valid[:39], valid: false},
		"uppercase":      {oid: "ABCDEF" + valid[6:], valid: false},
		"non hex":        {oid: "g" + valid[1:], valid: false},
		"wrong length":   {oid: valid + "0", valid: false},
		"path traversal": {oid: "../../../../../../../../etc/passwd" + valid[:10], valid: false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.valid, validOID(tc.oid))
		})
	}
}

func TestVerifyObject(t *testing.T) {
	t.Parallel()

	data := []byte("hello graft\n")

	for _, sha256Format := range []bool{false, true} {
		oid := gitObjectID("blob", data, sha256Format)
		obj := &object{typ: gitalypb.ObjectType_BLOB, data: data}

		require.NoError(t, verifyObject(oid, obj), "sha256=%v", sha256Format)

		tampered := &object{typ: gitalypb.ObjectType_BLOB, data: []byte("hello graft!\n")}
		require.ErrorIs(t, verifyObject(oid, tampered), errVerification, "sha256=%v", sha256Format)

		wrongType := &object{typ: gitalypb.ObjectType_TREE, data: data}
		require.ErrorIs(t, verifyObject(oid, wrongType), errVerification, "sha256=%v", sha256Format)
	}

	unknownType := &object{typ: gitalypb.ObjectType_UNKNOWN, data: data}
	require.ErrorIs(t, verifyObject(gitObjectID("blob", data, false), unknownType), errVerification)
}

func TestDiskStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := newDiskStore(dir, DefaultDiskMaxBytes)
	require.NoError(t, err)

	data := []byte("some blob content that is long enough to span chunks")
	oid := gitObjectID("blob", data, false)
	key := objectKey{storage: "default", relativePath: "repo.git", oid: oid}
	obj := &object{typ: gitalypb.ObjectType_BLOB, data: data}

	got, err := store.read(key, noReadLimit)
	require.NoError(t, err)
	require.Nil(t, got, "nothing is stored yet")

	require.NoError(t, store.write(key, obj))

	got, err = store.read(key, noReadLimit)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, gitalypb.ObjectType_BLOB, got.typ)
	require.Equal(t, data, got.data)

	t.Run("objects are isolated per repository", func(t *testing.T) {
		other := objectKey{storage: "default", relativePath: "other.git", oid: oid}
		got, err := store.read(other, noReadLimit)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("no temporary files are left behind", func(t *testing.T) {
		var tmp []string
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() && filepath.Base(path)[0] == '.' {
				tmp = append(tmp, path)
			}
			return nil
		})
		require.NoError(t, err)
		require.Empty(t, tmp)
	})

	t.Run("corrupted files are discarded and reported as missing", func(t *testing.T) {
		path := store.path(key)
		require.NoError(t, os.WriteFile(path, []byte("blob 3\nbad"), 0o644))

		got, err := store.read(key, noReadLimit)
		require.NoError(t, err)
		require.Nil(t, got)

		_, statErr := os.Stat(path)
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("a file with the wrong content is discarded", func(t *testing.T) {
		require.NoError(t, store.write(key, obj))
		path := store.path(key)
		require.NoError(t, os.WriteFile(path, []byte("blob 12\nhello WORLD!"), 0o644))

		got, err := store.read(key, noReadLimit)
		require.NoError(t, err)
		require.Nil(t, got)
	})
}

// TestDiskStore_CorruptSizeHeaderIsAMiss checks that the size in an entry's header is
// checked against the file before it is used: a damaged header must degrade into a
// cache miss, never into a panic or a huge allocation.
func TestDiskStore_CorruptSizeHeaderIsAMiss(t *testing.T) {
	t.Parallel()

	data := []byte("x")
	oid := gitObjectID("blob", data, false)
	key := objectKey{storage: "default", relativePath: "repo.git", oid: oid}

	for _, tc := range []struct {
		desc    string
		content string
	}{
		{desc: "size beyond any allocation", content: "blob 99999999999999999\nx"},
		{desc: "size of several gigabytes", content: "blob 4294967296\nx"},
		{desc: "size larger than the file", content: "blob 2\nx"},
		{desc: "size smaller than the file", content: "blob 0\nx"},
		{desc: "maximum int64 size", content: "blob 9223372036854775807\nx"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			store, err := newDiskStore(t.TempDir(), DefaultDiskMaxBytes)
			require.NoError(t, err)
			require.NoError(t, store.write(key, &object{typ: gitalypb.ObjectType_BLOB, data: data}))

			path := store.path(key)
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o644))

			got, err := store.read(key, noReadLimit)
			require.NoError(t, err)
			require.Nil(t, got)
			require.NoFileExists(t, path, "the damaged entry must be discarded")

			// The intact entry is accepted again once it is rewritten.
			require.NoError(t, store.write(key, &object{typ: gitalypb.ObjectType_BLOB, data: data}))
			got, err = store.read(key, noReadLimit)
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Equal(t, data, got.data)
		})
	}
}
