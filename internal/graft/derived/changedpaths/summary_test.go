package changedpaths

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSummary_ContainsAndMayContain(t *testing.T) {
	t.Parallel()

	s := newSummary([]string{"a/b/c", "d"})

	for _, p := range []string{"a", "a/b", "a/b/c", "d"} {
		require.True(t, s.Contains(p), p)
		require.True(t, s.MayContain(p), p)
	}
	for _, p := range []string{"b", "a/bc", "a/b/c/x", "d/x", "da"} {
		require.False(t, s.Contains(p), p)
	}

	// The root is changed when any file is.
	require.True(t, s.Contains(""))
	require.True(t, s.MayContain(""))
	require.False(t, newSummary(nil).Contains(""))
	require.False(t, newSummary(nil).MayContain(""))
	require.False(t, newSummary(nil).MayContain("a"))
}

func TestSummary_NoFalseNegatives(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(1))
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		parts := make([]string, 1+rng.Intn(4))
		for j := range parts {
			parts[j] = fmt.Sprintf("d%d", rng.Intn(20))
		}
		seen[strings.Join(parts, "/")] = true
	}

	var paths []string
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	s := newSummary(paths)
	for _, p := range paths {
		forEachPrefix(p, func(key string) {
			require.True(t, s.MayContain(key), key)
			require.True(t, s.Contains(key), key)
		})
	}
}

func TestSummary_EncodeRoundTrip(t *testing.T) {
	t.Parallel()

	for _, s := range []Summary{
		newSummary(nil),
		newSummary([]string{"a", "b/c", "b/d/e"}),
		{Overflow: true},
	} {
		got, err := decodeSummary(s.encode())
		require.NoError(t, err)
		require.Equal(t, s, got)
	}
}

func TestSummary_DecodeRejectsCorruptData(t *testing.T) {
	t.Parallel()

	for name, data := range map[string][]byte{
		"empty":                     {},
		"unknown version":           {2, 0, 0},
		"unknown flags":             {1, 2, 0},
		"overflow with extra bytes": {1, 1, 0},
		"truncated path":            {1, 0, 1, 2, 'a'},
		"unsorted paths":            {1, 0, 2, 1, 'b', 1, 'a'},
		"duplicate paths":           {1, 0, 2, 1, 'a', 1, 'a'},
		"empty path":                {1, 0, 1, 0},
		"trailing bytes":            {1, 0, 0, 0xff},
		"too many paths":            append([]byte{1, 0}, appendUvarint(nil, maxExactPaths+1)...),
		"truncated count":           {1, 0},
	} {
		_, err := decodeSummary(data)
		require.ErrorIs(t, err, errCorruptSummary, name)
	}
}

func appendUvarint(buf []byte, v uint64) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	return append(buf, byte(v))
}
