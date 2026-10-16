package gitcmd_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/stretchr/testify/require"
)

func TestNewHooksPayload_changelogMembers(t *testing.T) {
	t.Parallel()

	t.Run("members come from the context", func(t *testing.T) {
		t.Parallel()

		members := []changelog.Member{
			{OriginalCommit: "aaa", Commit: "bbb", Actor: "agent-1", Rewritten: true, ResolvedPaths: []string{"a.txt"}},
			{OriginalCommit: "ccc", Commit: "ccc", Actor: "agent-2"},
		}
		ctx := changelog.WithBatch(changelog.WithOperation(context.Background(), changelog.OperationLandBatch), members)

		meta := changelogMetaFromPayload(t, newGraftTestPayload(t, ctx, &gitcmd.UserDetails{UserID: "user-1"}))

		require.Equal(t, changelog.OperationLandBatch, meta.Operation)
		require.Equal(t, members, meta.Members)
		require.False(t, meta.MembersTruncated)
	})

	t.Run("members are capped to the payload limit", func(t *testing.T) {
		t.Parallel()

		// 1,000 members of about 1 KiB each do not fit into the payload.
		members := make([]changelog.Member, 1000)
		for i := range members {
			members[i] = changelog.Member{
				OriginalCommit: strings.Repeat("a", 512),
				Commit:         strings.Repeat("b", 512),
				Actor:          "agent",
			}
		}
		ctx := changelog.WithBatch(context.Background(), members)

		meta := changelogMetaFromPayload(t, newGraftTestPayload(t, ctx, &gitcmd.UserDetails{UserID: "user-1"}))

		require.True(t, meta.MembersTruncated)
		require.NotEmpty(t, meta.Members)
		require.Less(t, len(meta.Members), len(members))
		require.Equal(t, members[:len(meta.Members)], meta.Members, "the kept members are a prefix of the batch")

		encoded, err := json.Marshal(meta.Members)
		require.NoError(t, err)
		require.LessOrEqual(t, len(encoded), 32<<10)
	})

	t.Run("resolved paths are dropped before members are cut", func(t *testing.T) {
		t.Parallel()

		resolved := make([]string, 200)
		for i := range resolved {
			resolved[i] = strings.Repeat("p", 100)
		}
		members := make([]changelog.Member, 10)
		for i := range members {
			members[i] = changelog.Member{OriginalCommit: "aaa", Commit: "bbb", Actor: "agent", ResolvedPaths: resolved}
		}
		ctx := changelog.WithBatch(context.Background(), members)

		meta := changelogMetaFromPayload(t, newGraftTestPayload(t, ctx, &gitcmd.UserDetails{UserID: "user-1"}))

		require.True(t, meta.MembersTruncated)
		require.Len(t, meta.Members, len(members), "all members fit once the resolved paths are dropped")
		for _, m := range meta.Members {
			require.Empty(t, m.ResolvedPaths)
			require.Equal(t, "bbb", m.Commit)
		}
	})

	t.Run("the batch is not modified when capped", func(t *testing.T) {
		t.Parallel()

		resolved := []string{strings.Repeat("p", 40<<10)}
		members := []changelog.Member{{OriginalCommit: "aaa", Commit: "bbb", ResolvedPaths: resolved}}
		ctx := changelog.WithBatch(context.Background(), members)

		_ = newGraftTestPayload(t, ctx, &gitcmd.UserDetails{UserID: "user-1"})

		require.Equal(t, resolved, members[0].ResolvedPaths)
	})
}

func TestNewHooksPayload_changelogMembersKeepInvalidUTF8Paths(t *testing.T) {
	t.Parallel()

	// Reference names and paths are bytes. JSON would turn each invalid byte into
	// U+FFFD, so the payload must carry the exact bytes of the path.
	members := []changelog.Member{
		{OriginalCommit: "aaa", Commit: "bbb", Actor: "agent-1", Rewritten: true, ResolvedPaths: []string{"dir/\xff\xfe.txt", "ok.txt"}},
		{OriginalCommit: "ccc", Commit: "ccc", Actor: "agent-2"},
	}
	ctx := changelog.WithBatch(changelog.WithOperation(context.Background(), changelog.OperationLandBatch), members)

	meta := changelogMetaFromPayload(t, newGraftTestPayload(t, ctx, &gitcmd.UserDetails{UserID: "user-1"}))

	require.Equal(t, members, meta.Members)
	require.False(t, meta.MembersTruncated)
	require.Equal(t, "dir/\xff\xfe.txt", meta.Members[0].ResolvedPaths[0])
}

func TestChangelogMeta_IsZero(t *testing.T) {
	t.Parallel()

	require.True(t, gitcmd.ChangelogMeta{}.IsZero())
	require.False(t, gitcmd.ChangelogMeta{Operation: "land"}.IsZero())
	require.False(t, gitcmd.ChangelogMeta{Members: []changelog.Member{{OriginalCommit: "a", Commit: "b"}}}.IsZero())
	require.False(t, gitcmd.ChangelogMeta{MembersTruncated: true}.IsZero())
}
