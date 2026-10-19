package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/feder-cr/git-ai/internal/graft/resolver"
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

// TestConflictsWriterRefusesAnExistingFileWithoutOverwriting: the writer creates every file anew,
// so the second of two paths that the file system takes for one file finds that file in place: it
// is refused, the error names both paths, and the first file is not overwritten. The file system
// of the test does not fold case, so README.md is made to be readme.md by a symbolic link: the
// collision goes through the file system, as a case-insensitive one would make it.
func TestConflictsWriterRefusesAnExistingFileWithoutOverwriting(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := conflictsWriter{written: map[string]string{}}
	require.NoError(t, w.create(filepath.Join(dir, "readme.md"), "readme.md", []byte("a")))
	require.NoError(t, os.Symlink("readme.md", filepath.Join(dir, "README.md")))
	err := w.create(filepath.Join(dir, "README.md"), "README.md", []byte("b"))
	require.ErrorContains(t, err, `conflicting paths "readme.md" and "README.md" are the same file`)
	content, err := os.ReadFile(filepath.Join(dir, "readme.md"))
	require.NoError(t, err)
	require.Equal(t, "a", string(content), "the first file is not overwritten")
}

// TestConflictFileFor: the file to edit is a true merge when the CLI can make one, and otherwise
// a conflict holding every side, which the marker rule refuses unedited: a side is never dropped.
func TestConflictFileFor(t *testing.T) {
	t.Parallel()

	side := &gitalypb.LandConflictSide{Mode: "100644", Oid: "1"}
	unavailable := &gitalypb.LandConflictSide{Mode: "100644", Unavailable: "gone"}
	file := func(base, ours, theirs *gitalypb.LandConflictSide) *gitalypb.LandConflictFile {
		return &gitalypb.LandConflictFile{Path: []byte("c.txt"), Base: base, Ours: ours, Theirs: theirs}
	}
	long := func(prefix string) []byte {
		var b strings.Builder
		for i := 0; i < 3000; i++ {
			b.WriteString(prefix + strconv.Itoa(i) + "\n")
		}
		return []byte(b.String())
	}

	for _, tc := range []struct {
		name  string
		file  *gitalypb.LandConflictFile
		sides [3][]byte
		want  []string
	}{
		{
			name: "a true merge", file: file(side, side, side),
			sides: [3][]byte{[]byte("a\nb\n"), []byte("a\nours\n"), []byte("a\ntheirs\n")},
			want:  []string{"a\n<<<<<<< ours (the branch tip)\nours\n||||||| base\nb\n=======\ntheirs\n>>>>>>> theirs (the change)\n"},
		},
		{
			name: "the base unavailable, a binary ours", file: file(unavailable, side, side),
			sides: [3][]byte{nil, []byte("\x00bin"), []byte("theirs\n")},
			want: []string{"<<<<<<< ours (the branch tip): binary\n" + conflictPointerPrefix + "the binary ours side is sides/ours/c.txt\n" +
				"||||||| base: unavailable, gone\n=======\ntheirs\n>>>>>>> theirs (the change)\n"},
		},
		{
			name: "both readable, a binary theirs", file: file(side, side, side),
			sides: [3][]byte{[]byte("b\n"), []byte("ours\n"), []byte("\x00bin")},
			want:  []string{"<<<<<<< ours (the branch tip)\nours\n||||||| base\nb\n=======\n" + conflictPointerPrefix + "the binary theirs side is sides/theirs/c.txt\n>>>>>>> theirs (the change): binary\n"},
		},
		{
			name: "a diff over merge3's bound", file: file(side, side, side),
			sides: [3][]byte{long("b"), long("o"), long("t")},
			want:  []string{"<<<<<<< ours (the branch tip)\no0\n", "||||||| base\nb0\n", "=======\nt0\n", "t2999\n>>>>>>> theirs (the change)\n"},
		},
		{
			name: "modify/delete with the base", file: file(side, nil, side),
			sides: [3][]byte{[]byte("b\n"), nil, []byte("theirs\n")},
			want:  []string{"<<<<<<< ours (the branch tip): deleted\n||||||| base\nb\n=======\ntheirs\n>>>>>>> theirs (the change)\n"},
		},
		{
			name: "modify/delete without the base", file: file(unavailable, side, nil),
			sides: [3][]byte{nil, []byte("ours\n"), nil},
			want:  []string{"<<<<<<< ours (the branch tip)\nours\n||||||| base: unavailable, gone\n=======\n>>>>>>> theirs (the change): deleted\n"},
		},
		{
			name: "add/add with ours unavailable", file: file(nil, unavailable, side),
			sides: [3][]byte{nil, nil, []byte("added by theirs")},
			want:  []string{"<<<<<<< ours (the branch tip): unavailable, gone\n||||||| base: none, both sides added the file\n=======\nadded by theirs\n>>>>>>> theirs (the change)\n"},
		},
		{
			// No base and one side (a file/directory conflict, or a rename): the note says what
			// the sides are, not that both sides added the file.
			name: "no base and ours absent", file: file(nil, nil, side),
			sides: [3][]byte{nil, nil, []byte("theirs\n")},
			want:  []string{"<<<<<<< ours (the branch tip): absent\n||||||| base: none\n=======\ntheirs\n>>>>>>> theirs (the change)\n"},
		},
		{
			name: "no base and theirs absent", file: file(nil, side, nil),
			sides: [3][]byte{nil, []byte("ours\n"), nil},
			want:  []string{"<<<<<<< ours (the branch tip)\nours\n||||||| base: none\n=======\n>>>>>>> theirs (the change): absent\n"},
		},
	} {
		got := string(conflictFileFor(tc.file, tc.sides))
		for _, w := range tc.want {
			require.Contains(t, got, w, tc.name)
		}
		if tc.name != "a true merge" {
			require.True(t, strings.HasPrefix(got, "<<<<<<< ours"), "%s: one conflict holding every side:\n%s", tc.name, got)
		}
	}
}

