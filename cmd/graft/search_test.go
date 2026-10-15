package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/open_scion/sdk"
)

func TestWriteSearchText(t *testing.T) {
	t.Parallel()

	t.Run("matches without context", func(t *testing.T) {
		var out bytes.Buffer
		err := writeSearchText(&out, []sdk.SearchMatch{
			{Path: "a.go", Line: 3, Text: "func Foo() {}"},
			{Path: "a.go", Line: 9, Text: "return Foo()"},
			{Path: "b.txt", Line: 1, Text: "Foo"},
		}, false)
		require.NoError(t, err)
		require.Equal(t, "a.go:3:func Foo() {}\na.go:9:return Foo()\nb.txt:1:Foo\n", out.String())
	})

	t.Run("context lines and group separators", func(t *testing.T) {
		var out bytes.Buffer
		err := writeSearchText(&out, []sdk.SearchMatch{
			{Path: "a.go", Line: 5, Text: "hit", Before: []string{"l3", "l4"}, After: []string{"l6"}},
			{Path: "a.go", Line: 20, Text: "hit2", Before: []string{"l19"}},
			{Path: "b.go", Line: 2, Text: "hit3", Before: []string{"x1"}, After: []string{"x3"}},
		}, true)
		require.NoError(t, err)
		require.Equal(t, "a.go-3-l3\n"+
			"a.go-4-l4\n"+
			"a.go:5:hit\n"+
			"a.go-6-l6\n"+
			"--\n"+
			"a.go-19-l19\n"+
			"a.go:20:hit2\n"+
			"--\n"+
			"b.go-1-x1\n"+
			"b.go:2:hit3\n"+
			"b.go-3-x3\n", out.String())
	})

	t.Run("overlapping context is printed once", func(t *testing.T) {
		var out bytes.Buffer
		err := writeSearchText(&out, []sdk.SearchMatch{
			{Path: "a.go", Line: 4, Text: "one", After: []string{"two", "three"}},
			{Path: "a.go", Line: 6, Text: "three-hit", Before: []string{"two"}},
		}, true)
		require.NoError(t, err)
		require.Equal(t, "a.go:4:one\n"+
			"a.go-5-two\n"+
			"a.go:6:three-hit\n", out.String())
	})

	t.Run("control characters in text are escaped", func(t *testing.T) {
		var out bytes.Buffer
		err := writeSearchText(&out, []sdk.SearchMatch{
			{Path: "evil\x1b[2J.go", Line: 1, Text: "a\x1b[31mred"},
		}, false)
		require.NoError(t, err)
		require.NotContains(t, out.String(), "\x1b")
		require.Contains(t, out.String(), `\x1b`)
	})
}

func TestWriteSearchJSON(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := writeSearchJSON(&out, []sdk.SearchMatch{
		{Path: "a b.go", Line: 2, Text: "x", Before: []string{"w"}},
		{Path: "c.go", Line: 1, Text: "y"},
	})
	require.NoError(t, err)
	require.Equal(t, `{"path":"a b.go","line":2,"text":"x","before":["w"],"after":[]}`+"\n"+
		`{"path":"c.go","line":1,"text":"y","before":[],"after":[]}`+"\n", out.String())
}

// TestWriteSearchJSONInvalidUTF8 is a regression test: encoding/json replaces
// bytes that are not valid UTF-8 with U+FFFD, so two different Latin-1 paths
// used to print identically and could not be addressed. The _b64 fields now
// carry the exact bytes, as the changeset log does for resolved_paths.
func TestWriteSearchJSONInvalidUTF8(t *testing.T) {
	t.Parallel()

	b64 := base64.StdEncoding.EncodeToString

	var out bytes.Buffer
	err := writeSearchJSON(&out, []sdk.SearchMatch{
		{Path: "caf\xe9.txt", Line: 1, Text: "hit \xff", Before: []string{"ok", "bad \xfe"}, After: []string{"fine"}},
		{Path: "caf\xe8.txt", Line: 2, Text: "hit", After: []string{"\xc3"}},
	})
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	require.Len(t, lines, 2)

	var got []map[string]any
	for _, line := range lines {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		got = append(got, m)
	}

	// The readable form replaces the invalid bytes, and the base64 form is exact.
	require.Equal(t, map[string]any{
		"path":       "caf�.txt",
		"path_b64":   b64([]byte("caf\xe9.txt")),
		"line":       float64(1),
		"text":       "hit �",
		"text_b64":   b64([]byte("hit \xff")),
		"before":     []any{"ok", "bad �"},
		"before_b64": []any{b64([]byte("ok")), b64([]byte("bad \xfe"))},
		"after":      []any{"fine"},
	}, got[0])

	require.Equal(t, map[string]any{
		"path":      "caf�.txt",
		"path_b64":  b64([]byte("caf\xe8.txt")),
		"line":      float64(2),
		"text":      "hit",
		"before":    []any{},
		"after":     []any{"�"},
		"after_b64": []any{b64([]byte("\xc3"))},
	}, got[1])

	// The two paths read the same, but their exact forms tell them apart.
	require.Equal(t, got[0]["path"], got[1]["path"])
	require.NotEqual(t, got[0]["path_b64"], got[1]["path_b64"])
}

func TestSearchRejectsBadArgumentsBeforeContactingServer(t *testing.T) {
	tests := []struct {
		name string
		args []string
		msg  string
	}{
		{name: "no query", args: []string{"search"}, msg: "search requires QUERY"},
		{name: "two queries", args: []string{"search", "a", "b"}, msg: "search requires QUERY"},
		{name: "empty query", args: []string{"search", ""}, msg: "non-empty QUERY"},
		{name: "negative limit", args: []string{"search", "-n", "-1", "x"}, msg: "-n must not be negative"},
		{name: "negative context", args: []string{"search", "--context", "-2", "x"}, msg: "--context must not be negative"},
		{name: "unsafe path", args: []string{"search", "--path", "../etc", "x"}, msg: "--path"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			require.Equal(t, exitUsage, code, stderr.String())
			require.Contains(t, stderr.String(), tc.msg)
		})
	}
}
