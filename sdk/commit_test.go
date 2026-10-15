package sdk_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
)

func validCommit() sdk.CommitOptions {
	return sdk.CommitOptions{
		Branch:  "refs/heads/feature",
		Message: "add files",
		Author:  sdk.Signature{Name: "Ada", Email: "ada@example.com"},
		User:    sdk.User{ID: "user-7", Name: "Ada", Email: "ada@example.com", Username: "ada"},
		Actions: []sdk.Action{
			sdk.Put{Path: "a.txt", Content: []byte("A")},
		},
	}
}

func TestRepo_CommitFiles_StreamsHeaderAndActions(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.commits.revisions = map[string]string{"base": "base-sha"}
	repo := dialFake(t, fakes.register).Repo("repo")

	big := bytes.Repeat([]byte("x"), 150*1024) // spans three content messages

	opts := validCommit()
	opts.StartRev = "base"
	opts.ExpectedOldOID = "old-sha"
	opts.Timestamp = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	opts.Actions = []sdk.Action{
		sdk.Put{Path: "big.bin", Content: big, Executable: true},
		&sdk.Put{Path: "gen.txt", Content: []byte("v2"), Overwrite: true},
		sdk.Delete{Path: "old.txt"},
		sdk.Move{From: "from.txt", To: "to.txt"},
		sdk.Chmod{Path: "run.sh", Executable: true},
		&sdk.Delete{Path: "gone.txt"},
	}

	result, err := repo.CommitFiles(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, &sdk.CommitResult{CommitID: "new-commit", BranchCreated: true}, result)

	require.Len(t, fakes.operations.commitHeaders, 1)
	header := fakes.operations.commitHeaders[0]
	require.Equal(t, []byte("feature"), header.GetBranchName())
	require.Equal(t, []byte("add files"), header.GetCommitMessage())
	require.Equal(t, []byte("Ada"), header.GetCommitAuthorName())
	require.Equal(t, []byte("ada@example.com"), header.GetCommitAuthorEmail())
	require.Equal(t, "base-sha", header.GetStartSha())
	require.Equal(t, "old-sha", header.GetExpectedOldOid())
	require.Equal(t, "user-7", header.GetUser().GetGlId())
	require.Equal(t, "ada", header.GetUser().GetGlUsername())
	require.Equal(t, int64(1767323045), header.GetTimestamp().GetSeconds())
	require.Equal(t, "default", header.GetRepository().GetStorageName())
	require.Equal(t, "repo", header.GetRepository().GetRelativePath())

	actions := fakes.operations.commitActions
	require.Len(t, actions, 6)

	require.Equal(t, gitalypb.UserCommitFilesActionHeader_CREATE, actions[0].header.GetAction())
	require.Equal(t, []byte("big.bin"), actions[0].header.GetFilePath())
	require.True(t, actions[0].header.GetExecuteFilemode())
	require.Equal(t, big, actions[0].content)

	require.Equal(t, gitalypb.UserCommitFilesActionHeader_UPDATE, actions[1].header.GetAction())
	require.Equal(t, []byte("gen.txt"), actions[1].header.GetFilePath())
	require.Equal(t, []byte("v2"), actions[1].content)

	require.Equal(t, gitalypb.UserCommitFilesActionHeader_DELETE, actions[2].header.GetAction())
	require.Equal(t, []byte("old.txt"), actions[2].header.GetFilePath())

	require.Equal(t, gitalypb.UserCommitFilesActionHeader_MOVE, actions[3].header.GetAction())
	require.Equal(t, []byte("to.txt"), actions[3].header.GetFilePath())
	require.Equal(t, []byte("from.txt"), actions[3].header.GetPreviousPath())
	require.True(t, actions[3].header.GetInferContent())

	require.Equal(t, gitalypb.UserCommitFilesActionHeader_CHMOD, actions[4].header.GetAction())
	require.True(t, actions[4].header.GetExecuteFilemode())

	require.Equal(t, gitalypb.UserCommitFilesActionHeader_DELETE, actions[5].header.GetAction())
	require.Equal(t, []byte("gone.txt"), actions[5].header.GetFilePath())
}

func TestRepo_CommitFiles_BranchDefaults(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	repo := dialFake(t, fakes.register).Repo("repo")

	_, err := repo.CommitFiles(t.Context(), validCommit())
	require.NoError(t, err)

	header := fakes.operations.commitHeaders[0]
	require.Empty(t, header.GetStartSha(), "without StartRev the server uses the branch tip")
	require.Empty(t, header.GetExpectedOldOid())
	require.Nil(t, header.GetTimestamp())
}

func TestRepo_CommitFiles_IndexErrorIsFailedPrecondition(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.operations.indexError = "A file with this name already exists"
	repo := dialFake(t, fakes.register).Repo("repo")

	_, err := repo.CommitFiles(t.Context(), validCommit())
	require.Equal(t, codes.FailedPrecondition, sdk.Code(err))
	require.ErrorContains(t, err, "already exists")
}

func TestRepo_CommitFiles_Validation(t *testing.T) {
	t.Parallel()

	repo := dialFake(t, newFakeServices().register).Repo("repo")

	tests := []struct {
		name   string
		modify func(*sdk.CommitOptions)
	}{
		{name: "missing branch", modify: func(o *sdk.CommitOptions) { o.Branch = "refs/heads/" }},
		{name: "missing message", modify: func(o *sdk.CommitOptions) { o.Message = "" }},
		{name: "no actions", modify: func(o *sdk.CommitOptions) { o.Actions = nil }},
		{name: "nil action", modify: func(o *sdk.CommitOptions) { o.Actions = []sdk.Action{nil} }},
		{name: "put without path", modify: func(o *sdk.CommitOptions) { o.Actions = []sdk.Action{sdk.Put{}} }},
		{name: "move without target", modify: func(o *sdk.CommitOptions) {
			o.Actions = []sdk.Action{sdk.Move{From: "a"}}
		}},
		{name: "chmod without path", modify: func(o *sdk.CommitOptions) { o.Actions = []sdk.Action{sdk.Chmod{}} }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := validCommit()
			tc.modify(&opts)

			_, err := repo.CommitFiles(t.Context(), opts)
			require.Equal(t, codes.InvalidArgument, sdk.Code(err))
		})
	}
}
