package sdk_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
)

// fakeSearch implements SearchService. It records the requests it receives and
// sends the configured responses, or fails with err.
type fakeSearch struct {
	gitalypb.UnimplementedSearchServiceServer

	responses []*gitalypb.SearchCodeResponse
	err       error

	mu       sync.Mutex
	requests []*gitalypb.SearchCodeRequest
}

func (f *fakeSearch) register(srv *grpc.Server) {
	gitalypb.RegisterSearchServiceServer(srv, f)
}

func (f *fakeSearch) SearchCode(req *gitalypb.SearchCodeRequest, stream gitalypb.SearchService_SearchCodeServer) error {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	if f.err != nil {
		return f.err
	}
	for _, resp := range f.responses {
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSearch) recorded() []*gitalypb.SearchCodeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*gitalypb.SearchCodeRequest(nil), f.requests...)
}

func TestRepo_SearchCode_Request(t *testing.T) {
	t.Parallel()

	fake := &fakeSearch{}
	repo := dialFake(t, fake.register).Repo("repo")

	_, _, _, err := repo.SearchCode(t.Context(), sdk.SearchOptions{
		Revision:      "main",
		Query:         "a.*b",
		Regex:         true,
		CaseSensitive: true,
		Paths:         []string{"src/", "docs"},
		MaxResults:    7,
		ContextLines:  2,
	})
	require.NoError(t, err)

	reqs := fake.recorded()
	require.Len(t, reqs, 1)
	req := reqs[0]
	require.Equal(t, "default", req.GetRepository().GetStorageName())
	require.Equal(t, "repo", req.GetRepository().GetRelativePath())
	require.Equal(t, "graft:default/repo", req.GetRepository().GetGlRepository())
	require.Equal(t, []byte("main"), req.GetRevision())
	require.Equal(t, "a.*b", req.GetQuery())
	require.True(t, req.GetRegex())
	require.True(t, req.GetCaseSensitive())
	require.Equal(t, [][]byte{[]byte("src/"), []byte("docs")}, req.GetPaths())
	require.EqualValues(t, 7, req.GetMaxResults())
	require.EqualValues(t, 2, req.GetContextLines())
}

func TestRepo_SearchCode_DefaultsAreZero(t *testing.T) {
	t.Parallel()

	fake := &fakeSearch{}
	repo := dialFake(t, fake.register).Repo("repo")

	_, _, _, err := repo.SearchCode(t.Context(), sdk.SearchOptions{Revision: "main", Query: "todo"})
	require.NoError(t, err)

	req := fake.recorded()[0]
	require.False(t, req.GetRegex())
	require.False(t, req.GetCaseSensitive(), "matching is case insensitive by default")
	require.Empty(t, req.GetPaths())
	require.Zero(t, req.GetMaxResults(), "zero leaves the limit to the server")
	require.Zero(t, req.GetContextLines())
}

func TestRepo_SearchCode_Matches(t *testing.T) {
	t.Parallel()

	fake := &fakeSearch{
		responses: []*gitalypb.SearchCodeResponse{
			{
				CommitId: "abc123",
				Matches: []*gitalypb.SearchCodeResponse_Match{
					{
						Path:       []byte("a.go"),
						LineNumber: 3,
						Line:       []byte("func Foo() {}"),
					},
					{
						Path:           []byte("a.go"),
						LineNumber:     9,
						Line:           []byte("return Foo()"),
						PrecedingLines: [][]byte{[]byte("x"), []byte("y")},
						FollowingLines: [][]byte{[]byte("}")},
					},
				},
			},
			{
				Truncated: true,
				Matches: []*gitalypb.SearchCodeResponse_Match{
					{Path: []byte("b/c.txt"), LineNumber: 1, Line: []byte("Foo")},
				},
			},
		},
	}
	repo := dialFake(t, fake.register).Repo("repo")

	matches, commitID, truncated, err := repo.SearchCode(t.Context(), sdk.SearchOptions{Revision: "main", Query: "foo"})
	require.NoError(t, err)
	require.Equal(t, "abc123", commitID)
	require.True(t, truncated)
	require.Equal(t, []sdk.SearchMatch{
		{Path: "a.go", Line: 3, Text: "func Foo() {}"},
		{Path: "a.go", Line: 9, Text: "return Foo()", Before: []string{"x", "y"}, After: []string{"}"}},
		{Path: "b/c.txt", Line: 1, Text: "Foo"},
	}, matches)
}