// writePackage writes the conflict.json of a conflicts directory.
func writePackage(t *testing.T, dir string, c *gitalypb.LandConflict) {
	t.Helper()
	raw, err := protojson.Marshal(c)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "files"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "conflict.json"), raw, 0o644))
}

// TestConflictPointerPrefixIsTheServers: the pointer line the CLI writes is the one the server's
// validation refuses.
func TestConflictPointerPrefixIsTheServers(t *testing.T) {
	t.Parallel()
	require.Equal(t, resolver.ConflictPointerPrefix, conflictPointerPrefix)
}

// TestReadResolutionRefusesAPointerLine: a file whose binary pointer the agent left in place is
// refused, markers or not.
func TestReadResolutionRefusesAPointerLine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	side := &gitalypb.LandConflictSide{Mode: "100644", Oid: "1"}
	writePackage(t, dir, &gitalypb.LandConflict{Files: []*gitalypb.LandConflictFile{{Path: []byte("c.bin"), Base: side, Ours: side, Theirs: side}}})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "files", "c.bin"), []byte("x\n"+conflictPointerPrefix+"the binary ours side is sides/ours/c.bin\n"), 0o644))
	_, err := readResolution(dir)
	require.ErrorContains(t, err, "still holds the pointer to a binary side")
}

// TestReadResolutionAcceptsAPointerLineOfTheSides: a file that holds a pointer line which a side
// has of its own (a document about the line, say) is content, as the server exempts it too.
func TestReadResolutionAcceptsAPointerLineOfTheSides(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	side := &gitalypb.LandConflictSide{Mode: "100644", Oid: "1"}
	writePackage(t, dir, &gitalypb.LandConflict{Files: []*gitalypb.LandConflictFile{{Path: []byte("doc/c.md"), Base: side, Ours: side, Theirs: side}}})
	line := conflictPointerPrefix + "an example of the line\n"
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sides", "ours", "doc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sides", "ours", "doc", "c.md"), []byte("doc\n"+line), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "files", "doc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "files", "doc", "c.md"), []byte("doc, resolved\n"+line), 0o644))
	res, err := readResolution(dir)
	require.NoError(t, err)
	require.Len(t, res.Files, 1)
}

// TestReadResolutionModes: the mode is the first file mode of ours, theirs and the base; a path
// that is a file on none of its sides (a submodule) can only be deleted.
func TestReadResolutionModes(t *testing.T) {
	t.Parallel()

	gitlink := &gitalypb.LandConflictSide{Mode: "160000", Unavailable: "submodule"}
	exec := &gitalypb.LandConflictSide{Mode: "100755", Oid: "1"}
	for name, tc := range map[string]struct {
		file *gitalypb.LandConflictFile
		mode string
		err  string
	}{
		"ours a submodule, theirs a file": {&gitalypb.LandConflictFile{Path: []byte("s"), Ours: gitlink, Theirs: exec}, "100755", ""},
		"a submodule only":                {&gitalypb.LandConflictFile{Path: []byte("s"), Ours: gitlink}, "", "is a file on none of its sides"},
	} {
		dir := t.TempDir()
		writePackage(t, dir, &gitalypb.LandConflict{Files: []*gitalypb.LandConflictFile{tc.file}})
		require.NoError(t, os.WriteFile(filepath.Join(dir, "files", "s"), []byte("x\n"), 0o644), name)
		res, err := readResolution(dir)
		if tc.err != "" {
			require.ErrorContains(t, err, tc.err, name)
			// Removing the file deletes the path.
			require.NoError(t, os.Remove(filepath.Join(dir, "files", "s")), name)
			res, err = readResolution(dir)
			require.NoError(t, err, name)
			require.True(t, res.Files[0].Delete, name)
			continue
		}
		require.NoError(t, err, name)
		require.Equal(t, tc.mode, res.Files[0].Mode, name)
	}
}
