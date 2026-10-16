package ref

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/helper/lines"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestServer_ListRefs(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

	oldCommitID := gittest.WriteCommit(t, cfg, repoPath)
	newCommitID := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(oldCommitID),
		gittest.WithAuthorDate(time.Date(2011, 2, 16, 14, 1, 0, 0, time.FixedZone("UTC+1", +1*60*60))),
	)

	for _, cmd := range [][]string{
		{"update-ref", "refs/heads/main", newCommitID.String()},
		{"tag", "lightweight-tag", newCommitID.String()},
		{"tag", "old-commit-tag", oldCommitID.String()},
		{"tag", "-m", "tag message", "annotated-tag", "refs/heads/main"},
		{"symbolic-ref", "refs/heads/symbolic", "refs/heads/main"},
		{"update-ref", "refs/remote/remote-name/remote-branch", newCommitID.String()},
		{"symbolic-ref", "HEAD", "refs/heads/main"},
		{"update-ref", "refs/heads/old", oldCommitID.String()},
		{"update-ref", "refs/heads/UPPER", newCommitID.String()},
	} {
		gittest.Exec(t, cfg, append([]string{"-C", repoPath}, cmd...)...)
	}

	annotatedTagOID := text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", "annotated-tag"))

	for _, tc := range []struct {
		desc              string
		request           *gitalypb.ListRefsRequest
		expectedGrpcError codes.Code
		expectedError     string
		expected          []*gitalypb.ListRefsResponse_Reference
		expectedCursor    string
	}{
		{
			desc: "no repo",
			request: &gitalypb.ListRefsRequest{
				Patterns: [][]byte{[]byte("refs/")},
			},
			expectedGrpcError: codes.InvalidArgument,
			expectedError:     "", // Ideally we would test the message but it changes when running through praefect
		},
		{
			desc: "no patterns",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
			},
			expectedGrpcError: codes.InvalidArgument,
			expectedError:     "rpc error: code = InvalidArgument desc = patterns must have at least one entry",
		},
		{
			desc: "bad sorting key",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/")},
				SortBy: &gitalypb.ListRefsRequest_SortBy{
					Key: gitalypb.ListRefsRequest_SortBy_Key(100),
				},
			},
			expectedGrpcError: codes.InvalidArgument,
			expectedError:     `rpc error: code = InvalidArgument desc = sorting key "100" is not supported`,
		},
		{
			desc: "bad sorting direction",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/")},
				SortBy: &gitalypb.ListRefsRequest_SortBy{
					Direction: gitalypb.SortDirection(100),
				},
			},
			expectedGrpcError: codes.InvalidArgument,
			expectedError:     "rpc error: code = InvalidArgument desc = sorting direction is not supported",
		},
		{
			desc: "not found",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("this-pattern-does-not-match-anything")},
			},
		},
		{
			desc: "not found and main",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns: [][]byte{
					[]byte("this-pattern-does-not-match-anything"),
					[]byte("refs/heads/main"),
				},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/heads/main"), Target: newCommitID.String()},
			},
		},
		{
			desc: "all",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/")},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/heads/UPPER"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/main"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/old"), Target: oldCommitID.String()},
				{Name: []byte("refs/heads/symbolic"), Target: newCommitID.String()},
				{Name: []byte("refs/remote/remote-name/remote-branch"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/annotated-tag"), Target: annotatedTagOID},
				{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/old-commit-tag"), Target: oldCommitID.String()},
			},
		},
		{
			desc: "sort by authordate desc",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/heads")},
				SortBy: &gitalypb.ListRefsRequest_SortBy{
					Direction: gitalypb.SortDirection_DESCENDING,
					Key:       gitalypb.ListRefsRequest_SortBy_AUTHORDATE,
				},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/heads/old"), Target: oldCommitID.String()},
				{Name: []byte("refs/heads/UPPER"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/main"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/symbolic"), Target: newCommitID.String()},
			},
		},
		{
			desc: "sort by refname with ignore case",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/heads/*")},
				IgnoreCase: true,
				SortBy: &gitalypb.ListRefsRequest_SortBy{
					Key:       gitalypb.ListRefsRequest_SortBy_REFNAME,
					Direction: gitalypb.SortDirection_ASCENDING,
				},
			},
			// With case-insensitive sorting, "main" comes before "UPPER" (m < u alphabetically)
			// Without ignore_case, "UPPER" would come first (ASCII: U=85 < m=109)
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/heads/main"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/old"), Target: oldCommitID.String()},
				{Name: []byte("refs/heads/symbolic"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/UPPER"), Target: newCommitID.String()},
			},
		},
		{
			desc: "branches and tags only",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/heads/*"), []byte("refs/tags/*")},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/heads/UPPER"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/main"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/old"), Target: oldCommitID.String()},
				{Name: []byte("refs/heads/symbolic"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/annotated-tag"), Target: annotatedTagOID},
				{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/old-commit-tag"), Target: oldCommitID.String()},
			},
		},
		{
			desc: "head and branches and tags only",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Head:       true,
				Patterns:   [][]byte{[]byte("refs/heads/*"), []byte("refs/tags/*")},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("HEAD"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/UPPER"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/main"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/old"), Target: oldCommitID.String()},
				{Name: []byte("refs/heads/symbolic"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/annotated-tag"), Target: annotatedTagOID},
				{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/old-commit-tag"), Target: oldCommitID.String()},
			},
		},
		{
			desc: "tags filtered by one OID",
			request: &gitalypb.ListRefsRequest{
				Repository:     repo,
				Head:           false,
				Patterns:       [][]byte{[]byte("refs/tags/*")},
				PointingAtOids: [][]byte{[]byte(oldCommitID.String())},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/tags/old-commit-tag"), Target: oldCommitID.String()},
			},
		},
		{
			desc: "tags filtered by multiple OIDs",
			request: &gitalypb.ListRefsRequest{
				Repository:     repo,
				Head:           false,
				Patterns:       [][]byte{[]byte("refs/tags/*")},
				PointingAtOids: [][]byte{[]byte(oldCommitID.String()), []byte(newCommitID.String())},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/tags/annotated-tag"), Target: annotatedTagOID},
				{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/old-commit-tag"), Target: oldCommitID.String()},
			},
		},
		{
			desc: "with PeelTags option",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Head:       false,
				Patterns:   [][]byte{[]byte("refs/tags/*")},
				PeelTags:   true,
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/tags/annotated-tag"), Target: annotatedTagOID, PeeledTarget: newCommitID.String()},
				{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/old-commit-tag"), Target: oldCommitID.String()},
			},
		},
		{
			desc: "pagination with limit exceeding refs count",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/tags/*")},
				PaginationParams: &gitalypb.PaginationParameter{
					Limit: 100,
				},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/tags/annotated-tag"), Target: annotatedTagOID},
				{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/old-commit-tag"), Target: oldCommitID.String()},
			},
		},
		{
			desc: "pagination with limit smaller than refs count",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/tags/*")},
				PaginationParams: &gitalypb.PaginationParameter{
					Limit: 2,
				},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/tags/annotated-tag"), Target: annotatedTagOID},
				{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
			},
			expectedCursor: mustEncodeListRefsPageToken("refs/tags/lightweight-tag"),
		},
		{
			desc: "pagination with page token and no limit",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/tags/*")},
				PaginationParams: &gitalypb.PaginationParameter{
					PageToken: "refs/tags/annotated-tag",
				},
			},
			expectedGrpcError: codes.InvalidArgument,
			expectedError:     "rpc error: code = InvalidArgument desc = invalid page token: sending lines: could not find page token",
		},
		{
			desc: "pagination with page token and limit",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/tags/*")},
				PaginationParams: &gitalypb.PaginationParameter{
					PageToken: "refs/tags/annotated-tag",
					Limit:     2,
				},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/old-commit-tag"), Target: oldCommitID.String()},
			},
		},
		{
			desc: "pagination for the last page",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/tags/*")},
				PaginationParams: &gitalypb.PaginationParameter{
					PageToken: "refs/tags/old-commit-tag",
					Limit:     2,
				},
			},
			expected: nil,
		},
		{
			desc: "pagination with page token and reversed sorting",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/tags/*")},
				PaginationParams: &gitalypb.PaginationParameter{
					PageToken: "refs/tags/old-commit-tag",
					Limit:     2,
				},
				SortBy: &gitalypb.ListRefsRequest_SortBy{
					Direction: gitalypb.SortDirection_DESCENDING,
					Key:       gitalypb.ListRefsRequest_SortBy_AUTHORDATE,
				},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/annotated-tag"), Target: annotatedTagOID},
			},
		},
		{
			desc: "pagination with invalid page token",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/")},
				PaginationParams: &gitalypb.PaginationParameter{
					PageToken: "refs/tags/missing_tag",
				},
			},
			expectedGrpcError: codes.InvalidArgument,
			expectedError:     "rpc error: code = InvalidArgument desc = invalid page token: sending lines: could not find page token",
		},
		{
			desc: "case insensitive pattern matching with lowercase pattern",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/heads/upper")},
				IgnoreCase: true,
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/heads/UPPER"), Target: newCommitID.String()},
			},
		},
		{
			desc: "case insensitive pattern matching with uppercase pattern",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/heads/MAIN")},
				IgnoreCase: true,
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/heads/main"), Target: newCommitID.String()},
			},
		},
		{
			desc: "case sensitive pattern matching does not match different case",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/heads/upper")},
				IgnoreCase: false,
			},
			expected: nil,
		},
		{
			desc: "pagination with encoded page token and limit",
			request: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{[]byte("refs/tags/*")},
				PaginationParams: &gitalypb.PaginationParameter{
					PageToken: mustEncodeListRefsPageToken("refs/tags/annotated-tag"),
					Limit:     2,
				},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/old-commit-tag"), Target: oldCommitID.String()},
			},
		},
		{
			desc: "exclude single branch",
			request: &gitalypb.ListRefsRequest{
				Repository:      repo,
				Patterns:        [][]byte{[]byte("refs/heads/*")},
				ExcludePatterns: [][]byte{[]byte("refs/heads/main")},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/heads/UPPER"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/old"), Target: oldCommitID.String()},
				{Name: []byte("refs/heads/symbolic"), Target: newCommitID.String()},
			},
		},
		{
			desc: "exclude multiple branches",
			request: &gitalypb.ListRefsRequest{
				Repository:      repo,
				Patterns:        [][]byte{[]byte("refs/heads/*")},
				ExcludePatterns: [][]byte{[]byte("refs/heads/main"), []byte("refs/heads/old")},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/heads/UPPER"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/symbolic"), Target: newCommitID.String()},
			},
		},
		{
			desc: "exclude with glob pattern",
			request: &gitalypb.ListRefsRequest{
				Repository:      repo,
				Patterns:        [][]byte{[]byte("refs/tags/*")},
				ExcludePatterns: [][]byte{[]byte("refs/tags/old-*")},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/tags/annotated-tag"), Target: annotatedTagOID},
				{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
			},
		},
		{
			desc: "exclude non-existent ref",
			request: &gitalypb.ListRefsRequest{
				Repository:      repo,
				Patterns:        [][]byte{[]byte("refs/heads/*")},
				ExcludePatterns: [][]byte{[]byte("refs/heads/nonexistent")},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/heads/UPPER"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/main"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/old"), Target: oldCommitID.String()},
				{Name: []byte("refs/heads/symbolic"), Target: newCommitID.String()},
			},
		},
		{
			desc: "exclude with pagination",
			request: &gitalypb.ListRefsRequest{
				Repository:      repo,
				Patterns:        [][]byte{[]byte("refs/tags/*")},
				ExcludePatterns: [][]byte{[]byte("refs/tags/annotated-tag")},
				PaginationParams: &gitalypb.PaginationParameter{
					Limit: 2,
				},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
				{Name: []byte("refs/tags/old-commit-tag"), Target: oldCommitID.String()},
			},
		},
		{
			desc: "exclude pattern outside scanned namespace",
			request: &gitalypb.ListRefsRequest{
				Repository:      repo,
				Patterns:        [][]byte{[]byte("refs/heads/*")},
				ExcludePatterns: [][]byte{[]byte("refs/tags/v1")},
			},
			expected: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/heads/UPPER"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/main"), Target: newCommitID.String()},
				{Name: []byte("refs/heads/old"), Target: oldCommitID.String()},
				{Name: []byte("refs/heads/symbolic"), Target: newCommitID.String()},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			c, err := client.ListRefs(ctx, tc.request)
			require.NoError(t, err)

			var refs []*gitalypb.ListRefsResponse_Reference
			for {
				r, err := c.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if tc.expectedError == "" && tc.expectedGrpcError == 0 {
					require.NoError(t, err)
				} else {
					if tc.expectedError != "" {
						require.EqualError(t, err, tc.expectedError)
					}

					if tc.expectedGrpcError != 0 {
						testhelper.RequireGrpcCode(t, err, tc.expectedGrpcError)
					}

					return
				}

				if len(tc.expectedCursor) != 0 {
					require.Equal(t, tc.expectedCursor, r.GetPaginationCursor().GetNextCursor())
				} else {
					require.Nil(t, r.GetPaginationCursor())
				}

				refs = append(refs, r.GetReferences()...)
			}

			testhelper.ProtoEqual(t, tc.expected, refs)
		})
	}
}

func TestServer_ListRefs_taggerDate(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commitID := gittest.WriteCommit(t, cfg, repoPath)
	gittest.WriteRef(t, cfg, repoPath, "refs/heads/main", commitID)
	gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "HEAD", "refs/heads/main")

	date := time.Date(2020, 3, 4, 12, 30, 0, 0, time.FixedZone("UTC+5", 5*60*60))
	annotatedID := gittest.WriteTag(t, cfg, repoPath, "annotated", commitID.Revision(), gittest.WriteTagConfig{
		Message: "annotated tag", Date: date,
	})
	epochID := gittest.WriteTag(t, cfg, repoPath, "epoch", commitID.Revision(), gittest.WriteTagConfig{
		Message: "epoch tag", Date: time.Unix(0, 0),
	})
	gittest.WriteTag(t, cfg, repoPath, "lightweight", commitID.Revision())
	nestedDate := date.Add(time.Hour)
	nestedID := gittest.WriteTag(t, cfg, repoPath, "nested", annotatedID.Revision(), gittest.WriteTagConfig{
		Message: "nested tag", Date: nestedDate,
	})
	output := gittest.ExecOpts(t, cfg, gittest.ExecConfig{
		Stdin: strings.NewReader(fmt.Sprintf("object %s\ntype commit\ntag no-tagger\n\nno tagger\n", commitID)),
	}, "-C", repoPath, "hash-object", "--literally", "-t", "tag", "--stdin", "-w")
	missingDateID, err := gittest.DefaultObjectHash.FromHex(text.ChompBytes(output))
	require.NoError(t, err)
	gittest.WriteRef(t, cfg, repoPath, "refs/tags/no-tagger", missingDateID)

	for _, includeDate := range []bool{false, true} {
		for _, peelTags := range []bool{false, true} {
			t.Run(fmt.Sprintf("include date %t peel tags %t", includeDate, peelTags), func(t *testing.T) {
				request := &gitalypb.ListRefsRequest{
					Repository: repo, Patterns: [][]byte{[]byte("refs/")},
					Head: true, PeelTags: peelTags, IncludeTaggerDate: includeDate,
				}
				stream, err := client.ListRefs(ctx, request)
				require.NoError(t, err)
				var actual []*gitalypb.ListRefsResponse_Reference
				for {
					response, err := stream.Recv()
					if errors.Is(err, io.EOF) {
						break
					}
					require.NoError(t, err)
					actual = append(actual, response.GetReferences()...)
				}

				expected := []*gitalypb.ListRefsResponse_Reference{
					{Name: []byte("HEAD"), Target: commitID.String()},
					{Name: []byte("refs/heads/main"), Target: commitID.String()},
					{Name: []byte("refs/tags/annotated"), Target: annotatedID.String()},
					{Name: []byte("refs/tags/epoch"), Target: epochID.String()},
					{Name: []byte("refs/tags/lightweight"), Target: commitID.String()},
					{Name: []byte("refs/tags/nested"), Target: nestedID.String()},
					{Name: []byte("refs/tags/no-tagger"), Target: missingDateID.String()},
				}
				if includeDate {
					expected[2].TaggerDate = timestamppb.New(date)
					expected[3].TaggerDate = timestamppb.New(time.Unix(0, 0))
					expected[5].TaggerDate = timestamppb.New(nestedDate)
				}
				if peelTags {
					expected[2].PeeledTarget = commitID.String()
					expected[3].PeeledTarget = commitID.String()
					expected[5].PeeledTarget = commitID.String()
					expected[6].PeeledTarget = commitID.String()
				}
				testhelper.ProtoEqual(t, expected, actual)
			})
		}
	}

	t.Run("pagination with dates and peeling", func(t *testing.T) {
		var actual []*gitalypb.ListRefsResponse_Reference
		var cursor string
		for range 3 {
			stream, err := client.ListRefs(ctx, &gitalypb.ListRefsRequest{
				Repository: repo, Patterns: [][]byte{[]byte("refs/tags/")},
				PeelTags: true, IncludeTaggerDate: true,
				SortBy: &gitalypb.ListRefsRequest_SortBy{
					Key: gitalypb.ListRefsRequest_SortBy_CREATORDATE, Direction: gitalypb.SortDirection_DESCENDING,
				},
				PaginationParams: &gitalypb.PaginationParameter{Limit: 2, PageToken: cursor},
			})
			require.NoError(t, err)
			cursor = ""
			for {
				response, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				actual = append(actual, response.GetReferences()...)
				if next := response.GetPaginationCursor().GetNextCursor(); next != "" {
					cursor = next
				}
			}
			if cursor == "" {
				break
			}
		}
		require.Empty(t, cursor)
		testhelper.ProtoEqual(t, []*gitalypb.ListRefsResponse_Reference{
			{Name: []byte("refs/tags/nested"), Target: nestedID.String(), PeeledTarget: commitID.String(), TaggerDate: timestamppb.New(nestedDate)},
			{Name: []byte("refs/tags/annotated"), Target: annotatedID.String(), PeeledTarget: commitID.String(), TaggerDate: timestamppb.New(date)},
			{Name: []byte("refs/tags/lightweight"), Target: commitID.String()},
			{Name: []byte("refs/tags/epoch"), Target: epochID.String(), PeeledTarget: commitID.String(), TaggerDate: timestamppb.New(time.Unix(0, 0))},
			{Name: []byte("refs/tags/no-tagger"), Target: missingDateID.String(), PeeledTarget: commitID.String()},
		}, actual)
	})
}

func TestServer_ListRefs_taggerDateOverflow(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commitID := gittest.WriteCommit(t, cfg, repoPath)
	output := gittest.ExecOpts(t, cfg, gittest.ExecConfig{
		Stdin: strings.NewReader(fmt.Sprintf("object %s\ntype commit\ntag overflow\ntagger Author <author@example.com> 9223372036854775808 +0000\n\nmessage\n", commitID)),
	}, "-C", repoPath, "hash-object", "--literally", "-t", "tag", "--stdin", "-w")
	tagID, err := gittest.DefaultObjectHash.FromHex(text.ChompBytes(output))
	require.NoError(t, err)
	gittest.WriteRef(t, cfg, repoPath, "refs/tags/overflow", tagID)

	tag, err := client.FindTag(ctx, &gitalypb.FindTagRequest{Repository: repo, TagName: []byte("overflow")})
	require.NoError(t, err)
	stream, err := client.ListRefs(ctx, &gitalypb.ListRefsRequest{
		Repository: repo, Patterns: [][]byte{[]byte("refs/tags/")}, IncludeTaggerDate: true,
	})
	require.NoError(t, err)
	response, err := stream.Recv()
	require.NoError(t, err)
	testhelper.ProtoEqual(t, []*gitalypb.ListRefsResponse_Reference{
		{Name: []byte("refs/tags/overflow"), Target: tagID.String(), TaggerDate: tag.GetTag().GetTagger().GetDate()},
	}, response.GetReferences())
	_, err = stream.Recv()
	require.ErrorIs(t, err, io.EOF)
}

func TestListRefs_pagination(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

	oldCommitID := gittest.WriteCommit(t, cfg, repoPath)
	newCommitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(oldCommitID))

	for _, cmd := range [][]string{
		{"update-ref", "refs/heads/main", newCommitID.String()},
		{"tag", "lightweight-tag", newCommitID.String()},
		{"tag", "old-commit-tag", oldCommitID.String()},
		{"tag", "-m", "tag message", "annotated-tag", "refs/heads/main"},
		{"symbolic-ref", "refs/heads/symbolic", "refs/heads/main"},
		{"update-ref", "refs/remote/remote-name/remote-branch", newCommitID.String()},
		{"symbolic-ref", "HEAD", "refs/heads/main"},
		{"update-ref", "refs/heads/old", oldCommitID.String()},
	} {
		gittest.Exec(t, cfg, append([]string{"-C", repoPath}, cmd...)...)
	}

	annotatedTagOID := text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", "annotated-tag"))

	t.Run("paginate with limit 2", func(t *testing.T) {
		allRefs := collectAllRefs(t, ctx, client, repo, 2)

		expectedRefs := []*gitalypb.ListRefsResponse_Reference{
			{Name: []byte("refs/tags/annotated-tag"), Target: annotatedTagOID},
			{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
			{Name: []byte("refs/tags/old-commit-tag"), Target: oldCommitID.String()},
		}

		testhelper.ProtoEqual(t, expectedRefs, allRefs)
	})

	t.Run("different page sizes yield same results", func(t *testing.T) {
		refs1 := collectAllRefs(t, ctx, client, repo, 1)
		refs2 := collectAllRefs(t, ctx, client, repo, 2)
		refs3 := collectAllRefs(t, ctx, client, repo, 3)

		testhelper.ProtoEqual(t, refs1, refs2)
		testhelper.ProtoEqual(t, refs1, refs3)
	})

	t.Run("empty page token starts from beginning", func(t *testing.T) {
		page, nextPage := getPage(t, ctx, client, repo, "", 2)

		expectedRefs := []*gitalypb.ListRefsResponse_Reference{
			{Name: []byte("refs/tags/annotated-tag"), Target: annotatedTagOID},
			{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
		}

		testhelper.ProtoEqual(t, page, expectedRefs)
		require.NotEmpty(t, nextPage)
	})

	t.Run("cursor is base64-encoded JSON", func(t *testing.T) {
		_, nextPage := getPage(t, ctx, client, repo, "", 2)
		require.NotEmpty(t, nextPage)

		decodedBytes, err := base64.StdEncoding.DecodeString(nextPage)
		require.NoError(t, err, "cursor should be valid base64")

		var token struct {
			RefName string `json:"ref_name"`
		}
		err = json.Unmarshal(decodedBytes, &token)
		require.NoError(t, err, "cursor should be valid JSON")
		require.Equal(t, "refs/tags/lightweight-tag", token.RefName)
	})

	t.Run("first page includes all results", func(t *testing.T) {
		page, nextPage := getPage(t, ctx, client, repo, "", 3)

		expectedRefs := []*gitalypb.ListRefsResponse_Reference{
			{Name: []byte("refs/tags/annotated-tag"), Target: annotatedTagOID},
			{Name: []byte("refs/tags/lightweight-tag"), Target: newCommitID.String()},
			{Name: []byte("refs/tags/old-commit-tag"), Target: oldCommitID.String()},
		}

		testhelper.ProtoEqual(t, page, expectedRefs)
		require.Empty(t, nextPage)
	})
}

func TestListRefs_paginationCursorAtChunkBoundary(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)

	// This test relies on ItemsPerMessage being 3 (set in testhelper_test.go).
	require.Equal(t, 3, lines.ItemsPerMessage,
		"this test assumes ItemsPerMessage is 3; update test data if changed")

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commitID := gittest.WriteCommit(t, cfg, repoPath)

	// Create 5 tags so that limit=3 (equal to ItemsPerMessage) has more results beyond the limit.
	for i := 1; i <= 5; i++ {
		gittest.Exec(t, cfg, "-C", repoPath, "tag", fmt.Sprintf("tag-%03d", i), commitID.String())
	}

	// With 5 tags and limit=3, we must get a cursor back.
	// Before the fix, the cursor was nil when limit was a multiple of ItemsPerMessage.
	page, nextPage := getPage(t, ctx, client, repo, "", 3)
	require.Len(t, page, 3)
	require.NotEmpty(t, nextPage, "cursor must be set when limit equals chunk size and more refs exist")

	// Verify we can use the cursor to get the remaining refs
	page2, nextPage2 := getPage(t, ctx, client, repo, nextPage, 3)
	require.Len(t, page2, 2)
	require.Empty(t, nextPage2, "cursor should be empty on last page")

	// Verify all refs can be collected via pagination
	allRefs := collectAllRefs(t, ctx, client, repo, 3)
	require.Len(t, allRefs, 5, "pagination should return all refs across pages")
}

func TestListRefs_validate(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)
	repo, _ := gittest.CreateRepository(t, ctx, cfg)

	for _, tc := range []struct {
		desc        string
		req         *gitalypb.ListRefsRequest
		expectedErr error
	}{
		{
			desc:        "repository not provided",
			req:         &gitalypb.ListRefsRequest{Repository: nil},
			expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
		},
		{
			desc:        "no Patterns",
			req:         &gitalypb.ListRefsRequest{Repository: repo, Patterns: nil},
			expectedErr: status.Error(codes.InvalidArgument, "patterns must have at least one entry"),
		},
		{
			desc: "bad sort key",
			req: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{{}},
				SortBy:     &gitalypb.ListRefsRequest_SortBy{Key: gitalypb.ListRefsRequest_SortBy_Key(-1)},
			},
			expectedErr: status.Error(codes.InvalidArgument, `sorting key "-1" is not supported`),
		},
		{
			desc: "bad sort direction",
			req: &gitalypb.ListRefsRequest{
				Repository: repo,
				Patterns:   [][]byte{{}},
				SortBy: &gitalypb.ListRefsRequest_SortBy{
					Key:       gitalypb.ListRefsRequest_SortBy_REFNAME,
					Direction: gitalypb.SortDirection(-2),
				},
			},
			expectedErr: status.Error(codes.InvalidArgument, "sorting direction is not supported"),
		},
		{
			desc: "empty exclude pattern",
			req: &gitalypb.ListRefsRequest{
				Repository:      repo,
				Patterns:        [][]byte{[]byte("refs/heads/*")},
				ExcludePatterns: [][]byte{[]byte("")},
			},
			expectedErr: status.Error(codes.InvalidArgument, "empty exclude pattern"),
		},
		{
			desc: "exclude pattern without refs/ prefix",
			req: &gitalypb.ListRefsRequest{
				Repository:      repo,
				Patterns:        [][]byte{[]byte("refs/heads/*")},
				ExcludePatterns: [][]byte{[]byte("main")},
			},
			expectedErr: status.Error(codes.InvalidArgument, `exclude pattern must start with "refs/": "main"`),
		},
		{
			desc: "exclude pattern with null byte",
			req: &gitalypb.ListRefsRequest{
				Repository:      repo,
				Patterns:        [][]byte{[]byte("refs/heads/*")},
				ExcludePatterns: [][]byte{[]byte("refs/heads/\x00evil")},
			},
			expectedErr: status.Error(codes.InvalidArgument, `exclude pattern contains null byte: "refs/heads/\x00evil"`),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			stream, err := client.ListRefs(ctx, tc.req)
			require.NoError(t, err)
			_, err = stream.Recv()
			testhelper.RequireGrpcError(t, tc.expectedErr, err)
		})
	}
}

// Helper function to collect all refs across all pages
func collectAllRefs(t *testing.T, ctx context.Context, client gitalypb.RefServiceClient, repo *gitalypb.Repository, pageSize int32) []*gitalypb.ListRefsResponse_Reference {
	var allRefs []*gitalypb.ListRefsResponse_Reference
	cursor := ""

	for {
		page, nextCursor := getPage(t, ctx, client, repo, cursor, pageSize)
		allRefs = append(allRefs, page...)

		if len(nextCursor) == 0 {
			break
		}
		cursor = nextCursor
	}

	return allRefs
}

// Helper function to get a single page
func getPage(t *testing.T, ctx context.Context, client gitalypb.RefServiceClient, repo *gitalypb.Repository, cursor string, limit int32) ([]*gitalypb.ListRefsResponse_Reference, string) {
	c, err := client.ListRefs(ctx, &gitalypb.ListRefsRequest{
		Repository: repo,
		Patterns:   [][]byte{[]byte("refs/tags/*")},
		PaginationParams: &gitalypb.PaginationParameter{
			PageToken: cursor,
			Limit:     limit,
		},
	})
	require.NoError(t, err)

	var refs []*gitalypb.ListRefsResponse_Reference
	var nextCursor string

	for {
		r, err := c.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)

		if r.GetPaginationCursor() != nil {
			nextCursor = r.GetPaginationCursor().GetNextCursor()
		}
		refs = append(refs, r.GetReferences()...)
	}

	return refs, nextCursor
}

func TestEncodeDecodeListRefsPageToken(t *testing.T) {
	t.Parallel()

	t.Run("round trip", func(t *testing.T) {
		refName := "refs/heads/main"
		encoded, err := encodeListRefsPageToken(refName)
		require.NoError(t, err)
		require.NotEqual(t, refName, encoded)
		decoded := decodeListRefsPageToken(encoded)
		require.Equal(t, refName, decoded)
	})

	t.Run("raw ref name fallback", func(t *testing.T) {
		// A raw ref name is not valid base64, so it should be returned as-is
		raw := "refs/tags/my-tag"
		decoded := decodeListRefsPageToken(raw)
		require.Equal(t, raw, decoded)
	})

	t.Run("valid base64 but invalid JSON fallback", func(t *testing.T) {
		token := base64.StdEncoding.EncodeToString([]byte("not-json"))
		decoded := decodeListRefsPageToken(token)
		require.Equal(t, token, decoded)
	})

	t.Run("valid base64 and JSON but empty ref_name fallback", func(t *testing.T) {
		jsonBytes, err := json.Marshal(listRefsPageToken{RefName: ""})
		require.NoError(t, err)
		token := base64.StdEncoding.EncodeToString(jsonBytes)
		decoded := decodeListRefsPageToken(token)
		require.Equal(t, token, decoded)
	})
}

func mustEncodeListRefsPageToken(refName string) string {
	token, err := encodeListRefsPageToken(refName)
	if err != nil {
		panic(err)
	}
	return token
}
