package search

// Black-box spec tests for WP41 (SearchService.SearchCode). Written from
// doc/graft/PLAN5.md only. Gatekeeper wiring fixes: field names aligned with
// proto/search.proto (paths are bytes, case_sensitive instead of ignore_case
// with the proto default of insensitive matching, line_number/line,
// preceding_lines/following_lines), the server is started with the package
// helper (importing setup.RegisterAll from package search is an import cycle),
// and nested paths are written with writeNestedTree (mktree rejects slashes).

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// specEnv starts a gitaly server with the search service and returns a client,
// the repository message, the filesystem path of the repository and the config.
func specEnv(t *testing.T) (gitalypb.SearchServiceClient, *gitalypb.Repository, string, config.Cfg, context.Context) {
	t.Helper()
	ctx := testhelper.Context(t)
	cfg, client := setup(t, ctx, nil)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	return client, repo, repoPath, cfg, ctx
}

// specCommit writes a commit whose tree holds the given files (paths may be nested).
func specCommit(t *testing.T, cfg config.Cfg, repoPath string, files map[string]string) git.ObjectID {
	t.Helper()
	contents := make(map[string][]byte, len(files))
	for p, c := range files {
		contents[p] = []byte(c)
	}
	return gittest.WriteCommit(t, cfg, repoPath, gittest.WithTree(writeNestedTree(t, cfg, repoPath, contents)))
}

// specCollect drains the SearchCode stream and returns every match, the commit
// id seen on the first message, and the truncated flag of the last message.
func specCollect(t *testing.T, stream gitalypb.SearchService_SearchCodeClient) (matches []*gitalypb.SearchCodeResponse_Match, commitID string, truncated bool, err error) {
	t.Helper()
	first := true
	for {
		resp, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			return matches, commitID, truncated, nil
		}
		if recvErr != nil {
			return matches, commitID, truncated, recvErr
		}
		if first {
			commitID = resp.GetCommitId()
			first = false
		}
		matches = append(matches, resp.GetMatches()...)
		truncated = resp.GetTruncated()
	}
}

func specSearch(t *testing.T, client gitalypb.SearchServiceClient, ctx context.Context, req *gitalypb.SearchCodeRequest) ([]*gitalypb.SearchCodeResponse_Match, string, bool, error) {
	t.Helper()
	stream, err := client.SearchCode(ctx, req)
	require.NoError(t, err)
	return specCollect(t, stream)
}

// TestSpecFixedVersusRegexAndSorting checks that the default query is a fixed
// string, that --regex switches to extended regular expressions, and that
// matches are emitted sorted by path and then by line.
func TestSpecFixedVersusRegexAndSorting(t *testing.T) {
	client, repo, repoPath, cfg, ctx := specEnv(t)
	commit := specCommit(t, cfg, repoPath, map[string]string{
		"b.txt": "a.b\naxb\n",
		"a.txt": "x\na.b\n",
	})

	fixed, commitID, _, err := specSearch(t, client, ctx, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte(commit.String()), Query: "a.b",
	})
	require.NoError(t, err)
	require.Equal(t, commit.String(), commitID)
	require.Len(t, fixed, 2, "fixed-string search must not treat '.' as a wildcard")
	require.Equal(t, "a.txt", string(fixed[0].GetPath()))
	require.Equal(t, int32(2), fixed[0].GetLineNumber())
	require.Equal(t, "b.txt", string(fixed[1].GetPath()))
	require.Equal(t, int32(1), fixed[1].GetLineNumber())

	regex, _, _, err := specSearch(t, client, ctx, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte(commit.String()), Query: "a.b", Regex: true,
	})
	require.NoError(t, err)
	require.Len(t, regex, 3, "regex search must match 'axb' as well")
}

// TestSpecCaseInsensitive checks case-sensitive matching against the default
// case-insensitive behaviour (proto: case_sensitive, default insensitive).
func TestSpecCaseInsensitive(t *testing.T) {
	client, repo, repoPath, cfg, ctx := specEnv(t)
	commit := specCommit(t, cfg, repoPath, map[string]string{
		"f.txt": "Hello\nhello\nHELLO\n",
	})

	sensitive, _, _, err := specSearch(t, client, ctx, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte(commit.String()), Query: "hello", CaseSensitive: true,
	})
	require.NoError(t, err)
	require.Len(t, sensitive, 1)

	insensitive, _, _, err := specSearch(t, client, ctx, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte(commit.String()), Query: "hello",
	})
	require.NoError(t, err)
	require.Len(t, insensitive, 3)
}

