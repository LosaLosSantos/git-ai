package mvcc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// realManifestName is the content-addressed name (hex SHA-256 of the payload)
// of testdata/real-manifest.bin, a manifest body produced by real Git via
// `git init --bare --ref-format=mvcc`.
// It only contains references yet, no objects.
const realManifestName = "3f68130d6fd108b66536bee984ea7f575e3d1f9706982913339f42a9caf11a0b"

// chunkSpec is a single chunk (id plus its raw payload) fed to buildManifest.
type chunkSpec struct {
	id   uint32
	data []byte
}

func TestParseManifest_realBinaryManifest(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("testdata", "manifest.bin"))
	require.NoError(t, err)

	m, err := ParseManifest(body)
	require.NoError(t, err)

	expectedRefs := []string{
		"refs/e546fa0090bfdaa4a4d632cf241dfb6f411cd36e5c86d4274fe9dd9f85833a70.ref",
	}

	expectedRefHashes := []string{
		"e546fa0090bfdaa4a4d632cf241dfb6f411cd36e5c86d4274fe9dd9f85833a70",
	}

	expectedObjs := []string{
		"pack/cec0880cf4323043aeb2268a3eaefdbd0e006c60.idx",
		"pack/cec0880cf4323043aeb2268a3eaefdbd0e006c60.pack",
		"pack/cec0880cf4323043aeb2268a3eaefdbd0e006c60.rev",
	}

	expectedPackHashes := []string{
		"cec0880cf4323043aeb2268a3eaefdbd0e006c60",
	}

	require.Equal(t, byte(hashAlgoSHA1), m.HashAlgo)
	require.Equal(t, expectedRefs, m.RefPaths())
	require.Equal(t, expectedRefHashes, m.ReftableHashes())
	require.Equal(t, expectedObjs, m.ObjPaths())
	require.Equal(t, expectedPackHashes, m.PackHashes())

	assertManifestSelfNamed(t, body, realManifestName)
}

func TestParseManifest_Full(t *testing.T) {
	t.Parallel()

	original := &Manifest{
		Paths: []string{
			"pack/aaaa.idx",
			"pack/aaaa.pack",
			"pack/aaaa.rev",
			"refs/bbbb.ref",
			"refs/cccc.ref",
		},
		HashAlgo:      hashAlgoSHA1,
		objStart:      0,
		objCount:      3,
		refStackOrder: []string{"refs/cccc.ref", "refs/bbbb.ref"},
	}

	body, sha, err := SerializeManifest(original)
	require.NoError(t, err)
	require.NotEmpty(t, sha)

	// Verify the trailer is the SHA-256 of the payload.
	payloadEnd := len(body) - trailerSize
	want := sha256.Sum256(body[:payloadEnd])
	require.Equal(t, want[:], body[payloadEnd:])

	parsed, err := ParseManifest(body)
	require.NoError(t, err)
	require.Equal(t, original.Paths, parsed.Paths)
	require.Equal(t, byte(hashAlgoSHA1), parsed.HashAlgo)
	require.Equal(t, []string{"aaaa"}, parsed.PackHashes())
	// ReftableHashes follows REFS chunk stack order (cccc before bbbb).
	require.Equal(t, []string{"cccc", "bbbb"}, parsed.ReftableHashes())

	// Verify OBJS range and reftable stack order are preserved.
	require.Equal(t, original.objStart, parsed.objStart)
	require.Equal(t, original.objCount, parsed.objCount)
	require.Equal(t, original.refStackOrder, parsed.refStackOrder)
}

func TestParseManifest_Empty(t *testing.T) {
	t.Parallel()

	body, sha, err := SerializeManifest(&Manifest{HashAlgo: hashAlgoSHA1})
	require.NoError(t, err)
	require.NotEmpty(t, sha)

	parsed, err := ParseManifest(body)
	require.NoError(t, err)
	require.Empty(t, parsed.Paths)
	require.Empty(t, parsed.PackHashes())
	require.Empty(t, parsed.ReftableHashes())
}

func TestParseManifest_NilBody(t *testing.T) {
	t.Parallel()

	_, err := ParseManifest(nil)
	require.Error(t, err)
}

