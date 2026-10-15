package changelog

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateMembers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc    string
		members []Member
		errMsg  string
	}{
		{desc: "no members", members: nil},
		{
			desc:    "valid members",
			members: []Member{{OriginalCommit: "a", Commit: "a"}, {OriginalCommit: "b", Commit: "c", Rewritten: true}},
		},
		{
			desc:    "missing original commit",
			members: []Member{{Commit: "a"}},
			errMsg:  "member 0: original commit is empty",
		},
		{
			desc:    "missing landed commit",
			members: []Member{{OriginalCommit: "a", Commit: "a"}, {OriginalCommit: "b"}},
			errMsg:  "member 1: commit is empty",
		},
		{
			desc:    "rewritten without a new commit",
			members: []Member{{OriginalCommit: "a", Commit: "a", Rewritten: true}},
			errMsg:  "member 0: marked rewritten but commit equals original commit",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			err := ValidateMembers(tc.members)
			if tc.errMsg == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.errMsg)
		})
	}
}

func TestMemberResolved(t *testing.T) {
	t.Parallel()

	require.False(t, Member{}.Resolved())
	require.True(t, Member{ResolvedPaths: []string{"README.md"}}.Resolved())
}

func TestNormalizeMembers(t *testing.T) {
	t.Parallel()

	require.Nil(t, normalizeMembers(nil))

	in := []Member{{
		OriginalCommit: "a",
		Commit:         "b",
		Actor:          "agent\xff",
		ResolvedPaths:  []string{"dir/\xfe.txt"},
	}}
	out := normalizeMembers(in)

	require.Equal(t, "agent�", out[0].Actor)
	// Resolved paths are kept byte for byte, see TestMemberJSONKeepsInvalidUTF8Paths.
	require.Equal(t, []string{"dir/\xfe.txt"}, out[0].ResolvedPaths)
	// The input must not be modified.
	require.Equal(t, "agent\xff", in[0].Actor)
	in[0].ResolvedPaths[0] = "changed"
	require.Equal(t, []string{"dir/\xfe.txt"}, out[0].ResolvedPaths, "the output must not share the input's paths")
}

func TestMemberJSONKeepsInvalidUTF8Paths(t *testing.T) {
	t.Parallel()

	// A valid member keeps the plain encoding, without the base64 key.
	valid, err := json.Marshal(Member{OriginalCommit: "a", Commit: "b", ResolvedPaths: []string{"dir/ü.txt"}})
	require.NoError(t, err)
	require.JSONEq(t, `{"original_commit":"a","commit":"b","resolved_paths":["dir/ü.txt"]}`, string(valid))

	in := []Member{
		{OriginalCommit: "a", Commit: "b", Actor: "agent", ResolvedPaths: []string{"dir/\xff\xfe.txt", "ok.txt", "\xc3"}},
		{OriginalCommit: "c", Commit: "c"},
	}

	raw, err := json.Marshal(in)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"resolved_paths_b64"`)

	var out []Member
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, in, out, "invalid UTF-8 paths must survive the JSON round trip byte for byte")
	require.Equal(t, "dir/\xff\xfe.txt", out[0].ResolvedPaths[0])
	require.Nil(t, out[1].ResolvedPaths)
}

func TestMemberJSONRejectsMismatchedBase64Paths(t *testing.T) {
	t.Parallel()

	var m Member
	err := json.Unmarshal([]byte(`{"original_commit":"a","commit":"b","resolved_paths":["x","y"],"resolved_paths_b64":["eA=="]}`), &m)
	require.ErrorContains(t, err, "resolved_paths_b64 has 1 paths, resolved_paths has 2")

	err = json.Unmarshal([]byte(`{"original_commit":"a","commit":"b","resolved_paths":["x"],"resolved_paths_b64":["!!"]}`), &m)
	require.ErrorContains(t, err, "decoding resolved_paths_b64")
}

func TestFormatMembers(t *testing.T) {
	t.Parallel()

	lines := FormatMembers([]Member{
		{OriginalCommit: "0123456789abcdef", Commit: "0123456789abcdef", Actor: "agent-1"},
		{OriginalCommit: "fedcba9876543210", Commit: "1111222233334444", Actor: "agent-2", Rewritten: true, ResolvedPaths: []string{"a.go", "b.go"}},
		{OriginalCommit: "abc", Commit: "abc"},
	})

	require.Equal(t, []string{
		"01234567 -> 01234567 by agent-1",
		"fedcba98 -> 11112222 by agent-2 rewritten resolved: a.go, b.go",
		"abc -> abc",
	}, lines)
}

func TestAppendRecordsMembers(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	members := []Member{
		{OriginalCommit: "aaa", Commit: "bbb", Actor: "agent-1", Rewritten: true},
		{OriginalCommit: "ccc", Commit: "ccc", Actor: "agent-2"},
	}

	entries, err := Append(repoPath, Meta{Operation: "land", Actor: "batch", Members: members}, []Update{
		{Reference: "refs/heads/main", OldOID: "1", NewOID: "2"},
		{Reference: "refs/heads/other", OldOID: "3", NewOID: "4"},
	})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	for _, e := range entries {
		require.Equal(t, members, e.Members)
	}

	// The members survive the round trip through the log file.
	listed, err := List(repoPath, 0, 10)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	require.Equal(t, members, listed[1].Members)

	// An entry without members has no members in the log.
	_, err = Append(repoPath, Meta{Operation: "ff_branch"}, []Update{{Reference: "refs/heads/x", NewOID: "5"}})
	require.NoError(t, err)
	listed, err = List(repoPath, 2, 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Nil(t, listed[0].Members)
}

func TestAppendDropsInvalidMembers(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	// The reference update has happened already, so its entry is recorded even
	// though one member is malformed. The malformed member is dropped and flagged.
	members := []Member{
		{OriginalCommit: "a", Commit: "a1", Actor: "agent-1"},
		{Commit: "b"},
		{OriginalCommit: "c", Commit: "c", Rewritten: true},
		{OriginalCommit: "d", Commit: "d2", Actor: "agent-4"},
	}

	entries, err := Append(repoPath, Meta{Operation: "land_batch", Members: members}, []Update{{Reference: "refs/heads/main", NewOID: "2"}})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].MembersTruncated)
	require.Equal(t, []Member{members[0], members[3]}, entries[0].Members)

	listed, err := List(repoPath, 0, 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, entries[0].Members, listed[0].Members)
	require.True(t, listed[0].MembersTruncated)

	// A batch whose members are all malformed is still recorded, flagged as truncated.
	_, err = Append(repoPath, Meta{Operation: "land_batch", Members: []Member{{Commit: "x"}}}, []Update{{Reference: "refs/heads/main", NewOID: "3"}})
	require.NoError(t, err)
	listed, err = List(repoPath, 1, 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Nil(t, listed[0].Members)
	require.True(t, listed[0].MembersTruncated)
}

func TestAppendKeepsInvalidUTF8ResolvedPaths(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	members := []Member{{OriginalCommit: "aaa", Commit: "bbb", Actor: "agent-1", ResolvedPaths: []string{"dir/\xff\xfe.txt"}}}

	entries, err := Append(repoPath, Meta{Operation: "land_batch", Members: members}, []Update{
		{Reference: "refs/heads/main", OldOID: "1", NewOID: "2"},
	})
	require.NoError(t, err)
	require.Equal(t, members, entries[0].Members, "the returned entry matches the recorded one")

	listed, err := List(repoPath, 0, 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, members, listed[0].Members, "the log returns the resolved path byte for byte")
}
