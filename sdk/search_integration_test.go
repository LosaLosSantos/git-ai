package sdk_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/sdk"
)

func TestIntegration_SearchCode(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	commit := env.writeCommit(t, "main", map[string]string{
		"a.go":           "package a\n\nfunc Foo() {}\n\nvar x = foo\n",
		"dir/b.txt":      "nothing here\nFOO in caps\n",
		"dir/sub/c.md":   "foo\n",
		"with space.txt": "foo bar\n",
	})
	commitID := env.mustResolve(t, "main")
	require.Equal(t, string(commit), commitID)

	t.Run("case insensitive fixed string by default", func(t *testing.T) {
		matches, gotCommit, truncated, err := env.repo.SearchCode(env.ctx, sdk.SearchOptions{
			Revision: "main",
			Query:    "foo",
		})
		require.NoError(t, err)
		require.Equal(t, commitID, gotCommit)
		require.False(t, truncated)
		require.Equal(t, []sdk.SearchMatch{
			{Path: "a.go", Line: 3, Text: "func Foo() {}"},
			{Path: "a.go", Line: 5, Text: "var x = foo"},
			{Path: "dir/b.txt", Line: 2, Text: "FOO in caps"},
			{Path: "dir/sub/c.md", Line: 1, Text: "foo"},
			{Path: "with space.txt", Line: 1, Text: "foo bar"},
		}, matches)
	})

	t.Run("case sensitive", func(t *testing.T) {
		matches, _, _, err := env.repo.SearchCode(env.ctx, sdk.SearchOptions{
			Revision:      "main",
			Query:         "Foo",
			CaseSensitive: true,
		})
		require.NoError(t, err)
		require.Equal(t, []sdk.SearchMatch{
			{Path: "a.go", Line: 3, Text: "func Foo() {}"},
		}, matches)
	})

	t.Run("regex", func(t *testing.T) {
		matches, _, _, err := env.repo.SearchCode(env.ctx, sdk.SearchOptions{
			Revision: "main",
			Query:    "^(func|var) [a-z]+ ",
			Regex:    true,
		})
		require.NoError(t, err)
		require.Equal(t, []sdk.SearchMatch{
			{Path: "a.go", Line: 5, Text: "var x = foo"},
		}, matches)
	})

	t.Run("invalid regex is an invalid argument", func(t *testing.T) {
		_, _, _, err := env.repo.SearchCode(env.ctx, sdk.SearchOptions{
			Revision: "main",
			Query:    "(unclosed",
			Regex:    true,
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
	})

	t.Run("paths limit the search", func(t *testing.T) {
		matches, _, _, err := env.repo.SearchCode(env.ctx, sdk.SearchOptions{
			Revision: "main",
			Query:    "foo",
			Paths:    []string{"dir"},
		})
		require.NoError(t, err)
		require.Equal(t, []sdk.SearchMatch{
			{Path: "dir/b.txt", Line: 2, Text: "FOO in caps"},
			{Path: "dir/sub/c.md", Line: 1, Text: "foo"},
		}, matches)
	})

	t.Run("filenames with spaces are preserved", func(t *testing.T) {
		matches, _, _, err := env.repo.SearchCode(env.ctx, sdk.SearchOptions{
			Revision: "main",
			Query:    "bar",
		})
		require.NoError(t, err)
		require.Equal(t, []sdk.SearchMatch{
			{Path: "with space.txt", Line: 1, Text: "foo bar"},
		}, matches)
	})

	t.Run("context lines", func(t *testing.T) {
		matches, _, _, err := env.repo.SearchCode(env.ctx, sdk.SearchOptions{
			Revision:     "main",
			Query:        "var x",
			ContextLines: 1,
		})
		require.NoError(t, err)
		require.Equal(t, []sdk.SearchMatch{
			{
				Path:   "a.go",
				Line:   5,
				Text:   "var x = foo",
				Before: []string{""},
			},
		}, matches)
	})

	t.Run("no match is not an error", func(t *testing.T) {
		matches, gotCommit, truncated, err := env.repo.SearchCode(env.ctx, sdk.SearchOptions{
			Revision: "main",
			Query:    "does-not-occur-anywhere",
		})
		require.NoError(t, err)
		require.Empty(t, matches)
		require.Equal(t, commitID, gotCommit)
		require.False(t, truncated)
	})

	t.Run("max results truncates", func(t *testing.T) {
		matches, _, truncated, err := env.repo.SearchCode(env.ctx, sdk.SearchOptions{
			Revision:   "main",
			Query:      "foo",
			MaxResults: 2,
		})
		require.NoError(t, err)
		require.Len(t, matches, 2)
		require.True(t, truncated)
	})

	t.Run("unknown revision is not found", func(t *testing.T) {
		_, _, _, err := env.repo.SearchCode(env.ctx, sdk.SearchOptions{
			Revision: "no-such-branch",
			Query:    "foo",
		})
		require.Equal(t, codes.NotFound, status.Code(err), "got %v", err)
	})
}