func TestParseManifest_ChecksumMismatch(t *testing.T) {
	t.Parallel()

	body := validManifest(t)
	// Flip a payload byte without re-signing, so the trailer no longer matches.
	body[0] ^= 0xFF
	_, err := ParseManifest(body)
	require.ErrorContains(t, err, "checksum mismatch")
}

func TestParseManifest_HeaderValidations(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		mutate  func([]byte)
		wantErr string
	}{
		{"bad magic", func(b []byte) { b[0] = 0 }, "missing magic number"},
		{"unsupported version", func(b []byte) { b[4] = 99 }, "unsupported manifest version"},
		{"zero path width", func(b []byte) { b[6] = 0 }, "PATH record width"},
		{"path width above cap", func(b []byte) { b[6] = maxPathWidth + 1 }, "PATH record width"},
		{"unsupported hash algo", func(b []byte) { b[7] = 9 }, "unsupported hash algorithm"},
		{"too short for declared TOC", func(b []byte) { b[5] = 255 }, "too short for"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := validManifest(t)
			tc.mutate(body)
			resign(body)

			_, err := ParseManifest(body)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestParseManifest_zeroChunks(t *testing.T) {
	t.Parallel()

	_, err := ParseManifest(buildManifest(t, manifestVersion, 16, hashAlgoSHA1, nil))
	require.ErrorContains(t, err, "at least one chunk")
}

func TestParseManifest_missingPathChunk(t *testing.T) {
	t.Parallel()

	body := buildManifest(t, manifestVersion, 16, hashAlgoSHA1, []chunkSpec{
		{chunkIDObjs, objsChunkData(0, 0)},
		{chunkIDRefs, refsChunkData()},
	})
	_, err := ParseManifest(body)
	require.ErrorContains(t, err, "no PATH chunks found")
}

func TestParseManifest_optionalChunksAbsent(t *testing.T) {
	t.Parallel()

	t.Run("OBJS absent", func(t *testing.T) {
		t.Parallel()

		body := buildManifest(t, manifestVersion, 16, hashAlgoSHA1, []chunkSpec{
			{chunkIDPath, pathChunkData(16, "refs/aaaa.ref")},
			{chunkIDRefs, refsChunkData(0)},
		})
		m, err := ParseManifest(body)
		require.NoError(t, err)
		require.Empty(t, m.ObjPaths())
		require.Equal(t, []string{"refs/aaaa.ref"}, m.RefPaths())
	})

	t.Run("REFS absent", func(t *testing.T) {
		t.Parallel()

		body := buildManifest(t, manifestVersion, 16, hashAlgoSHA1, []chunkSpec{
			{chunkIDPath, pathChunkData(16, "pack/aaaa.pack")},
			{chunkIDObjs, objsChunkData(0, 1)},
		})
		m, err := ParseManifest(body)
		require.NoError(t, err)
		require.Equal(t, []string{"pack/aaaa.pack"}, m.ObjPaths())
		require.Empty(t, m.RefPaths())
	})

	t.Run("only PATH", func(t *testing.T) {
		t.Parallel()

		body := pathOnlyManifest(t, 16, "extra/thing.dat")
		m, err := ParseManifest(body)
		require.NoError(t, err)
		require.Equal(t, []string{"extra/thing.dat"}, m.Paths)
		require.Empty(t, m.ObjPaths())
		require.Empty(t, m.RefPaths())
	})
}

func TestParseManifest_duplicateChunks(t *testing.T) {
	t.Parallel()

	pathData := pathChunkData(16, "pack/aaaa.pack")
	for _, tc := range []struct {
		name    string
		chunks  []chunkSpec
		wantErr string
	}{
		{
			name:    "duplicate PATH",
			chunks:  []chunkSpec{{chunkIDPath, pathData}, {chunkIDPath, pathData}},
			wantErr: "multiple PATH chunks found",
		},
		{
			name:    "duplicate OBJS",
			chunks:  []chunkSpec{{chunkIDPath, pathData}, {chunkIDObjs, objsChunkData(0, 0)}, {chunkIDObjs, objsChunkData(0, 0)}},
			wantErr: "multiple OBJS chunks found",
		},
		{
			name:    "duplicate REFS",
			chunks:  []chunkSpec{{chunkIDPath, pathData}, {chunkIDRefs, refsChunkData()}, {chunkIDRefs, refsChunkData()}},
			wantErr: "multiple REFS chunks found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseManifest(buildManifest(t, manifestVersion, 16, hashAlgoSHA1, tc.chunks))
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestParseManifest_invalidPath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"leading slash", "/pack/aaaa.pack"},
		{"trailing slash", "pack/aaaa/"},
		{"empty component", "pack//aaaa.pack"},
		{"dot-dot component", "pack/../aaaa"},
		{"disallowed character", "PACK/aaaa.pack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseManifest(pathOnlyManifest(t, 16, tc.path))
			require.ErrorContains(t, err, "invalid path")
		})
	}
}

func TestManifest_convenienceMethods(t *testing.T) {
	t.Parallel()

	m := &Manifest{
		Paths: []string{
			"pack/abc.idx",
			"pack/abc.pack",
			"pack/abc.rev",
			"refs/def.ref",
			"refs/ghi.ref",
		},
		objStart:      0,
		objCount:      3,
		refStackOrder: []string{"refs/def.ref", "refs/ghi.ref"},
	}

	require.Equal(t, []string{"abc"}, m.PackHashes())
	require.Equal(t, []string{"def", "ghi"}, m.ReftableHashes())
	require.Equal(t, []string{"pack/abc.idx", "pack/abc.pack", "pack/abc.rev"}, m.ObjPaths())
	require.Equal(t, []string{"refs/def.ref", "refs/ghi.ref"}, m.RefPaths())
}

// craftedOffsetManifests returns manifests with a valid checksum whose TOC offsets are 64-bit
// values that do not fit in a non-negative int. The TOC row i holds the start of chunk i, and
// the sentinel row (numChunks) holds the end of the last chunk. Some of these offsets are rejected
// with an out-of-bounds error by any correct check. The 2^63 plus real offset variants are the
// dangerous ones: when the chunk size is computed as a wrapped difference and converted to int
// before the bounds are checked, the size becomes a negative multiple of the width and make()
// panics with "len out of range".
func craftedOffsetManifests(tb testing.TB) map[string][]byte {
	tb.Helper()

	const width = 16
	chunks := []chunkSpec{
		{chunkIDPath, pathChunkData(width, "pack/aaaa.pack")},
		{chunkIDObjs, objsChunkData(0, 1)},
		{chunkIDRefs, refsChunkData()},
	}
	sentinelRow := len(chunks)

	setTOCOffset := func(body []byte, row int, offset uint64) []byte {
		start := headerSize + row*tocChunkSize + 4
		binary.BigEndian.PutUint64(body[start:start+8], offset)
		resign(body)
		return body
	}

	return map[string][]byte{
		"toc_start_2pow63": setTOCOffset(
			buildManifest(tb, manifestVersion, width, hashAlgoSHA1, chunks), 0, 1<<63),
		"toc_start_max_uint64": setTOCOffset(
			buildManifest(tb, manifestVersion, width, hashAlgoSHA1, chunks), 0, math.MaxUint64),
		"toc_next_start_2pow63": setTOCOffset(
			buildManifest(tb, manifestVersion, width, hashAlgoSHA1, chunks), 1, 1<<63),
		"sentinel_2pow63_plus_one": setTOCOffset(
			buildManifest(tb, manifestVersion, width, hashAlgoSHA1, chunks), sentinelRow, 1<<63+1),
		// The real PATH start is 56 (header plus four TOC rows) and its end is 72. Adding 2^63 to
		// either value gives the wrapped chunk size that used to panic in make().
		"toc_start_2pow63_plus_real_start": setTOCOffset(
			buildManifest(tb, manifestVersion, width, hashAlgoSHA1, chunks), 0, 1<<63+56),
		"toc_start_2pow63_plus_real_end": setTOCOffset(
			buildManifest(tb, manifestVersion, width, hashAlgoSHA1, chunks), 0, 1<<63+72),
	}
}

// TestParseManifest_offsetsBeyondInt63 is the regression test for the panic on crafted TOC
// offsets. Each body must be rejected with an error, not a panic.
func TestParseManifest_offsetsBeyondInt63(t *testing.T) {
	t.Parallel()

	for name, body := range craftedOffsetManifests(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var err error
			require.NotPanics(t, func() {
				_, err = ParseManifest(body)
			})
			require.ErrorContains(t, err, "out-of-bounds offsets")
		})
	}
}

