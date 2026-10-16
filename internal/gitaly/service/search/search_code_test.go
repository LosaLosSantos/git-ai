package search

import (
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var searchFixture = map[string][]byte{
	"a.txt":   []byte("Hello world\nhello again\nbye\n"),
	"b/c.go":  []byte("package main\n// Hello\n"),
	"bin.dat": []byte("hello\x00binary\n"),
}

func lines(matches []*gitalypb.SearchCodeResponse_Match) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, lineOf(m))
	}
	return out
}

func TestSearchCode_fixedStringsAndCase(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg, client := setup(t, ctx, nil)
	repo, _, commitID := createSearchRepo(t, ctx, cfg, searchFixture)

	t.Run("case insensitive by default", func(t *testing.T) {
		result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
			Repository: repo, Revision: []byte("main"), Query: "hello",
		})
		require.NoError(t, err)
		require.Equal(t, commitID, result.commitID)
		require.False(t, result.truncated)
		require.Equal(t, []string{
			"a.txt:1:Hello world",
			"a.txt:2:hello again",
			"b/c.go:2:// Hello",
		}, lines(result.matches))
	})

	t.Run("case sensitive", func(t *testing.T) {
		result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
			Repository: repo, Revision: []byte(commitID), Query: "Hello", CaseSensitive: true,
		})
		require.NoError(t, err)
		require.Equal(t, []string{"a.txt:1:Hello world", "b/c.go:2:// Hello"}, lines(result.matches))
	})

	t.Run("fixed strings treat metacharacters literally", func(t *testing.T) {
		result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
			Repository: repo, Revision: []byte("main"), Query: "w.rld",
		})
		require.NoError(t, err)
		require.Empty(t, result.matches)
	})

	t.Run("no match", func(t *testing.T) {
		result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
			Repository: repo, Revision: []byte("main"), Query: "absent",
		})
		require.NoError(t, err)
		require.Empty(t, result.matches)
		require.False(t, result.truncated)
		require.Equal(t, commitID, result.commitID)
		require.Equal(t, 1, result.messages)
	})
}

func TestSearchCode_regex(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg, client := setup(t, ctx, nil)
	repo, _, _ := createSearchRepo(t, ctx, cfg, searchFixture)

	result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte("main"), Query: "^h.llo a", Regex: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"a.txt:2:hello again"}, lines(result.matches))

	_, err = runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte("main"), Query: "(", Regex: true,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestSearchCode_pathsAndBinary(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg, client := setup(t, ctx, nil)
	repo, _, _ := createSearchRepo(t, ctx, cfg, searchFixture)

	result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte("main"), Query: "hello", Paths: [][]byte{[]byte("b")},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"b/c.go:2:// Hello"}, lines(result.matches))

	// bin.dat contains NUL bytes and must be skipped entirely.
	result, err = runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte("main"), Query: "binary",
	})
	require.NoError(t, err)
	require.Empty(t, result.matches)
}

func TestSearchCode_contextLines(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg, client := setup(t, ctx, func(cfg *config.Cfg) {
		cfg.Graft.Search.MaxContextLines = 1
	})
	repo, _, _ := createSearchRepo(t, ctx, cfg, map[string][]byte{
		"ctx.txt": []byte("1\n2\nmatch\n4\n5\n6\n"),
	})

	result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte("main"), Query: "match", ContextLines: 3,
	})
	require.NoError(t, err)
	require.Len(t, result.matches, 1)

	m := result.matches[0]
	require.Equal(t, int32(3), m.GetLineNumber())
	require.Equal(t, [][]byte{[]byte("2")}, m.GetPrecedingLines(), "context is capped by MaxContextLines")
	require.Equal(t, [][]byte{[]byte("4")}, m.GetFollowingLines())
}

