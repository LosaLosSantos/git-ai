package sdk_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// fastRetry is the retry of Unavailable with a backoff short enough for tests.
var fastRetry = sdk.UnavailableRetry{Base: time.Millisecond, Cap: 2 * time.Millisecond}

// mergeLand is a land with LandMerge, which RetryUnavailable retries by default.
func mergeLand() sdk.LandOptions {
	opts := validLand()
	opts.Strategy = sdk.LandMerge
	return opts
}

func TestRepo_Land_RebaseLandsAreRetried(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc             string
		resolve          bool
		retry            sdk.UnavailableRetry
		expectedRequests int
	}{
		{desc: "rebase", retry: fastRetry, expectedRequests: 6},
		{desc: "rebase with conflict resolution", resolve: true, retry: fastRetry, expectedRequests: 6},
		{desc: "rebase, opted out", retry: sdk.UnavailableRetry{SkipRebaseLands: true, Base: time.Millisecond, Cap: 2 * time.Millisecond}, expectedRequests: 1},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			fakes := newFakeServices()
			fakes.operations.landErr = status.Error(codes.Unavailable, "still syncing")
			repo := dialFake(t, fakes.register).Repo("repo")

			opts := validLand()
			opts.Strategy = sdk.LandRebase
			opts.ResolveConflicts = tc.resolve
			opts.RetryUnavailable = tc.retry
			_, err := repo.Land(t.Context(), opts)
			require.Equal(t, codes.Unavailable, sdk.Code(err))
			require.Len(t, fakes.operations.landRequests, tc.expectedRequests)
		})
	}
}

func TestRepo_Land_RetriesUnavailable(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.operations.landErrs = []error{
		status.Error(codes.Unavailable, "storage core: waiting for the sync"),
		status.Error(codes.Unavailable, "the land lane is closed"),
	}
	fakes.operations.landResponse = &gitalypb.UserLandResponse{LandedCommitId: "landed-sha", Attempts: 1}
	repo := dialFake(t, fakes.register).Repo("repo")

	opts := mergeLand()
	opts.RetryUnavailable = fastRetry
	result, err := repo.Land(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, "landed-sha", result.LandedCommitID)
	require.Equal(t, 2, result.UnavailableRetries)
	require.Equal(t, "the land lane is closed", result.LastUnavailable)
	require.Len(t, fakes.operations.landRequests, 3)
}

func TestRepo_Land_UnavailableRetriesAreBounded(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc             string
		retry            sdk.UnavailableRetry
		expectedRequests int
	}{
		{desc: "default", retry: fastRetry, expectedRequests: 6},
		{desc: "max retries", retry: sdk.UnavailableRetry{MaxRetries: 2, Base: time.Millisecond}, expectedRequests: 3},
		{desc: "disabled", retry: sdk.UnavailableRetry{Disabled: true}, expectedRequests: 1},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			fakes := newFakeServices()
			fakes.operations.landErr = status.Error(codes.Unavailable, "still syncing")
			repo := dialFake(t, fakes.register).Repo("repo")

			opts := mergeLand()
			opts.RetryUnavailable = tc.retry
			_, err := repo.Land(t.Context(), opts)
			require.Equal(t, codes.Unavailable, sdk.Code(err), "the last Unavailable is returned")
			require.Len(t, fakes.operations.landRequests, tc.expectedRequests)

			// The retries are reported with the failure.
			var retriesErr *sdk.UnavailableRetriesError
			if tc.expectedRequests == 1 {
				require.False(t, errors.As(err, &retriesErr), "a land that was not retried returns its error as is")
				return
			}
			require.ErrorAs(t, err, &retriesErr)
			require.Equal(t, tc.expectedRequests-1, retriesErr.Retries)
			require.Equal(t, "still syncing", retriesErr.LastUnavailable)
		})
	}
}

func TestRepo_Land_FailureAfterRetriesKeepsItsStatus(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.operations.landErrs = []error{status.Error(codes.Unavailable, "the land lane is closed")}
	fakes.operations.landErr = status.Error(codes.Aborted, "giving up after 20 attempts")
	repo := dialFake(t, fakes.register).Repo("repo")

	opts := mergeLand()
	opts.RetryUnavailable = fastRetry
	_, err := repo.Land(t.Context(), opts)
	require.Equal(t, codes.Aborted, sdk.Code(err), "the status of the last error")
	require.Contains(t, status.Convert(err).Message(), "giving up after 20 attempts")
	var retriesErr *sdk.UnavailableRetriesError
	require.ErrorAs(t, err, &retriesErr)
	require.Equal(t, 1, retriesErr.Retries)
	require.Equal(t, "the land lane is closed", retriesErr.LastUnavailable)
}

func TestRepo_Land_NegativeRetriesAreInvalid(t *testing.T) {
	t.Parallel()

	for _, retry := range []sdk.UnavailableRetry{{MaxRetries: -1}, {Base: -time.Second}, {Cap: -time.Second}} {
		fakes := newFakeServices()
		repo := dialFake(t, fakes.register).Repo("repo")

		opts := mergeLand()
		opts.RetryUnavailable = retry
		_, err := repo.Land(t.Context(), opts)
		require.Equal(t, codes.InvalidArgument, sdk.Code(err), "%+v", retry)
		require.Empty(t, fakes.operations.landRequests)
	}
}