// FuzzParseManifest checks that ParseManifest never panics on arbitrary input. The crafted
// offset manifests are seeds, and the corpus under testdata/fuzz/FuzzParseManifest holds them as
// regression inputs.
func FuzzParseManifest(f *testing.F) {
	realBody, err := os.ReadFile(filepath.Join("testdata", "manifest.bin"))
	require.NoError(f, err)
	f.Add(realBody)

	f.Add(buildManifest(f, manifestVersion, 16, hashAlgoSHA1, []chunkSpec{
		{chunkIDPath, pathChunkData(16, "pack/aaaa.pack")},
		{chunkIDObjs, objsChunkData(0, 1)},
	}))

	for _, body := range craftedOffsetManifests(f) {
		f.Add(body)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseManifest(data)
	})
}

// manifestNameOf returns the content-addressed name of a manifest body: the hex SHA-256 of its
// payload, which is the body without the trailer.
func manifestNameOf(body []byte) string {
	payloadEnd := len(body) - trailerSize
	sum := sha256.Sum256(body[:payloadEnd])
	return fmt.Sprintf("%x", sum)
}

func TestVerifyManifestName(t *testing.T) {
	t.Parallel()

	body := validManifest(t)
	name := manifestNameOf(body)

	require.NoError(t, verifyManifestName(body, name))

	require.ErrorContains(t, verifyManifestName(body, manifestNameOf(pathOnlyManifest(t, 16, "refs/aaaa.ref"))), "hashes to")
	require.ErrorContains(t, verifyManifestName([]byte("short"), name), "too short")
}

