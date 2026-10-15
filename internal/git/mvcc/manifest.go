// Package mvcc (Multi-Version Concurrency Control) contains the components used within Gitaly
// to create and manage Git repositories using the MVCC backend.
package mvcc

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"path"
	"strings"
)

// The manifest is expressed in the format specified by gitformat-chunk(5). These
// constants are specific to the manifest format. Hex values are big-endian.
//
// See Documentation/technical/mvcc-storage.adoc in Git's gitaly-mvcc-backend branch
// for more information.
const (
	// manifestMagic identifies the file as an MVCC manifest ("MVCC" in ASCII).
	manifestMagic = 0x4D564343
	// manifestVersion is the format version. Readers MUST reject unknown versions.
	manifestVersion = 1
	// maxPathWidth is the upper bound on the fixed-width PATH record size.
	maxPathWidth = 128

	// hashAlgoSHA1 and hashAlgoSHA256 identify the object hash algorithm
	// used by the repository.
	hashAlgoSHA1   = 1
	hashAlgoSHA256 = 2

	// The manifest format declares three kinds of chunks.
	chunkIDPath = 0x50415448
	chunkIDObjs = 0x4F424A53
	chunkIDRefs = 0x52454653

	// trailerSize is the length of the trailing SHA-256 checksum.
	trailerSize = 32

	// headerSize is the size of the manifest header
	headerSize = 8

	tocChunkSize = 12
)

// Manifest is the parsed form of an MVCC manifest body. A manifest file is
// a binary file that contains paths to locations on disk of Git artifacts,
// the sum of which is a repository's persistent state.
//
// The binary format carries three chunks — PATH, OBJS, REFS — that Gitaly
// uses to categorise artifacts for phased downloads: refs first (so
// ref-reading RPCs can proceed quickly), then packs, then anything else.
type Manifest struct {
	// Paths holds every relative artifact path in the snapshot, in
	// lexicographic order. Examples: "pack/abcdef.pack", "refs/012345.ref".
	Paths []string
	// HashAlgo identifies the repository's object hash algorithm
	// (1 = SHA-1, 2 = SHA-256).
	HashAlgo byte

	// objStart and objCount define the contiguous slice
	// Paths[objStart:objStart+objCount] identified by the OBJS chunk
	// as object-storage artifacts.
	objStart int
	objCount int

	// refStackOrder holds reftable paths in stack order (oldest first),
	// as identified by the REFS chunk. Stack order is semantically
	// significant: newer reftables shadow older ones.
	refStackOrder []string
}

type manifestHeader struct {
	magic     []byte
	version   byte
	numChunks int
	pathWidth int
	hashAlgo  byte
}

type manifestChunk struct {
	id     uint32
	offset int
	size   int
}

type manifestChunks struct {
	chunks  []manifestChunk
	pathIdx int
	objsIdx int
	refsIdx int
}

func (c *manifestChunks) AtIndex(i int) (manifestChunk, error) {
	if i < 0 || i >= len(c.chunks) {
		return manifestChunk{}, errors.New("chunk index out of range")
	}
	return c.chunks[i], nil
}

// PackHashes returns bare pack hashes extracted from the OBJS chunk's paths.
func (m *Manifest) PackHashes() []string {
	var hashes []string
	for _, p := range m.ObjPaths() {
		_, file := path.Split(p)
		if strings.HasSuffix(file, ".pack") {
			hashes = append(hashes, strings.TrimSuffix(file, ".pack"))
		}
	}
	return hashes
}

// ReftableHashes returns bare reftable hashes extracted from the REFS chunk's paths.
func (m *Manifest) ReftableHashes() []string {
	var hashes []string
	for _, p := range m.RefPaths() {
		_, file := path.Split(p)
		if strings.HasSuffix(file, ".ref") {
			hashes = append(hashes, strings.TrimSuffix(file, ".ref"))
		}
	}
	return hashes
}

// ObjRange returns the start index and count of the contiguous range of Paths
// identified by the OBJS chunk as object-storage artifacts.
func (m *Manifest) ObjRange() (start, count int) {
	return m.objStart, m.objCount
}

