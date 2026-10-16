package ref

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/helper"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestSuccessfulGetTagMessagesRequest(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

	message1 := strings.Repeat("a", helper.MaxCommitOrTagMessageSize*2)
	message2 := strings.Repeat("b", helper.MaxCommitOrTagMessageSize)

	commit1 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("big-tag-1"))
	commit2 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("big-tag-2"))
	tag1ID := gittest.WriteTag(t, cfg, repoPath, "big-tag-1", commit1.Revision(), gittest.WriteTagConfig{Message: message1})
	tag2ID := gittest.WriteTag(t, cfg, repoPath, "big-tag-2", commit2.Revision(), gittest.WriteTagConfig{Message: message2})

	request := &gitalypb.GetTagMessagesRequest{
		Repository: repo,
		TagIds:     []string{tag1ID.String(), tag2ID.String()},
	}

	expectedMessages := []*gitalypb.GetTagMessagesResponse{
		{
			TagId:   tag1ID.String(),
			Message: []byte(message1 + "\n"),
		},
		{
			TagId:   tag2ID.String(),
			Message: []byte(message2 + "\n"),
		},
	}

	c, err := client.GetTagMessages(ctx, request)
	require.NoError(t, err)

	fetchedMessages := readAllMessagesFromClient(t, c)
	require.Len(t, fetchedMessages, len(expectedMessages))
	testhelper.ProtoEqual(t, expectedMessages[0], fetchedMessages[0])
	testhelper.ProtoEqual(t, expectedMessages[1], fetchedMessages[1])
}

func TestFailedGetTagMessagesRequest(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	_, client := setupRefService(t)

	testCases := []struct {
		desc        string
		request     *gitalypb.GetTagMessagesRequest
		expectedErr error
	}{
		{
			desc: "unset repository",
			request: &gitalypb.GetTagMessagesRequest{
				Repository: nil,
				TagIds:     []string{"5937ac0a7beb003549fc5fd26fc247adbce4a52e"},
			},
			expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.desc, func(t *testing.T) {
			c, err := client.GetTagMessages(ctx, testCase.request)
			require.NoError(t, err)
			_, err = c.Recv()
			testhelper.RequireGrpcError(t, testCase.expectedErr, err)
		})
	}
}

func readAllMessagesFromClient(t *testing.T, c gitalypb.RefService_GetTagMessagesClient) (messages []*gitalypb.GetTagMessagesResponse) {
	for {
		resp, err := c.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)

		if resp.GetTagId() != "" {
			messages = append(messages, resp)
			// first message contains a chunk of the message, so no need to append anything
			continue
		}

		currentMessage := messages[len(messages)-1]
		currentMessage.Message = append(currentMessage.Message, resp.GetMessage()...)
	}

	return messages
}
