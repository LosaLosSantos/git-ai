package sdk_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
)

func TestRepo_Blame(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.derived.blame = &gitalypb.BlameResponse{
		CommitId: "head-sha",
		Lines: []*gitalypb.BlameResponse_Line{
			{LineNumber: 1, CommitId: "c1", OriginalLineNumber: 1, Content: []byte("package x")},
			{LineNumber: 2, CommitId: "c2", OriginalLineNumber: 5, Content: []byte("")},
		},
	}
	repo := dialFake(t, fakes.register).Repo("repo")

	result, err := repo.Blame(t.Context(), "main", "x.go")
	require.NoError(t, err)
	require.Equal(t, &sdk.BlameResult{
		CommitID: "head-sha",
		Lines: []sdk.BlameLine{
			{LineNumber: 1, CommitID: "c1", OriginalLineNumber: 1, Content: []byte("package x")},
			{LineNumber: 2, CommitID: "c2", OriginalLineNumber: 5, Content: nil},
		},
	}, result)
}

func TestRepo_Blame_Errors(t *testing.T) {
	t.Parallel()

	repo := dialFake(t, newFakeServices().register).Repo("repo")

	_, err := repo.Blame(t.Context(), "main", "missing.go")
	require.True(t, sdk.IsNotFound(err))

	_, err = repo.Blame(t.Context(), "", "x.go")
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))
}

func TestRepo_PathHistory(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.derived.commits = []*gitalypb.GitCommit{
		{Id: "c3", Subject: []byte("third")},
		{Id: "c2", Subject: []byte("second")},
		{Id: "c1", Subject: []byte("first")},
	}
	repo := dialFake(t, fakes.register).Repo("repo")

	commits, err := repo.PathHistory(t.Context(), "main", "docs", 2)
	require.NoError(t, err)
	require.Len(t, commits, 2)
	require.Equal(t, "c3", commits[0].GetId())
	require.Equal(t, "c2", commits[1].GetId())

	require.Equal(t, uint32(2), fakes.derived.lastPathHistoryRequest.GetLimit())
	require.Equal(t, []byte("docs"), fakes.derived.lastPathHistoryRequest.GetPath())
}

func TestRepo_PathHistory_Validation(t *testing.T) {
	t.Parallel()

	repo := dialFake(t, newFakeServices().register).Repo("repo")

	_, err := repo.PathHistory(t.Context(), "main", "", 0)
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))
}
