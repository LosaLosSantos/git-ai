package changedpaths

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
	"strings"
)

const (
	// maxExactPaths bounds the changed paths that a summary stores (20,000, as in
	// doc/graft/PLAN3.md, WP27). A commit that changes more files is stored as an
	// overflow, and path queries for it are answered by a diff restricted to the path.
	maxExactPaths = 20000

	// bloomBitsPerKey sizes the filter per inserted key, which is a directory prefix
	// or a path. With bloomHashes hash functions that is a false positive rate of
	// about 0.2%.
	bloomBitsPerKey = 16
	bloomMinBytes   = 64
	bloomHashes     = 4

	summaryVersion byte = 1
	flagOverflow   byte = 1
)

var errCorruptSummary = errors.New("corrupt changed-paths summary")

// Summary is the set of files that a commit changes relative to its first parent.
//
// It answers two questions about a path: MayContain is a fast test that is never
// false when the path is changed, and Contains is the exact test. A path is changed
// when it is a changed file or a directory holding changed files, so a commit that
// changes "a/b/c" contains "a", "a/b" and "a/b/c".
type Summary struct {
	// Overflow reports that the commit changed too many files to store them. An
	// overflow holds no paths and must be answered with a diff.
	Overflow bool

	// paths holds the changed files, sorted and unique.
	paths []string
	// bloom holds the filter of every directory prefix and path in paths.
	bloom []byte
}

// newSummary returns the summary of the sorted, unique changed files paths.
func newSummary(paths []string) Summary {
	keys := 0
	for _, p := range paths {
		keys += strings.Count(p, "/") + 1
	}

	size := bloomBitsPerKey * keys / 8
	if size < bloomMinBytes {
		size = bloomMinBytes
	}

	s := Summary{paths: paths, bloom: make([]byte, size)}
	for _, p := range paths {
		forEachPrefix(p, func(key string) { bloomAdd(s.bloom, key) })
	}
	return s
}

// MayContain reports whether path may be changed. It returns false only when path is
// certainly unchanged. The root path "" is changed when the commit changes any file.
func (s Summary) MayContain(path string) bool {
	switch {
	case s.Overflow:
		return true
	case path == "":
		return len(s.paths) > 0
	}
	return bloomHas(s.bloom, path)
}

// Contains reports whether path is changed exactly. It must not be called on an
// overflow, which holds no paths; callers check Overflow first.
func (s Summary) Contains(path string) bool {
	if path == "" {
		return len(s.paths) > 0
	}

	if i := lowerBound(s.paths, path); i < len(s.paths) && s.paths[i] == path {
		return true
	}

	// Every path below path starts with path + "/", and those paths are contiguous in
	// the sorted list, so the first one at or after that prefix decides.
	below := path + "/"
	j := lowerBound(s.paths, below)
	return j < len(s.paths) && strings.HasPrefix(s.paths[j], below)
}

// lowerBound returns the index of the first element of the sorted slice that is not
// less than x.
func lowerBound(sorted []string, x string) int {
	lo, hi := 0, len(sorted)
	for lo < hi {
		mid := (lo + hi) / 2
		if sorted[mid] < x {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// encode returns the stored form of the summary:
//
//	version byte, flags byte, then for a summary without overflow the number of
//	paths as a uvarint, followed by each path as a uvarint length and its bytes.
//
// The filter is not stored. It is rebuilt from the paths when the summary is decoded,
// which costs one hash per path prefix and keeps the stored form small and free of
// redundancy that could disagree with the paths.
func (s Summary) encode() []byte {
	if s.Overflow {
		return []byte{summaryVersion, flagOverflow}
	}

	buf := []byte{summaryVersion, 0}
	buf = binary.AppendUvarint(buf, uint64(len(s.paths)))
	for _, p := range s.paths {
		buf = binary.AppendUvarint(buf, uint64(len(p)))
		buf = append(buf, p...)
	}
	return buf
}

// decodeSummary parses the output of encode. It rejects anything that encode could
// not have produced, such as unsorted or empty paths, trailing bytes or an unknown
// version, so that corrupt entries are recomputed instead of trusted.
func decodeSummary(data []byte) (Summary, error) {
	if len(data) < 2 || data[0] != summaryVersion {
		return Summary{}, errCorruptSummary
	}

	switch data[1] {
	case flagOverflow:
		if len(data) != 2 {
			return Summary{}, errCorruptSummary
		}
		return Summary{Overflow: true}, nil
	case 0:
	default:
		return Summary{}, errCorruptSummary
	}

	rest := data[2:]
	count, n := binary.Uvarint(rest)
	if n <= 0 || count > maxExactPaths {
		return Summary{}, errCorruptSummary
	}
	rest = rest[n:]

	var paths []string
	for i := uint64(0); i < count; i++ {
		size, n := binary.Uvarint(rest)
		if n <= 0 || size == 0 || size > uint64(len(rest)-n) {
			return Summary{}, errCorruptSummary
		}

		p := string(rest[n : n+int(size)])
		rest = rest[n+int(size):]
		if len(paths) > 0 && p <= paths[len(paths)-1] {
			return Summary{}, errCorruptSummary
		}
		paths = append(paths, p)
	}
	if len(rest) != 0 {
		return Summary{}, errCorruptSummary
	}

	return newSummary(paths), nil
}

// forEachPrefix calls fn with every directory prefix of p and with p itself, so
// that a query for a directory finds the files below it.
func forEachPrefix(p string, fn func(string)) {
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			fn(p[:i])
		}
	}
	fn(p)
}

// bloomPositions returns the bit positions of key in a filter of len(bits) bytes.
// The hashes are FNV-1a, which is stable across processes, as stored filters must be
// read back by any process. The positions use double hashing.
func bloomPositions(bits []byte, key string) [bloomHashes]uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	h1 := h.Sum64()
	_, _ = h.Write([]byte{0})
	h2 := h.Sum64() | 1

	n := uint64(len(bits)) * 8
	var pos [bloomHashes]uint64
	for i := range pos {
		pos[i] = (h1 + uint64(i)*h2) % n
	}
	return pos
}

func bloomAdd(bits []byte, key string) {
	for _, p := range bloomPositions(bits, key) {
		bits[p/8] |= 1 << (p % 8)
	}
}

func bloomHas(bits []byte, key string) bool {
	for _, p := range bloomPositions(bits, key) {
		if bits[p/8]&(1<<(p%8)) == 0 {
			return false
		}
	}
	return true
}