// TestSearchCode_contextLinesWithGlobPaths is a regression test: git grep
// matches glob paths, so the context of its matches must be found too.
func TestSearchCode_contextLinesWithGlobPaths(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg, client := setup(t, ctx, nil)
	repo, _, _ := createSearchRepo(t, ctx, cfg, map[string][]byte{
		"dir/a.go":  []byte("one\ntwo\nneedle\nfour\n"),
		"dir/b.txt": []byte("one\nneedle\nthree\n"),
		"c.txt":     []byte("needle\nsecond\n"),
	})

	for _, tc := range []struct {
		desc  string
		paths []string
		want  []string
	}{
		{desc: "glob in a directory", paths: []string{"dir/*.go"}, want: []string{"dir/a.go:3:needle [two] [four]"}},
		{desc: "extension glob", paths: []string{"*.txt"}, want: []string{"c.txt:1:needle [] [second]", "dir/b.txt:2:needle [one] [three]"}},
		{desc: "glob and prefix", paths: []string{"dir/?.go", "c.txt"}, want: []string{"c.txt:1:needle [] [second]", "dir/a.go:3:needle [two] [four]"}},
		{desc: "prefix only", paths: []string{"dir"}, want: []string{"dir/a.go:3:needle [two] [four]", "dir/b.txt:2:needle [one] [three]"}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			req := &gitalypb.SearchCodeRequest{Repository: repo, Revision: []byte("main"), Query: "needle", ContextLines: 1}
			for _, p := range tc.paths {
				req.Paths = append(req.Paths, []byte(p))
			}

			result, err := runSearch(t, ctx, client, req)
			require.NoError(t, err)

			var got []string
			for _, m := range result.matches {
				got = append(got, fmt.Sprintf("%s %s %s", lineOf(m), m.GetPrecedingLines(), m.GetFollowingLines()))
			}
			require.Equal(t, tc.want, got)
		})
	}
}

func TestSearchCode_limits(t *testing.T) {
	ctx := testhelper.Context(t)

	t.Run("request max_results", func(t *testing.T) {
		cfg, client := setup(t, ctx, nil)
		repo, _, _ := createSearchRepo(t, ctx, cfg, map[string][]byte{
			"f.txt": []byte("x1\nx2\nx3\nx4\n"),
		})

		result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
			Repository: repo, Revision: []byte("main"), Query: "x", MaxResults: 2,
		})
		require.NoError(t, err)
		require.Len(t, result.matches, 2)
		require.True(t, result.truncated)
	})

	t.Run("configured max_results caps the request", func(t *testing.T) {
		cfg, client := setup(t, ctx, func(cfg *config.Cfg) { cfg.Graft.Search.MaxResults = 3 })
		repo, _, _ := createSearchRepo(t, ctx, cfg, map[string][]byte{
			"f.txt": []byte("x1\nx2\nx3\nx4\n"),
		})

		result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
			Repository: repo, Revision: []byte("main"), Query: "x", MaxResults: 100,
		})
		require.NoError(t, err)
		require.Len(t, result.matches, 3)
		require.True(t, result.truncated)
	})

	t.Run("exact limit is not truncated", func(t *testing.T) {
		cfg, client := setup(t, ctx, nil)
		repo, _, _ := createSearchRepo(t, ctx, cfg, map[string][]byte{
			"f.txt": []byte("x1\nx2\n"),
		})

		result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
			Repository: repo, Revision: []byte("main"), Query: "x", MaxResults: 2,
		})
		require.NoError(t, err)
		require.Len(t, result.matches, 2)
		require.False(t, result.truncated)
	})

	t.Run("timeout truncates without error", func(t *testing.T) {
		cfg, client := setup(t, ctx, func(cfg *config.Cfg) { cfg.Graft.Search.Timeout = "1ns" })
		repo, _, commitID := createSearchRepo(t, ctx, cfg, searchFixture)

		result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
			Repository: repo, Revision: []byte("main"), Query: "hello",
		})
		require.NoError(t, err)
		require.True(t, result.truncated)
		require.Equal(t, commitID, result.commitID)
	})
}

func TestSearchCode_batching(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg, client := setup(t, ctx, nil)

	var sb strings.Builder
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&sb, "hit %d\n", i)
	}
	repo, _, _ := createSearchRepo(t, ctx, cfg, map[string][]byte{"big.txt": []byte(sb.String())})

	result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte("main"), Query: "hit",
	})
	require.NoError(t, err)
	require.Len(t, result.matches, 250)
	require.False(t, result.truncated)
	require.Equal(t, 3, result.messages, "250 matches are sent in batches of 100")
	require.Equal(t, []bool{true, false, false}, result.commitIDSet, "only the first message carries the commit ID")
	require.Equal(t, "big.txt:1:hit 0", lineOf(result.matches[0]))
	require.Equal(t, "big.txt:250:hit 249", lineOf(result.matches[249]))
}