func TestRepo_Land_RetriesOnlyUnavailable(t *testing.T) {
	t.Parallel()

	for _, code := range []codes.Code{codes.DeadlineExceeded, codes.Internal, codes.Unknown, codes.Aborted, codes.FailedPrecondition} {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()

			fakes := newFakeServices()
			fakes.operations.landErr = status.Error(code, "not to be retried")
			repo := dialFake(t, fakes.register).Repo("repo")

			opts := mergeLand()
			opts.RetryUnavailable = fastRetry
			_, err := repo.Land(t.Context(), opts)
			require.Equal(t, code, sdk.Code(err))
			require.Len(t, fakes.operations.landRequests, 1)
		})
	}
}

func TestRepo_Land_ContextStopsTheRetries(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.operations.landErr = status.Error(codes.Unavailable, "still syncing")

	// The context ends once the client has the first Unavailable, so during the wait before the
	// first retry, whatever the time the call took.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelAfterUnavailable := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		err := invoker(ctx, method, req, reply, cc, opts...)
		if status.Code(err) == codes.Unavailable {
			cancel()
		}
		return err
	}
	repo := dialFake(t, fakes.register, sdk.WithGRPCOptions(grpc.WithUnaryInterceptor(cancelAfterUnavailable))).Repo("repo")

	opts := mergeLand()
	opts.RetryUnavailable = sdk.UnavailableRetry{Base: time.Hour, Cap: time.Hour}
	_, err := repo.Land(ctx, opts)
	require.Equal(t, codes.Canceled, sdk.Code(err))
	require.Len(t, fakes.operations.landRequests, 1)

	// The error keeps what the server answered last.
	var retriesErr *sdk.UnavailableRetriesError
	require.ErrorAs(t, err, &retriesErr)
	require.Equal(t, "still syncing", retriesErr.LastUnavailable)
	require.Contains(t, err.Error(), "still syncing")
}

func TestRepo_Land_RetriedResolveLandReportsItsOutcome(t *testing.T) {
	t.Parallel()

	// The first call landed the change but its answer was lost to an Unavailable: the retry,
	// which may run the resolver again, finds the change on the branch.
	fakes := newFakeServices()
	fakes.operations.landErrs = []error{status.Error(codes.Unavailable, "connection reset")}
	fakes.operations.landResponse = &gitalypb.UserLandResponse{
		LandedCommitId: "tip-sha",
		AlreadyLanded:  true,
		OldTip:         "tip-sha",
		BatchSize:      3,
		BatchPosition:  2,
	}
	repo := dialFake(t, fakes.register).Repo("repo")

	opts := mergeLand()
	opts.ResolveConflicts = true
	opts.RetryUnavailable = fastRetry
	result, err := repo.Land(t.Context(), opts)
	require.NoError(t, err)
	require.True(t, result.AlreadyLanded)
	require.Equal(t, "tip-sha", result.LandedCommitID)
	require.Equal(t, sdk.BatchInfo{Size: 3, Position: 2}, result.Batch)
	require.Equal(t, 1, result.UnavailableRetries)

	require.Len(t, fakes.operations.landRequests, 2)
	for _, req := range fakes.operations.landRequests {
		require.True(t, req.GetResolveConflicts(), "every call asks for the resolution")
	}
}

func TestRepo_Land_RetriedRebaseReportsTheEarlierLand(t *testing.T) {
	t.Parallel()

	// The first call rebased the change but its answer was lost to an Unavailable: the retry
	// answers from the record of that land, with the commit it landed as and the revert after it.
	fakes := newFakeServices()
	fakes.operations.landErrs = []error{status.Error(codes.Unavailable, "connection reset")}
	fakes.operations.landResponse = &gitalypb.UserLandResponse{
		BranchUpdate:        &gitalypb.OperationBranchUpdate{CommitId: "tip-sha"},
		LandedCommitId:      "copy-sha",
		AlreadyLanded:       true,
		OldTip:              "tip-sha",
		LandedSequence:      42,
		RevertedByCommitId:  "revert-sha",
		AlreadyLandedDetail: "landed as copy-sha at changelog 42; it was reverted later by revert-sha",
	}
	repo := dialFake(t, fakes.register).Repo("repo")

	opts := validLand()
	opts.Strategy = sdk.LandRebase
	opts.RetryUnavailable = fastRetry
	result, err := repo.Land(t.Context(), opts)
	require.NoError(t, err)
	require.True(t, result.AlreadyLanded)
	require.Equal(t, "copy-sha", result.LandedCommitID)
	require.Equal(t, uint64(42), result.LandedSequence)
	require.Equal(t, "revert-sha", result.RevertedByCommitID)
	require.Equal(t, "landed as copy-sha at changelog 42; it was reverted later by revert-sha", result.AlreadyLandedDetail)
	require.Equal(t, 1, result.UnavailableRetries)
	require.Len(t, fakes.operations.landRequests, 2)
}
