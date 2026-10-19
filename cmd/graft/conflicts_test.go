package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func TestConflictPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	got, err := conflictPath(dir, conflictsFilesDir, "a/b.txt")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "files", "a", "b.txt"), got)

	for _, p := range []string{
		"", "/etc/passwd", "../x", "a/../../x", "a//b", "a/./b", "a/",
		// A separator, a drive or a stream on Windows, whatever the system the CLI runs on.
		`a\b`, `..\x`, "c:x", "a:stream", "a\x00b",
	} {
		_, err := conflictPath(dir, conflictsFilesDir, p)
		require.Error(t, err, "%q", p)
	}
}

// TestWriteConflictsRefusesAnExistingDirectory: the files of an earlier package, which the agent
// may be editing, are never overwritten.
func TestWriteConflictsRefusesAnExistingDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "files"), 0o755))
	err := writeConflicts(context.Background(), nil, &gitalypb.LandConflict{}, dir, nil)
	require.ErrorContains(t, err, "already holds a conflicts directory")
}