func TestSearchCode_unusualNames(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg, client := setup(t, ctx, nil)
	repo, _, _ := createSearchRepo(t, ctx, cfg, map[string][]byte{
		"with space.txt": []byte("needle\n"),
		"ünï/cödé.txt":   []byte("needle ünïcödé\n"),
		"new\nline.txt":  []byte("needle\nsecond\n"),
	})

	result, err := runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte("main"), Query: "needle",
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		"with space.txt:1:needle",
		"ünï/cödé.txt:1:needle ünïcödé",
		"new\nline.txt:1:needle",
	}, lines(result.matches))

	result, err = runSearch(t, ctx, client, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte("main"), Query: "ünïcödé",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"ünï/cödé.txt:1:needle ünïcödé"}, lines(result.matches))
}

func TestSearchCode_validation(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg, client := setup(t, ctx, nil)
	repo, _, _ := createSearchRepo(t, ctx, cfg, searchFixture)

	valid := func() *gitalypb.SearchCodeRequest {
		return &gitalypb.SearchCodeRequest{Repository: repo, Revision: []byte("main"), Query: "hello"}
	}

	tests := []struct {
		name   string
		mutate func(*gitalypb.SearchCodeRequest)
		code   codes.Code
	}{
		{name: "missing repository", mutate: func(r *gitalypb.SearchCodeRequest) { r.Repository = nil }, code: codes.InvalidArgument},
		{name: "empty query", mutate: func(r *gitalypb.SearchCodeRequest) { r.Query = "" }, code: codes.InvalidArgument},
		{name: "query too long", mutate: func(r *gitalypb.SearchCodeRequest) { r.Query = strings.Repeat("a", 1025) }, code: codes.InvalidArgument},
		{name: "query with newline", mutate: func(r *gitalypb.SearchCodeRequest) { r.Query = "hello\nbye" }, code: codes.InvalidArgument},
		{name: "query with carriage return", mutate: func(r *gitalypb.SearchCodeRequest) { r.Query = "hello\rbye" }, code: codes.InvalidArgument},
		{name: "empty revision", mutate: func(r *gitalypb.SearchCodeRequest) { r.Revision = nil }, code: codes.InvalidArgument},
		{name: "revision starts with dash", mutate: func(r *gitalypb.SearchCodeRequest) { r.Revision = []byte("--all") }, code: codes.InvalidArgument},
		{name: "revision range", mutate: func(r *gitalypb.SearchCodeRequest) { r.Revision = []byte("main..main") }, code: codes.InvalidArgument},
		{name: "unknown revision", mutate: func(r *gitalypb.SearchCodeRequest) { r.Revision = []byte("does-not-exist") }, code: codes.NotFound},
		{name: "absolute path", mutate: func(r *gitalypb.SearchCodeRequest) { r.Paths = [][]byte{[]byte("/a")} }, code: codes.InvalidArgument},
		{name: "parent path", mutate: func(r *gitalypb.SearchCodeRequest) { r.Paths = [][]byte{[]byte("../a")} }, code: codes.InvalidArgument},
		{name: "unclean path", mutate: func(r *gitalypb.SearchCodeRequest) { r.Paths = [][]byte{[]byte("a//b")} }, code: codes.InvalidArgument},
		{name: "trailing slash", mutate: func(r *gitalypb.SearchCodeRequest) { r.Paths = [][]byte{[]byte("b/")} }, code: codes.InvalidArgument},
		{name: "empty path", mutate: func(r *gitalypb.SearchCodeRequest) { r.Paths = [][]byte{nil} }, code: codes.InvalidArgument},
		{name: "pathspec magic", mutate: func(r *gitalypb.SearchCodeRequest) { r.Paths = [][]byte{[]byte(":(top)a.txt")} }, code: codes.InvalidArgument},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := valid()
			tc.mutate(req)

			_, err := runSearch(t, ctx, client, req)
			require.Equal(t, tc.code, status.Code(err), "error: %v", err)
		})
	}
}