// ObjPaths returns the paths identified by the OBJS chunk as object-storage
// artifacts (pack .idx/.pack/.rev triples).
func (m *Manifest) ObjPaths() []string {
	if m.objCount == 0 {
		return nil
	}
	return m.Paths[m.objStart : m.objStart+m.objCount]
}

// RefPaths returns the reftable paths identified by the REFS chunk, in stack
// order (oldest first).
func (m *Manifest) RefPaths() []string {
	return m.refStackOrder
}

// ParseManifest parses an MVCC manifest body in binary chunk-based format
// (starting with "MVCC" magic).
func ParseManifest(data []byte) (*Manifest, error) {
	if len(data) == 0 {
		return &Manifest{}, fmt.Errorf("empty manifest")
	}

	if len(data) < headerSize+trailerSize {
		return &Manifest{}, fmt.Errorf("binary manifest too short (%d bytes)", len(data))
	}

	// Validate trailing SHA-256 checksum.
	payloadEnd := len(data) - trailerSize
	want := sha256.Sum256(data[:payloadEnd])
	if !bytes.Equal(want[:], data[payloadEnd:]) {
		return nil, fmt.Errorf("manifest checksum mismatch")
	}

	header, err := parseManifestHeader(data)
	if err != nil {
		return nil, err
	}

	chunks, err := parseChunks(data, header.numChunks)
	if err != nil {
		return nil, err
	}

	m := &Manifest{HashAlgo: header.hashAlgo}

	// Process PATH chunk
	pathChunk, err := chunks.AtIndex(chunks.pathIdx)
	if err != nil {
		return nil, err
	}

	pw := header.pathWidth
	if pathChunk.size%pw != 0 {
		return nil, fmt.Errorf("PATH chunk size %d not divisible by width %d", pathChunk.size, pw)
	}

	numPath := pathChunk.size / pw
	m.Paths = make([]string, numPath)
	for i := range numPath {
		start := pathChunk.offset + i*pw
		record := data[start : start+pw]
		end := bytes.IndexByte(record, 0)
		if end == -1 {
			return nil, fmt.Errorf("PATH record %d is not NUL-terminated", i)
		}
		p := string(record[:end])
		if !isValidManifestPath(p) {
			return nil, fmt.Errorf("manifest contains invalid path %q", p)
		}
		m.Paths[i] = p
	}

	// Process OBJS chunk if present.
	objsChunk, err := chunks.AtIndex(chunks.objsIdx)
	if err == nil {
		if objsChunk.size != 8 {
			return nil, fmt.Errorf("OBJS chunk has unexpected size %d (want 8)", objsChunk.size)
		}
		m.objStart = int(binary.BigEndian.Uint32(data[objsChunk.offset : objsChunk.offset+4]))
		m.objCount = int(binary.BigEndian.Uint32(data[objsChunk.offset+4 : objsChunk.offset+8]))
		if m.objStart+m.objCount > len(m.Paths) {
			return nil, fmt.Errorf("OBJS range [%d, %d) out of bounds (have %d paths)", m.objStart, m.objStart+m.objCount, len(m.Paths))
		}
	}

	// Process REFS chunk if present.
	refsChunk, err := chunks.AtIndex(chunks.refsIdx)
	if err == nil {
		if refsChunk.size%4 != 0 {
			return nil, fmt.Errorf("REFS chunk size %d not divisible by 4", refsChunk.size)
		}
		numRefs := refsChunk.size / 4
		m.refStackOrder = make([]string, numRefs)
		for i := range numRefs {
			idx := binary.BigEndian.Uint32(data[refsChunk.offset+i*4 : refsChunk.offset+i*4+4])
			if int(idx) >= len(m.Paths) {
				return nil, fmt.Errorf("REFS index %d out of bounds (have %d paths)", idx, len(m.Paths))
			}
			m.refStackOrder[i] = m.Paths[idx]
		}
	}

	return m, nil
}

// isValidManifestPath mirrors Git's is_valid_manifest_path(): non-empty,
// no leading slash, no "..", no "//", only [a-z0-9/.].
func isValidManifestPath(p string) bool {
	if p == "" || p[0] == '/' || p[len(p)-1] == '/' {
		return false
	}
	if strings.Contains(p, "..") || strings.Contains(p, "//") {
		return false
	}
	for _, c := range p {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '/' || c == '.') {
			return false
		}
	}
	return true
}

