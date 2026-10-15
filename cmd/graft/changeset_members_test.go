package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/graft/workspace"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// batchChangeset is a changeset of a batched land with two members, the first of
// which was rewritten and resolved and has a path that is not valid UTF-8.
func batchChangeset() workspace.Changeset {
	return workspace.Changeset{
		Sequence:   9,
		Reference:  "refs/heads/main",
		OldOID:     strings.Repeat("a", 40),
		NewOID:     strings.Repeat("b", 40),
		Operation:  "land_batch",
		Actor:      "graft-batch",
		RecordedAt: time.Unix(0, 0).UTC(),
		Members: []workspace.ChangesetMember{
			{
				OriginalCommit: strings.Repeat("c", 40),
				Commit:         strings.Repeat("d", 40),
				Actor:          "agent-1",
				Rewritten:      true,
				ResolvedPaths:  []string{"dir/a.txt", "\xff\xfe.txt"},
			},
			{
				OriginalCommit: strings.Repeat("e", 40),
				Commit:         strings.Repeat("e", 40),
				Actor:          "agent-2",
			},
		},
	}
}

// TestPrintChangesetBatchMembers checks that graft log prints a batch of N and one
// "actor original→landed" line per member, below the line of the update.
func TestPrintChangesetBatchMembers(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	printChangeset(&out, batchChangeset())

	require.Equal(t, strings.Join([]string{
		"9\trefs/heads/main\taaaaaaaaaaaa..bbbbbbbbbbbb\tland_batch\tgraft-batch\t1970-01-01T00:00:00Z",
		"  batch of 2",
		`    agent-1 cccccccccccc→dddddddddddd rewritten resolved: dir/a.txt, \xff\xfe.txt`,
		"    agent-2 eeeeeeeeeeee→eeeeeeeeeeee",
		"",
	}, "\n"), out.String())
}

// TestPrintChangesetWithoutMembers checks that an update that is not a batch prints
// one line, as before.
func TestPrintChangesetWithoutMembers(t *testing.T) {
	t.Parallel()

	c := batchChangeset()
	c.Members = nil

	var out bytes.Buffer
	printChangeset(&out, c)

	require.Equal(t, 1, strings.Count(out.String(), "\n"))
	require.NotContains(t, out.String(), "batch of")
}

// TestEmitChangesetJSONMembers checks that --json carries the members in the form
// of the changeset log, with the exact bytes of a path that is not valid UTF-8.
func TestEmitChangesetJSONMembers(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	require.NoError(t, emitChangeset(&out, &options{jsonOut: true}, batchChangeset()))

	line := out.String()
	require.Equal(t, 1, strings.Count(line, "\n"), "one object per line")

	var got struct {
		Members []map[string]any `json:"members"`
	}
	require.NoError(t, json.Unmarshal([]byte(line), &got))
	require.Len(t, got.Members, 2)

	// The readable form replaces the invalid bytes, and the base64 form is exact.
	require.Equal(t, map[string]any{
		"original_commit": strings.Repeat("c", 40),
		"commit":          strings.Repeat("d", 40),
		"actor":           "agent-1",
		"rewritten":       true,
		"resolved_paths":  []any{"dir/a.txt", "�.txt"},
		"resolved_paths_b64": []any{
			base64.StdEncoding.EncodeToString([]byte("dir/a.txt")),
			base64.StdEncoding.EncodeToString([]byte("\xff\xfe.txt")),
		},
	}, got.Members[0])

	require.Equal(t, map[string]any{
		"original_commit": strings.Repeat("e", 40),
		"commit":          strings.Repeat("e", 40),
		"actor":           "agent-2",
	}, got.Members[1])
}

// TestEmitChangesetJSONWithoutMembers checks that an update without members has
// no members key, as before.
func TestEmitChangesetJSONWithoutMembers(t *testing.T) {
	t.Parallel()

	c := batchChangeset()
	c.Members = nil

	var out bytes.Buffer
	require.NoError(t, emitChangeset(&out, &options{jsonOut: true}, c))
	require.NotContains(t, out.String(), "members")
}

// TestToChangesetMembers checks that the members of a changeset from the server
// reach the workspace changeset, with their paths byte for byte.
func TestToChangesetMembers(t *testing.T) {
	t.Parallel()

	got := toChangeset(&gitalypb.Changeset{
		Sequence:  3,
		Operation: "land_batch",
		Members: []*gitalypb.ChangesetMember{
			{
				OriginalCommitId: "aa",
				CommitId:         "bb",
				Actor:            "agent-1",
				Rewritten:        true,
				ResolvedPaths:    [][]byte{{0xff, 'x'}},
			},
			{OriginalCommitId: "cc", CommitId: "cc", Actor: "agent-2"},
		},
	})

	require.Equal(t, []workspace.ChangesetMember{
		{OriginalCommit: "aa", Commit: "bb", Actor: "agent-1", Rewritten: true, ResolvedPaths: []string{"\xffx"}},
		{OriginalCommit: "cc", Commit: "cc", Actor: "agent-2"},
	}, got.Members)

	require.Nil(t, toChangeset(&gitalypb.Changeset{Sequence: 4}).Members)
}