// TestLocalCache_prepareManifestFileVerifiesName checks that a manifest fetched from object
// storage is only written to the local cache when it hashes to the name it was requested with.
func TestLocalCache_prepareManifestFileVerifiesName(t *testing.T) {
	t.Parallel()

	const repositoryID = "@hashed/ab/cd/repository"
	body := validManifest(t)
	name := manifestNameOf(body)

	for _, tc := range []struct {
		desc     string
		stored   []byte
		wantErr  string
		wantFile bool
	}{
		{desc: "matching manifest", stored: body, wantFile: true},
		{desc: "other manifest under the name", stored: pathOnlyManifest(t, 16, "refs/aaaa.ref"), wantErr: "hashes to"},
		{desc: "corrupt payload under the name", stored: func() []byte {
			corrupt := bytes.Clone(body)
			corrupt[headerSize+tocChunkSize] ^= 0xFF
			return corrupt
		}(), wantErr: "hashes to"},
		{desc: "too short", stored: []byte("short"), wantErr: "too short"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			remoteRoot := t.TempDir()
			localRoot := t.TempDir()

			remoteDir := filepath.Join(remoteRoot, repositoryID, mvccCacheDir, manifestDirectory)
			require.NoError(t, os.MkdirAll(remoteDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(remoteDir, name), tc.stored, 0o644))

			localDir := filepath.Join(localRoot, repositoryID, mvccCacheDir, manifestDirectory)
			require.NoError(t, os.MkdirAll(localDir, 0o755))

			cache := NewLocalCache(NewLocalStorage(remoteRoot), t.TempDir(), localRoot, repositoryID, true)
			manifest, err := cache.prepareManifestFile(context.Background(), name)

			localPath := filepath.Join(localDir, name)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, manifest)
				_, statErr := os.Stat(localPath)
				require.ErrorIs(t, statErr, os.ErrNotExist, "a manifest that fails verification must not be cached")
				return
			}

			require.NoError(t, err)
			require.Equal(t, []string{"pack/aaaa.idx", "pack/aaaa.pack", "pack/aaaa.rev", "refs/bbbb.ref", "refs/cccc.ref"}, manifest.Paths)
			_, statErr := os.Stat(localPath)
			require.NoError(t, statErr)
		})
	}
}

