package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/graft/workspace"
	"github.com/stretchr/testify/require"
)

func TestEmitChangesetJSONLine(t *testing.T) {
	t.Parallel()

	berlin := time.FixedZone("CET", 3600)
	c := workspace.Changeset{
		Sequence:   7,
		Reference:  "refs/heads/main",
		OldOID:     strings.Repeat("0", 40),
		NewOID:     strings.Repeat("b", 40),
		Operation:  "land",
		Actor:      "agent-1",
		RecordedAt: time.Date(2026, 10, 1, 11, 0, 0, 500, berlin),
		Rewritten:  true,
	}

	var out bytes.Buffer
	require.NoError(t, emitChangeset(&out, &options{jsonOut: true}, c))
	line := out.String()
	require.True(t, strings.HasSuffix(line, "\n"), "one object per line")
	require.Equal(t, 1, strings.Count(line, "\n"))

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &got))
	require.Equal(t, map[string]any{
		"sequence":    float64(7),
		"reference":   "refs/heads/main",
		"old_oid":     strings.Repeat("0", 40),
		"new_oid":     strings.Repeat("b", 40),
		"operation":   "land",
		"actor":       "agent-1",
		"recorded_at": "2026-10-01T10:00:00.0000005Z",
		"rewritten":   true,
	}, got)
}

func TestEmitChangesetTextByDefault(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	c := workspace.Changeset{Sequence: 1, Reference: "refs/heads/main", NewOID: strings.Repeat("c", 40), RecordedAt: time.Unix(0, 0).UTC()}
	require.NoError(t, emitChangeset(&out, &options{}, c))
	require.Contains(t, out.String(), "1\trefs/heads/main\t")
	require.False(t, strings.HasPrefix(out.String(), "{"))
}

func TestRunLogJSONLines(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("one"))
	useMemoryBackend(t, mem)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "log", "-C", dir, "--json")
	require.Equal(t, exitOK, code, stderr)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	require.NotEmpty(t, lines)
	for _, line := range lines {
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &obj), line)
		require.Contains(t, obj, "sequence")
		require.Contains(t, obj, "recorded_at")
	}
}
