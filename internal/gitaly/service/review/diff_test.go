package review

import (
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/stretchr/testify/require"
)

func TestSpecParseDiffTree(t *testing.T) {
	oldID := "1111111111111111111111111111111111111111"
	newID := "2222222222222222222222222222222222222222"
	nullID := "0000000000000000000000000000000000000000"

	out := []byte(
		":100644 100644 " + oldID + " " + newID + " M\x00a.go\x00" +
			":000000 100644 " + nullID + " " + newID + " A\x00dir/with space\n.go\x00" +
			":100644 000000 " + oldID + " " + nullID + " D\x00gone.go\x00",
	)

	entries, err := parseDiffTree(out)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	require.Equal(t, "a.go", entries[0].path)
	require.Equal(t, git.ObjectID(oldID), entries[0].oldID)
	require.Equal(t, git.ObjectID(newID), entries[0].newID)

	require.Equal(t, "dir/with space\n.go", entries[1].path, "paths with spaces and newlines are kept")
	require.Empty(t, entries[1].oldID, "an added file has no old side")
	require.Equal(t, git.ObjectID(newID), entries[1].newID)

	require.Equal(t, "gone.go", entries[2].path)
	require.Empty(t, entries[2].newID, "a deleted file has no new side")
}

func TestSpecParseDiffTreeEmpty(t *testing.T) {
	entries, err := parseDiffTree(nil)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestSpecParseDiffTreeRejectsMalformed(t *testing.T) {
	for name, out := range map[string][]byte{
		"path without header": []byte("a.go\x00"),
		"odd field count":     []byte(":100644 100644 a b M\x00"),
		"short header":        []byte(":100644 M\x00a.go\x00"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseDiffTree(out)
			require.Error(t, err)
		})
	}
}

func TestSpecValidBranchName(t *testing.T) {
	for _, name := range []string{"main", "feature/x", "release-1.2"} {
		require.True(t, validBranchName(name), name)
	}
	for _, name := range []string{"", "-main", "a..b", "main:file", "a b", "a~1", "a^", "a?", "a*", "a[b", `a\b`, "a\nb"} {
		require.False(t, validBranchName(name), name)
	}
}

func TestSpecIsNullOID(t *testing.T) {
	require.True(t, isNullOID("0000000000000000000000000000000000000000"))
	require.True(t, isNullOID(strings.Repeat("0", 64)), "sha256 null id")
	require.False(t, isNullOID("0000000000000000000000000000000000000001"))
}
