package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/stretchr/testify/require"
)

func TestDisplayText(t *testing.T) {
	t.Parallel()

	rlo := string(rune(0x202e))
	lineSeparator := string(rune(0x2028))
	nextLine := string(rune(0x85))

	for _, tc := range []struct {
		desc, in, want string
	}{
		{desc: "plain", in: "dir/a.txt", want: "dir/a.txt"},
		{desc: "printable unicode", in: "dir/é→ü.txt", want: "dir/é→ü.txt"},
		{desc: "backslash kept", in: `a\b`, want: `a\b`},
		{desc: "escape sequence", in: "x\x1b[1A\x1b[2K", want: `x\x1b[1A\x1b[2K`},
		{desc: "tab and delete", in: "a\tb\x7f", want: `a\x09b\x7f`},
		{desc: "C1 control", in: "a" + nextLine + "b", want: `a\u0085b`},
		{desc: "bidi override", in: "txt." + rlo + "exe", want: `txt.\u202eexe`},
		{desc: "line separator", in: "a" + lineSeparator + "b", want: `a\u2028b`},
		{desc: "invalid UTF-8", in: "\xff\x9b[2J", want: `\xff\x9b[2J`},
		{desc: "tag character", in: "a" + string(rune(0xe0001)), want: `a\U000e0001`},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.want, displayText(tc.in))
		})
	}
}

// TestPrintChangesetEscapesMemberText is the regression test for a resolved path
// that carried a terminal escape sequence into graft log: printed raw, it erased
// the line above and forged the attribution of a batch member.
func TestPrintChangesetEscapesMemberText(t *testing.T) {
	t.Parallel()

	c := batchChangeset()
	c.Actor = "graft-batch\x1b]0;title\x07"
	c.Members = c.Members[:1]
	c.Members[0].Actor = "agent-1\x1b[2K"
	c.Members[0].ResolvedPaths = []string{"x.txt\x1b[1A\x1b[2K    alice 111111111111 -> 222222222222"}

	var out bytes.Buffer
	printChangeset(&out, c)

	require.NotContains(t, out.String(), "\x1b")
	require.NotContains(t, out.String(), "\x07")
	require.Equal(t, strings.Join([]string{
		"9\trefs/heads/main\taaaaaaaaaaaa..bbbbbbbbbbbb\tland_batch\t" + `graft-batch\x1b]0;title\x07` + "\t1970-01-01T00:00:00Z",
		"  batch of 1",
		`    agent-1\x1b[2K cccccccccccc→dddddddddddd rewritten resolved: x.txt\x1b[1A\x1b[2K    alice 111111111111 -> 222222222222`,
		"",
	}, "\n"), out.String())
}

func TestResolvedNoteEscapesPaths(t *testing.T) {
	t.Parallel()

	require.Equal(t, ` (resolved 2 conflicting files: a.txt, b\x1b[2J.txt)`,
		resolvedNote([]string{"a.txt", "b\x1b[2J.txt"}))
	require.Empty(t, resolvedNote(nil))
}

// TestMemberNoteEscapesPaths checks the note of one member on its own.
func TestMemberNoteEscapesPaths(t *testing.T) {
	t.Parallel()

	require.Equal(t, ` rewritten resolved: a\x0d.txt`,
		memberNote(workspace.ChangesetMember{Rewritten: true, ResolvedPaths: []string{"a\r.txt"}}))
}
