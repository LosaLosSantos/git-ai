package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		// A separator on Windows, whatever the system the CLI runs on.
		`a\b`, `..\x`, "a\x00b",
	} {
		_, err := conflictPath(dir, conflictsFilesDir, p)
		require.Error(t, err, "%q", p)
	}

	// A colon is a drive or a stream on Windows only.
	for _, p := range []string{"c:x", "a:stream"} {
		_, err := conflictPath(dir, conflictsFilesDir, p)
		if runtime.GOOS == "windows" {
			require.Error(t, err, "%q", p)
		} else {
			require.NoError(t, err, "%q", p)
		}
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

// TestConflictsWriterNamesBothPathsOfACollision: on a file system that folds case, the second of
// two paths finds the first one's file. Here the folding is simulated by a target that exists under
// the folded name; the error names both paths.
func TestConflictsWriterNamesBothPathsOfACollision(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := conflictsWriter{written: map[string]string{}}
	require.NoError(t, w.create(filepath.Join(dir, "readme.md"), "readme.md", []byte("a")))
	// A case-insensitive file system would resolve README.md to readme.md.
	w.written[strings.ToLower(filepath.Join(dir, "readme.md"))] = "README.md"
	err := w.create(filepath.Join(dir, "readme.md"), "readme.md", []byte("b"))
	require.ErrorContains(t, err, `conflicting paths "README.md" and "readme.md" are the same file`)
}

// TestConflictFileWithoutASide: a side that cannot be read is never dropped silently.
func TestConflictFileWithoutASide(t *testing.T) {
	t.Parallel()

	side := &gitalypb.LandConflictSide{Mode: "100644"}
	file := &gitalypb.LandConflictFile{Path: []byte("c.txt"), Base: side, Ours: side, Theirs: side}

	// Without the base: the two-way merge of the sides, with markers where they differ.
	got := string(conflictFileWithout(file, [3][]byte{nil, []byte("a\nours\n"), []byte("a\ntheirs\n")}))
	require.Contains(t, got, "<<<<<<< ours (the branch tip)\n")
	require.Contains(t, got, "ours\n")
	require.Contains(t, got, "theirs\n")

	// Without ours: the side there is against a note, between markers.
	got = string(conflictFileWithout(file, [3][]byte{[]byte("b\n"), nil, []byte("added by theirs")}))
	require.Equal(t, "<<<<<<< ours (the branch tip): unavailable, see conflict.json\n=======\nadded by theirs\n>>>>>>> theirs (the change)\n", got)

	// Without theirs, a binary ours is named by its side file rather than embedded.
	got = string(conflictFileWithout(file, [3][]byte{[]byte("b\n"), []byte("\x00bin"), nil}))
	require.Equal(t, "<<<<<<< ours (the branch tip)\n(binary: see sides/ours/c.txt)\n=======\n>>>>>>> theirs (the change): unavailable, see conflict.json\n", got)

	// Without either: a conflict to resolve.
	require.Equal(t, conflictsUnreadable, string(conflictFileWithout(file, [3][]byte{})))
}
