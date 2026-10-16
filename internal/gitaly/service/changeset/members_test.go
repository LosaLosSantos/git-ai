package changeset

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestToProtoMembers(t *testing.T) {
	t.Parallel()

	require.Empty(t, toProto(changelog.Entry{Sequence: 1}).GetMembers())

	entry := changelog.Entry{
		Sequence:  2,
		Operation: changelog.OperationLandBatch,
		Members: []changelog.Member{
			{OriginalCommit: "aaa", Commit: "bbb", Actor: "agent-1", Rewritten: true, ResolvedPaths: []string{"a.txt"}},
			{OriginalCommit: "ccc", Commit: "ccc", Actor: "agent-2"},
		},
	}

	require.Equal(t, []*gitalypb.ChangesetMember{
		{
			OriginalCommitId: "aaa",
			CommitId:         "bbb",
			Actor:            "agent-1",
			Rewritten:        true,
			ResolvedPaths:    [][]byte{[]byte("a.txt")},
		},
		{
			OriginalCommitId: "ccc",
			CommitId:         "ccc",
			Actor:            "agent-2",
		},
	}, toProto(entry).GetMembers())
}
