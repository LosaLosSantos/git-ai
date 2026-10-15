package sdk_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
)

func TestErrorHelpers_PlainErrors(t *testing.T) {
	t.Parallel()

	plain := errors.New("plain failure")

	require.Equal(t, codes.OK, sdk.Code(nil))
	require.Equal(t, codes.Unknown, sdk.Code(plain))
	require.False(t, sdk.IsNotFound(nil))
	require.False(t, sdk.IsNotFound(plain))

	_, ok := sdk.LandError(plain)
	require.False(t, ok)
	require.False(t, sdk.IsMergeConflict(plain))
	require.False(t, sdk.IsRejectedByHook(plain))

	_, ok = sdk.CommitFilesError(plain)
	require.False(t, ok)
}

func TestErrorHelpers_DetailsOfWrappedStatus(t *testing.T) {
	t.Parallel()

	st, err := status.New(codes.FailedPrecondition, "rejected").WithDetails(&gitalypb.UserCommitFilesError{
		Error: &gitalypb.UserCommitFilesError_CustomHook{
			CustomHook: &gitalypb.CustomHookError{},
		},
	})
	require.NoError(t, err)

	wrapped := errors.Join(errors.New("context"), st.Err())
	detail, ok := sdk.CommitFilesError(wrapped)
	require.True(t, ok)
	require.NotNil(t, detail.GetCustomHook())
	require.True(t, sdk.IsRejectedByHook(wrapped))
}