func parseManifestHeader(data []byte) (manifestHeader, error) {
	hdr := manifestHeader{
		magic:     data[:4],
		version:   data[4],
		numChunks: int(data[5]),
		pathWidth: int(data[6]),
		hashAlgo:  data[7],
	}

	if binary.BigEndian.Uint32(hdr.magic) != manifestMagic {
		return manifestHeader{}, errors.New("invalid manifest; missing magic number in header")
	}

	if hdr.version != manifestVersion {
		return manifestHeader{}, fmt.Errorf("unsupported manifest version %d", hdr.version)
	}

	if hdr.pathWidth == 0 || hdr.pathWidth > 128 {
		return manifestHeader{}, fmt.Errorf("manifest PATH record width must be between 0 and 128")
	}
	if hdr.hashAlgo != hashAlgoSHA1 && hdr.hashAlgo != hashAlgoSHA256 {
		return manifestHeader{}, fmt.Errorf("unsupported hash algorithm %d", hdr.hashAlgo)
	}

	tocSize := (hdr.numChunks + 1) * 12
	if len(data) < headerSize+tocSize+trailerSize {
		return manifestHeader{}, fmt.Errorf("manifest too short for %d-chunk TOC (%d bytes)", hdr.numChunks, len(data))
	}

	return hdr, nil
}

func parseChunks(data []byte, numChunks int) (*manifestChunks, error) {
	if numChunks <= 0 {
		return nil, errors.New("there must be at least one chunk besides the null terminator")
	}

	tocTerminator := headerSize + numChunks*12
	if binary.BigEndian.Uint32(data[tocTerminator:tocTerminator+4]) != 0 {
		return nil, fmt.Errorf("expected zero sentinel in TOC")
	}

	// The offsets are 64-bit values on disk. They are bounds-checked as uint64 before any
	// conversion to int: a value of 2^63 or more would otherwise become negative and slip
	// past the range checks below, and then slice the body with a negative index.
	dataEnd := uint64(len(data) - trailerSize)

	result := &manifestChunks{
		chunks:  make([]manifestChunk, numChunks),
		pathIdx: -1,
		objsIdx: -1,
		refsIdx: -1,
	}

	for i := range numChunks {
		// The TOC starts after the header
		chunkOffset := headerSize + i*tocChunkSize
		chunkID := binary.BigEndian.Uint32(data[chunkOffset : chunkOffset+4])
		chunkStart := binary.BigEndian.Uint64(data[chunkOffset+4 : chunkOffset+tocChunkSize])

		// Calculate the size of the chunk
		nextChunkStart := headerSize + (i+1)*tocChunkSize
		chunkEnd := binary.BigEndian.Uint64(data[nextChunkStart+4 : nextChunkStart+tocChunkSize])

		if chunkStart > dataEnd || chunkEnd > dataEnd || chunkEnd < chunkStart {
			return nil, fmt.Errorf("chunk %d has out-of-bounds offsets [%d, %d)", i, chunkStart, chunkEnd)
		}

		// Both offsets are at most dataEnd, which fits in an int, so the conversions are safe.
		chunkOffsetInFile := int(chunkStart)
		chunkSize := int(chunkEnd - chunkStart)

		result.chunks[i] = manifestChunk{
			id:     chunkID,
			offset: chunkOffsetInFile,
			size:   chunkSize,
		}

		switch chunkID {
		case chunkIDObjs:
			if result.objsIdx != -1 {
				return nil, fmt.Errorf("multiple OBJS chunks found")
			}
			result.objsIdx = i
		case chunkIDRefs:
			if result.refsIdx != -1 {
				return nil, fmt.Errorf("multiple REFS chunks found")
			}
			result.refsIdx = i

		case chunkIDPath:
			if result.pathIdx != -1 {
				return nil, fmt.Errorf("multiple PATH chunks found")
			}
			result.pathIdx = i
		}
	}

	// A PATH chunk is required so we validate here that one exist.
	// OBJS and REFS path are optional, so we don't validate them.
	if result.pathIdx == -1 {
		return nil, fmt.Errorf("no PATH chunks found")
	}

	return result, nil
}