func TestRepo_SearchCode_NoMatch(t *testing.T) {
	t.Parallel()

	repo := dialFake(t, (&fakeSearch{}).register).Repo("repo")

	matches, commitID, truncated, err := repo.SearchCode(t.Context(), sdk.SearchOptions{Revision: "main", Query: "absent"})
	require.NoError(t, err)
	require.Empty(t, matches)
	require.Empty(t, commitID)
	require.False(t, truncated)
}

func TestRepo_SearchCode_UnicodeAndSpecialPathsPreserved(t *testing.T) {
	t.Parallel()

	fake := &fakeSearch{
		responses: []*gitalypb.SearchCodeResponse{{
			CommitId: "c1",
			Matches: []*gitalypb.SearchCodeResponse_Match{{
				Path:       []byte("dir with space/naïve\nname.txt"),
				LineNumber: 2,
				Line:       []byte("héllo wörld 日本語"),
			}},
		}},
	}
	repo := dialFake(t, fake.register).Repo("repo")

	matches, _, _, err := repo.SearchCode(t.Context(), sdk.SearchOptions{Revision: "main", Query: "héllo"})
	require.NoError(t, err)
	require.Equal(t, []sdk.SearchMatch{{
		Path: "dir with space/naïve\nname.txt",
		Line: 2,
		Text: "héllo wörld 日本語",
	}}, matches)
}

func TestRepo_SearchCode_ServerErrors(t *testing.T) {
	t.Parallel()

	t.Run("invalid regex", func(t *testing.T) {
		repo := dialFake(t, (&fakeSearch{err: status.Error(codes.InvalidArgument, "invalid regex")}).register).Repo("repo")

		_, _, _, err := repo.SearchCode(t.Context(), sdk.SearchOptions{Revision: "main", Query: "(", Regex: true})
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))
	})

	t.Run("unknown revision", func(t *testing.T) {
		repo := dialFake(t, (&fakeSearch{err: status.Error(codes.NotFound, "revision not found")}).register).Repo("repo")

		_, _, _, err := repo.SearchCode(t.Context(), sdk.SearchOptions{Revision: "nope", Query: "x"})
		require.True(t, sdk.IsNotFound(err))
	})
}

func TestRepo_SearchCode_ValidationBeforeCalling(t *testing.T) {
	t.Parallel()

	valid := sdk.SearchOptions{Revision: "main", Query: "x"}
	tests := []struct {
		name string
		edit func(o *sdk.SearchOptions)
	}{
		{name: "empty revision", edit: func(o *sdk.SearchOptions) { o.Revision = "" }},
		{name: "empty query", edit: func(o *sdk.SearchOptions) { o.Query = "" }},
		{name: "query too long", edit: func(o *sdk.SearchOptions) { o.Query = strings.Repeat("q", 1025) }},
		{name: "negative max results", edit: func(o *sdk.SearchOptions) { o.MaxResults = -1 }},
		{name: "negative context", edit: func(o *sdk.SearchOptions) { o.ContextLines = -1 }},
		{name: "empty path", edit: func(o *sdk.SearchOptions) { o.Paths = []string{"a/", ""} }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeSearch{}
			repo := dialFake(t, fake.register).Repo("repo")

			opts := valid
			tc.edit(&opts)
			_, _, _, err := repo.SearchCode(t.Context(), opts)
			require.Equal(t, codes.InvalidArgument, sdk.Code(err))
			require.Empty(t, fake.recorded(), "invalid options must not reach the server")
		})
	}
}

func TestRepo_SearchCode_QueryAtLimitIsAccepted(t *testing.T) {
	t.Parallel()

	fake := &fakeSearch{}
	repo := dialFake(t, fake.register).Repo("repo")

	_, _, _, err := repo.SearchCode(t.Context(), sdk.SearchOptions{Revision: "main", Query: strings.Repeat("q", 1024)})
	require.NoError(t, err)
	require.Len(t, fake.recorded(), 1)
}
