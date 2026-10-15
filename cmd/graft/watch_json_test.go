package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/graft/workspace"
	"github.com/stretchr/testify/require"
)

func TestRunWatchJSONLines(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("one"))
	mem.Put("main", "b.txt", []byte("two"))
	useMemoryBackend(t, mem)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "watch", "-C", dir, "--json")
	require.Equal(t, exitOK, code, stderr)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	require.NotEmpty(t, lines)

	var previous float64
	for _, line := range lines {
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &obj), line)
		for _, key := range []string{"sequence", "reference", "old_oid", "new_oid", "operation", "actor", "recorded_at", "rewritten"} {
			require.Contains(t, obj, key, line)
		}
		require.Equal(t, "refs/heads/main", obj["reference"])
		seq, ok := obj["sequence"].(float64)
		require.True(t, ok)
		require.Greater(t, seq, previous, "sequences grow along the stream")
		previous = seq
	}
}

func TestWatchTextByDefault(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("one"))
	useMemoryBackend(t, mem)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "watch", "-C", dir)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "refs/heads/main")
	require.False(t, strings.HasPrefix(stdout, "{"))
}