// TestSpecPathsFilterAndNoMatch checks that the paths filter limits the search
// and that a query without hits returns an empty stream without an error.
func TestSpecPathsFilterAndNoMatch(t *testing.T) {
	client, repo, repoPath, cfg, ctx := specEnv(t)
	commit := specCommit(t, cfg, repoPath, map[string]string{
		"dir/one.go":   "needle\n",
		"other/two.go": "needle\n",
	})

	filtered, _, _, err := specSearch(t, client, ctx, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte(commit.String()), Query: "needle", Paths: [][]byte{[]byte("dir")},
	})
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	require.Equal(t, "dir/one.go", string(filtered[0].GetPath()))

	none, _, truncated, err := specSearch(t, client, ctx, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte(commit.String()), Query: "absent-token-xyz",
	})
	require.NoError(t, err, "exit status 1 of git grep is no match, not an error")
	require.Empty(t, none)
	require.False(t, truncated)
}

// TestSpecContextLinesCapped checks that context lines are returned around a
// match and that requests above MaxContextLines are capped by configuration.
func TestSpecContextLinesCapped(t *testing.T) {
	client, repo, repoPath, cfg, ctx := specEnv(t)

	var b strings.Builder
	for i := 1; i <= 30; i++ {
		if i == 15 {
			b.WriteString("TARGET\n")
		} else {
			b.WriteString("line\n")
		}
	}
	commit := specCommit(t, cfg, repoPath, map[string]string{"ctx.txt": b.String()})

	maxCtx := cfg.Graft.Search.MaxContextLinesOrDefault()
	matches, _, _, err := specSearch(t, client, ctx, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte(commit.String()), Query: "TARGET", ContextLines: 100,
	})
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.Equal(t, int32(15), matches[0].GetLineNumber())
	require.LessOrEqual(t, len(matches[0].GetPrecedingLines()), maxCtx, "before-context must be capped")
	require.LessOrEqual(t, len(matches[0].GetFollowingLines()), maxCtx, "after-context must be capped")
	require.Len(t, matches[0].GetPrecedingLines(), maxCtx)
	require.Len(t, matches[0].GetFollowingLines(), maxCtx)
}

// TestSpecMaxResultsTruncates checks that the stream stops at the requested
// max_results and reports truncated=true without an error.
func TestSpecMaxResultsTruncates(t *testing.T) {
	client, repo, repoPath, cfg, ctx := specEnv(t)
	commit := specCommit(t, cfg, repoPath, map[string]string{
		"many.txt": "hit\nhit\nhit\nhit\nhit\n",
	})

	matches, _, truncated, err := specSearch(t, client, ctx, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte(commit.String()), Query: "hit", MaxResults: 2,
	})
	require.NoError(t, err)
	require.Len(t, matches, 2)
	require.True(t, truncated)
}

// TestSpecBinaryFilesSkippedAndUnicode checks that binary blobs are skipped
// and that unicode text and filenames containing spaces are searched and
// reported verbatim.
func TestSpecBinaryFilesSkippedAndUnicode(t *testing.T) {
	client, repo, repoPath, cfg, ctx := specEnv(t)
	commit := specCommit(t, cfg, repoPath, map[string]string{
		"bin.dat":     "needle\x00\x01\x02needle",
		"my file.txt": "naïve café needle\n",
	})

	matches, _, _, err := specSearch(t, client, ctx, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte(commit.String()), Query: "needle",
	})
	require.NoError(t, err)
	require.Len(t, matches, 1, "binary file must be skipped")
	require.Equal(t, "my file.txt", string(matches[0].GetPath()))
	require.Equal(t, "naïve café needle", string(matches[0].GetLine()))
}

// TestSpecValidationErrors checks the InvalidArgument and NotFound error codes
// for an empty query, an invalid regular expression and an unknown revision.
func TestSpecValidationErrors(t *testing.T) {
	client, repo, repoPath, cfg, ctx := specEnv(t)
	commit := specCommit(t, cfg, repoPath, map[string]string{"f.txt": "x\n"})

	_, _, _, err := specCollect2(t, client, ctx, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte(commit.String()), Query: "",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "empty query")

	_, _, _, err = specCollect2(t, client, ctx, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte(commit.String()), Query: "([", Regex: true,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "invalid regex")

	_, _, _, err = specCollect2(t, client, ctx, &gitalypb.SearchCodeRequest{
		Repository: repo, Revision: []byte("refs/heads/does-not-exist"), Query: "x",
	})
	require.Equal(t, codes.NotFound, status.Code(err), "unknown revision")
}

// specCollect2 is specSearch without a require on the initial call, so error
// codes can be inspected on either the call or the first Recv.
func specCollect2(t *testing.T, client gitalypb.SearchServiceClient, ctx context.Context, req *gitalypb.SearchCodeRequest) ([]*gitalypb.SearchCodeResponse_Match, string, bool, error) {
	t.Helper()
	stream, err := client.SearchCode(ctx, req)
	if err != nil {
		return nil, "", false, err
	}
	return specCollect(t, stream)
}
