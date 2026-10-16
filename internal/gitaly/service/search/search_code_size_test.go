package search

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// longLinesFile returns n lines, each "needle " followed by width bytes of filler.
func longLinesFile(n, width int, filler string) []byte {
	line := "needle " + strings.Repeat(filler, width/len(filler)) + "\n"
	return []byte(strings.Repeat(line, n))
}

// TestSearchCode_longLines is a regression test: lines of 100 KiB used to be sent whole, so
// that one batch exceeded the 4 MiB a gRPC client accepts by default and the whole search failed
// with ResourceExhausted. Lines are now cut and batches are bounded in bytes.
func TestSearchCode_longLines(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg, client := setup(t, ctx, nil)
	repo, _, commitID := createSearchRepo(t, ctx, cfg, map[string][]byte{
		"big.txt": longLinesFile(60, 100*1024, "x"),
	})

	requireBounded := func(t *testing.T, result searchResult) {
		t.Helper()
		for _, size := range result.sizes {
			require.LessOrEqual(t, size, maxBatchBytes+1024, "a message exceeds the byte budget")
		}
		for _, m := range result.matches {
			require.Len(t, m.GetLine(), maxLineBytes)
			require.True(t, bytes.HasPrefix(m.GetLine(), []byte("needle x")))
			for _, l := range append(m.GetPrecedingLines(), m.GetFollowingLines()...) {
				require.Len(t, l, maxLineBytes)
			}
		}
	}

	t.Run("without context", func(t *testing.T) {
		result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
			Repository: repo, Revision: []byte("main"), Query: "needle",
		})
		require.NoError(t, err)
		require.Equal(t, commitID, result.commitID)
		require.False(t, result.truncated)
		require.Len(t, result.matches, 60)
		requireBounded(t, result)
	})

	t.Run("with context and a limit", func(t *testing.T) {
		result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
			Repository: repo, Revision: []byte("main"), Query: "needle", ContextLines: 5, MaxResults: 10,
		})
		require.NoError(t, err)
		require.True(t, result.truncated)
		require.Len(t, result.matches, 10)
		requireBounded(t, result)

		require.Empty(t, result.matches[0].GetPrecedingLines())
		require.Len(t, result.matches[0].GetFollowingLines(), 5)
		require.Len(t, result.matches[9].GetPrecedingLines(), 5)
		require.Len(t, result.matches[9].GetFollowingLines(), 5)
	})

	t.Run("with context, batches split by bytes", func(t *testing.T) {
		result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
			Repository: repo, Revision: []byte("main"), Query: "needle", ContextLines: 5,
		})
		require.NoError(t, err)
		require.False(t, result.truncated)
		require.Len(t, result.matches, 60)
		requireBounded(t, result)

		// 60 matches of 11 lines of 4 KiB are about 2.6 MiB: fewer than matchBatchSize
		// matches, but more than one message's byte budget.
		require.Greater(t, result.messages, 2)
		require.Equal(t, append([]bool{true}, make([]bool, result.messages-1)...), result.commitIDSet)
		for i, m := range result.matches {
			require.Equal(t, int32(i+1), m.GetLineNumber(), "matches stay in line order across batches")
		}
	})
}

// TestSearchCode_longLinesKeepUTF8 checks that a long line of multi-byte characters is cut at a
// character boundary, so that the returned text stays valid UTF-8.
func TestSearchCode_longLinesKeepUTF8(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg, client := setup(t, ctx, nil)
	repo, _, _ := createSearchRepo(t, ctx, cfg, map[string][]byte{
		// "needle " is 7 bytes, so the 4096-byte limit falls inside a 2-byte "é".
		"u.txt": longLinesFile(3, 20*1024, "é"),
	})

	result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte("main"), Query: "needle", ContextLines: 1,
	})
	require.NoError(t, err)
	require.Len(t, result.matches, 3)

	for _, m := range result.matches {
		for _, l := range append([][]byte{m.GetLine()}, append(m.GetPrecedingLines(), m.GetFollowingLines()...)...) {
			require.True(t, utf8.Valid(l), "truncated line is not valid UTF-8")
			require.Len(t, l, maxLineBytes-1)
		}
	}
}

func TestTruncateLine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc  string
		line  string
		limit int
		want  string
	}{
		{desc: "shorter", line: "abc", limit: 4, want: "abc"},
		{desc: "exact", line: "abcd", limit: 4, want: "abcd"},
		{desc: "longer", line: "abcdef", limit: 4, want: "abcd"},
		{desc: "empty", line: "", limit: 4, want: ""},
		{desc: "cut before a 2-byte character", line: "abcé", limit: 4, want: "abc"},
		{desc: "cut inside a 3-byte character", line: "a€b", limit: 3, want: "a"},
		{desc: "cut inside a 4-byte character", line: "a😀", limit: 4, want: "a"},
		{desc: "character ends at the limit", line: "aé€", limit: 3, want: "aé"},
		{desc: "invalid UTF-8 is cut at the limit or shortly before", line: "\x80\x80\x80\x80\x80\x80", limit: 5, want: "\x80\x80"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			got := truncateLine([]byte(tc.line), tc.limit)
			require.Equal(t, tc.want, string(got))
		})
	}
}

func TestReadCappedLine(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("y", 1000)
	input := "short\n\n" + long + "\n" + "0123456789\n" + "tail"

	// A tiny buffer forces lines longer than the buffer to be read in several pieces.
	r := bufio.NewReaderSize(strings.NewReader(input), 16)

	for _, want := range []string{"short", "", long[:10], "0123456789"} {
		line, err := readCappedLine(r, 10)
		require.NoError(t, err)
		require.Equal(t, want, string(line))
	}

	// A line without its newline is incomplete output.
	_, err := readCappedLine(r, 10)
	require.ErrorIs(t, err, io.EOF)
}
