package corestore

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewPackKeyIsOwnedAndParses(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("a pack"))
	key := NewPackKey(42, sum)

	require.Regexp(t, regexp.MustCompile(`^repos/42/packs/[0-9a-f]{64}-[0-9a-f]{32}\.pack$`), key)
	require.True(t, IsOwnedPackKey(key))

	id, got, ok := ParsePackKey(key)
	require.True(t, ok)
	require.Equal(t, int64(42), id)
	require.Equal(t, sum, got)
}

func TestNewPackKeyIsUniquePerUpload(t *testing.T) {
	t.Parallel()

	// Byte-identical packs get distinct keys, so no pack object is shared
	// between two rows (PLAN7, PACK OWNERSHIP RULE 1).
	sum := sha256.Sum256([]byte("identical content"))
	seen := map[string]struct{}{}
	for range 1000 {
		key := NewPackKey(1, sum)
		_, dup := seen[key]
		require.False(t, dup, "duplicate key %s", key)
		seen[key] = struct{}{}
	}
}

func TestParsePackKeyAcceptsLegacyKeys(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("written by v0.1.0"))
	legacy := fmt.Sprintf("repos/7/packs/%x.pack", sum)

	id, got, ok := ParsePackKey(legacy)
	require.True(t, ok)
	require.Equal(t, int64(7), id)
	require.Equal(t, sum, got)
	require.False(t, IsOwnedPackKey(legacy), "a legacy key is readable but not owned")
}

func TestParsePackKeyRejectsNonCanonicalKeys(t *testing.T) {
	t.Parallel()

	hexSum := fmt.Sprintf("%x", sha256.Sum256([]byte("x")))
	nonce := strings.Repeat("ab", 16)

	for _, key := range []string{
		"",
		"repos/1/packs/" + hexSum, // no suffix
		"repos/1/packs/" + hexSum + ".pack/extra",                            // trailing segment
		"repo/1/packs/" + hexSum + ".pack",                                   // wrong prefix
		"repos/1/objects/" + hexSum + ".pack",                                // wrong directory
		"repos/0/packs/" + hexSum + ".pack",                                  // repository ID 0
		"repos/-1/packs/" + hexSum + ".pack",                                 // negative ID
		"repos/01/packs/" + hexSum + ".pack",                                 // leading zero
		"repos/1/packs/" + strings.ToUpper(hexSum) + ".pack",                 // uppercase hex
		"repos/1/packs/" + hexSum[:63] + ".pack",                             // short SHA-256
		"repos/1/packs/" + hexSum + "-" + nonce[:31] + ".pack",               // short nonce
		"repos/1/packs/" + hexSum + "-" + nonce + "ab" + ".pack",             // long nonce
		"repos/1/packs/" + hexSum + "-" + strings.ToUpper(nonce) + ".pack",   // uppercase nonce
		"repos/1/packs/" + hexSum + "-" + strings.Repeat("zz", 16) + ".pack", // nonce not hex
		"repos/1/packs/" + hexSum + "-" + nonce + "-" + nonce + ".pack",      // two nonces
	} {
		_, _, ok := ParsePackKey(key)
		require.False(t, ok, "accepted %q", key)
		require.False(t, IsOwnedPackKey(key), "owned %q", key)
	}
}