// buildManifest assembles a manifest body from the given header fields and
// chunks and appends a valid SHA-256 trailer. It computes contiguous chunk
// offsets and a zero-ID sentinel but performs no validation, so it can produce
// deliberately malformed bodies for negative tests.
func buildManifest(tb testing.TB, version, pathWidth, hashAlgo byte, chunks []chunkSpec) []byte {
	tb.Helper()

	numChunks := len(chunks)
	tocSize := (numChunks + 1) * tocChunkSize
	dataStart := headerSize + tocSize

	offsets := make([]int, numChunks+1)
	off := dataStart
	for i, c := range chunks {
		offsets[i] = off
		off += len(c.data)
	}
	offsets[numChunks] = off // sentinel points at the end of chunk data

	buf := make([]byte, off+trailerSize)

	// Header: magic(4) + version(1) + num_chunks(1) + path_width(1) + hash_algo(1).
	binary.BigEndian.PutUint32(buf[0:4], manifestMagic)
	buf[4] = version
	buf[5] = byte(numChunks)
	buf[6] = pathWidth
	buf[7] = hashAlgo

	// Chunk lookup table plus trailing zero-ID sentinel.
	for i, c := range chunks {
		row := headerSize + i*tocChunkSize
		binary.BigEndian.PutUint32(buf[row:row+4], c.id)
		binary.BigEndian.PutUint64(buf[row+4:row+tocChunkSize], uint64(offsets[i]))
	}
	sentinel := headerSize + numChunks*tocChunkSize
	binary.BigEndian.PutUint32(buf[sentinel:sentinel+4], 0)
	binary.BigEndian.PutUint64(buf[sentinel+4:sentinel+tocChunkSize], uint64(offsets[numChunks]))

	// Chunk data in TOC order.
	for i, c := range chunks {
		copy(buf[offsets[i]:], c.data)
	}

	trailer := sha256.Sum256(buf[:off])
	copy(buf[off:], trailer[:])
	return buf
}

// resign recomputes the trailing SHA-256 checksum in place after a body has
// been mutated, so tests can target validation past the checksum gate.
func resign(body []byte) {
	payloadEnd := len(body) - trailerSize
	trailer := sha256.Sum256(body[:payloadEnd])
	copy(body[payloadEnd:], trailer[:])
}

func pathChunkData(width int, paths ...string) []byte {
	data := make([]byte, len(paths)*width)
	for i, p := range paths {
		copy(data[i*width:], p)
	}
	return data
}

func objsChunkData(start, count uint32) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], start)
	binary.BigEndian.PutUint32(b[4:8], count)
	return b
}

func refsChunkData(indices ...uint32) []byte {
	b := make([]byte, len(indices)*4)
	for i, idx := range indices {
		binary.BigEndian.PutUint32(b[i*4:], idx)
	}
	return b
}

// validManifest returns a well-formed body with PATH, OBJS and REFS chunks that
// ParseManifest accepts. Individual tests mutate it to exercise one failure.
func validManifest(t *testing.T) []byte {
	t.Helper()

	const width = 16
	paths := []string{
		"pack/aaaa.idx",
		"pack/aaaa.pack",
		"pack/aaaa.rev",
		"refs/bbbb.ref",
		"refs/cccc.ref",
	}
	return buildManifest(t, manifestVersion, width, hashAlgoSHA1, []chunkSpec{
		{chunkIDPath, pathChunkData(width, paths...)},
		{chunkIDObjs, objsChunkData(0, 3)},
		{chunkIDRefs, refsChunkData(4, 3)},
	})
}

// pathOnlyManifest returns a body whose only chunk is a single-record PATH.
func pathOnlyManifest(t *testing.T, width int, path string) []byte {
	t.Helper()
	return buildManifest(t, manifestVersion, byte(width), hashAlgoSHA1, []chunkSpec{
		{chunkIDPath, pathChunkData(width, path)},
	})
}

// assertManifestSelfNamed checks the manifest spec's content-addressing
// invariant: the 32-byte trailer is the SHA-256 of everything preceding it, and
// its hex form is the body's filename under manifests/.
func assertManifestSelfNamed(t *testing.T, body []byte, wantName string) {
	t.Helper()
	payload := body[:len(body)-trailerSize]
	sum := sha256.Sum256(payload)
	require.Equal(t, body[len(body)-trailerSize:], sum[:])
	require.Equal(t, wantName, fmt.Sprintf("%x", sum))
}

