package sdk_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

func TestRepo_Land_RebaseLandsAreNotRetriedByDefault(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc             string
		resolve          bool
		retry            sdk.UnavailableRetry
		expectedRequests int
	}{
		{desc: "rebase", retry: fastRetry, expectedRequests: 1},
		{desc: "rebase with conflict resolution", resolve: true, retry: fastRetry, expectedRequests: 1},
		{desc: "rebase, opted in", retry: sdk.UnavailableRetry{RebaseLands: true, Base: time.Millisecond, Cap: 2 * time.Millisecond}, expectedRequests: 6},
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
		})
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
	repo := dialFake(t, fakes.register).Repo("repo")

	// The backoff is longer than the context, which ends during the first wait.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	opts := mergeLand()
	opts.RetryUnavailable = sdk.UnavailableRetry{Base: time.Hour, Cap: time.Hour}
	// Each wait is drawn from [0, 1h]: the context ends during one of them unless five draws in
	// a row fall below 50ms.
	_, err := repo.Land(ctx, opts)
	require.Equal(t, codes.DeadlineExceeded, sdk.Code(err))
	require.Less(t, len(fakes.operations.landRequests), 6)
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
