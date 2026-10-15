package changelog

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWithBatch(t *testing.T) {
	t.Parallel()

	require.Nil(t, BatchFromContext(context.Background()))

	members := []Member{{OriginalCommit: "a", Commit: "b", Actor: "agent-1", Rewritten: true}}
	ctx := WithBatch(context.Background(), members)
	require.Equal(t, members, BatchFromContext(ctx))
}

func TestMemberJSONKeys(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(Member{OriginalCommit: "a", Commit: "b", Actor: "agent", Rewritten: true, ResolvedPaths: []string{"x"}})
	require.NoError(t, err)
	require.JSONEq(t, `{"original_commit":"a","commit":"b","actor":"agent","rewritten":true,"resolved_paths":["x"]}`, string(encoded))
}

func TestAppendRecordsMembersTruncated(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	members := []Member{{OriginalCommit: "aaa", Commit: "bbb", Actor: "agent-1"}}

	_, err := Append(repoPath, Meta{Operation: OperationLandBatch, Members: members, MembersTruncated: true}, []Update{
		{Reference: "refs/heads/main", OldOID: "1", NewOID: "2"},
	})
	require.NoError(t, err)

	listed, err := List(repoPath, 0, 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.True(t, listed[0].MembersTruncated)
	require.Equal(t, members, listed[0].Members)
}