// SerializeManifest writes a binary chunk-based manifest body. Returns the
// complete body (including the 32-byte SHA-256 trailer) and the hex trailer
// hash. The trailer hash is the content-addressed filename under which Git
// and Gitaly store the manifest body in manifests/<sha>.
func SerializeManifest(m *Manifest) ([]byte, string, error) {
	paths := make([]string, len(m.Paths))
	copy(paths, m.Paths)
	sort.Strings(paths)

	// Compute PATH record width: longest path + NUL terminator, rounded up
	// to a 4-byte boundary. Minimum is 4 (even for empty manifests).
	maxLen := 0
	for _, p := range paths {
		if len(p) > maxLen {
			maxLen = len(p)
		}
	}
	pathWidth := (maxLen + 1 + 3) & ^3
	if pathWidth < 4 {
		pathWidth = 4
	}
	if pathWidth > maxPathWidth {
		return nil, "", fmt.Errorf("path width %d exceeds maximum %d", pathWidth, maxPathWidth)
	}

	hashAlgo := m.HashAlgo
	if hashAlgo == 0 {
		hashAlgo = hashAlgoSHA1
	}

	pathIndex := make(map[string]int, len(paths))
	for i, p := range paths {
		pathIndex[p] = i
	}

	// OBJS: contiguous range of pack/ entries in the sorted path list.
	var objStart, objCount int
	for i, p := range paths {
		if strings.HasPrefix(p, "pack/") {
			if objCount == 0 {
				objStart = i
			}
			objCount++
		}
	}

	// REFS: indices into sorted paths, in reftable stack order.
	var refIndices []int
	for _, rp := range m.refStackOrder {
		if idx, ok := pathIndex[rp]; ok {
			refIndices = append(refIndices, idx)
		}
	}

	// Build chunk payloads.
	pathData := make([]byte, len(paths)*pathWidth)
	for i, p := range paths {
		copy(pathData[i*pathWidth:], p)
	}

	objsData := make([]byte, 8)
	binary.BigEndian.PutUint32(objsData[0:4], uint32(objStart))
	binary.BigEndian.PutUint32(objsData[4:8], uint32(objCount))

	refsData := make([]byte, len(refIndices)*4)
	for i, idx := range refIndices {
		binary.BigEndian.PutUint32(refsData[i*4:], uint32(idx))
	}

	// Compute absolute byte offsets for each chunk.
	const numChunks = 3 // PATH, OBJS, REFS — always emitted in v1
	headerSize := 8
	tocSize := (numChunks + 1) * 12
	dataStart := uint64(headerSize + tocSize)
	pathOff := dataStart
	objsOff := pathOff + uint64(len(pathData))
	refsOff := objsOff + uint64(len(objsData))
	endOff := refsOff + uint64(len(refsData))

	buf := make([]byte, 0, int(endOff)+trailerSize)

	// Header: magic(4) + version(1) + num_chunks(1) + path_width(1) + hash_algo(1).
	hdr := [8]byte{}
	binary.BigEndian.PutUint32(hdr[0:4], manifestMagic)
	hdr[4] = manifestVersion
	hdr[5] = numChunks
	hdr[6] = byte(pathWidth)
	hdr[7] = hashAlgo
	buf = append(buf, hdr[:]...)

	// Chunk lookup table: (C+1) rows of (4-byte ID + 8-byte offset).
	// Final row is the zero-ID sentinel marking the end of chunk data.
	toc := make([]byte, tocSize)
	binary.BigEndian.PutUint32(toc[0:4], chunkIDPath)
	binary.BigEndian.PutUint64(toc[4:12], pathOff)
	binary.BigEndian.PutUint32(toc[12:16], chunkIDObjs)
	binary.BigEndian.PutUint64(toc[16:24], objsOff)
	binary.BigEndian.PutUint32(toc[24:28], chunkIDRefs)
	binary.BigEndian.PutUint64(toc[28:36], refsOff)
	binary.BigEndian.PutUint32(toc[36:40], 0) // sentinel
	binary.BigEndian.PutUint64(toc[40:48], endOff)
	buf = append(buf, toc...)

	// Chunk data in TOC order: PATH, OBJS, REFS.
	buf = append(buf, pathData...)
	buf = append(buf, objsData...)
	buf = append(buf, refsData...)

	// Trailing SHA-256 checksum. Its hex form doubles as the content-addressed
	// filename (manifests/<hex>).
	trailer := sha256.Sum256(buf)
	buf = append(buf, trailer[:]...)

	return buf, fmt.Sprintf("%x", trailer), nil
}
