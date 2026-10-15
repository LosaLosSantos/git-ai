package sdk_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
)

func validLand() sdk.LandOptions {
	return sdk.LandOptions{
		Branch: "refs/heads/main",
		Commit: "change-sha",
		User:   sdk.User{ID: "user-1", Name: "Ada", Email: "ada@example.com"},
	}
}

func TestRepo_Land_SendsRequestAndMapsResponse(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.operations.landResponse = &gitalypb.UserLandResponse{
		BranchUpdate:   &gitalypb.OperationBranchUpdate{CommitId: "landed-sha", BranchCreated: false},
		LandedCommitId: "landed-sha",
		Rewritten:      true,
		Attempts:       3,
		OldTip:         "old-tip-sha",
	}
	repo := dialFake(t, fakes.register).Repo("repo")

	opts := validLand()
	opts.Strategy = sdk.LandMerge
	opts.MaxAttempts = 7
	opts.CreateBranch = true
	opts.MergeMessage = "merge it"
	opts.Timestamp = time.Unix(1700000000, 0)

	result, err := repo.Land(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, &sdk.LandResult{
		LandedCommitID: "landed-sha",
		OldTip:         "old-tip-sha",
		Rewritten:      true,
		Attempts:       3,
	}, result)

	require.Len(t, fakes.operations.landRequests, 1)
	req := fakes.operations.landRequests[0]
	require.Equal(t, []byte("main"), req.GetBranch())
	require.Equal(t, "change-sha", req.GetCommitId())
	require.Equal(t, gitalypb.UserLandRequest_STRATEGY_MERGE, req.GetStrategy())
	require.Equal(t, int32(7), req.GetMaxAttempts())
	require.True(t, req.GetCreateBranch())
	require.Equal(t, []byte("merge it"), req.GetMergeMessage())
	require.Equal(t, int64(1700000000), req.GetTimestamp().GetSeconds())
	require.Equal(t, "user-1", req.GetUser().GetGlId())
	require.Equal(t, "default", req.GetRepository().GetStorageName())
}

func TestRepo_Land_AlreadyLanded(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.operations.landResponse = &gitalypb.UserLandResponse{
		LandedCommitId: "tip-sha",
		AlreadyLanded:  true,
		OldTip:         "tip-sha",
	}
	repo := dialFake(t, fakes.register).Repo("repo")

	result, err := repo.Land(t.Context(), validLand())
	require.NoError(t, err)
	require.True(t, result.AlreadyLanded)
	require.False(t, result.Rewritten)
	require.Equal(t, "tip-sha", result.LandedCommitID)
}

func TestRepo_Land_BranchCreated(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.operations.landResponse = &gitalypb.UserLandResponse{
		BranchUpdate:   &gitalypb.OperationBranchUpdate{CommitId: "change-sha", BranchCreated: true},
		LandedCommitId: "change-sha",
	}
	repo := dialFake(t, fakes.register).Repo("repo")

	result, err := repo.Land(t.Context(), validLand())
	require.NoError(t, err)
	require.True(t, result.BranchCreated)
}

func TestRepo_Land_StrategyDefaultsToRebase(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.operations.landResponse = &gitalypb.UserLandResponse{}
	repo := dialFake(t, fakes.register).Repo("repo")

	_, err := repo.Land(t.Context(), validLand())
	require.NoError(t, err)
	require.Equal(t, gitalypb.UserLandRequest_STRATEGY_REBASE_UNSPECIFIED, fakes.operations.landRequests[0].GetStrategy())
}

func TestRepo_Land_MergeConflictDetails(t *testing.T) {
	t.Parallel()

	st, err := status.New(codes.FailedPrecondition, "merge conflict").WithDetails(&gitalypb.UserLandError{
		Error: &gitalypb.UserLandError_MergeConflict{
			MergeConflict: &gitalypb.MergeConflictError{
				ConflictingFiles: [][]byte{[]byte("a.txt")},
			},
		},
	})
	require.NoError(t, err)

	fakes := newFakeServices()
	fakes.operations.landErr = st.Err()
	repo := dialFake(t, fakes.register).Repo("repo")

	_, err = repo.Land(t.Context(), validLand())
	require.Equal(t, codes.FailedPrecondition, sdk.Code(err))
	require.True(t, sdk.IsMergeConflict(err))
	require.False(t, sdk.IsNotAncestor(err))
	require.False(t, sdk.IsRejectedByHook(err))

	landErr, ok := sdk.LandError(err)
	require.True(t, ok)
	require.Equal(t, [][]byte{[]byte("a.txt")}, landErr.GetMergeConflict().GetConflictingFiles())
}

func TestRepo_Land_FailureKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		detail *gitalypb.UserLandError
		check  func(error) bool
	}{
		{
			name: "not ancestor",
			detail: &gitalypb.UserLandError{Error: &gitalypb.UserLandError_NotAncestor{
				NotAncestor: &gitalypb.NotAncestorError{},
			}},
			check: sdk.IsNotAncestor,
		},
		{
			name: "reference update exhausted",
			detail: &gitalypb.UserLandError{Error: &gitalypb.UserLandError_ReferenceUpdate{
				ReferenceUpdate: &gitalypb.ReferenceUpdateError{ReferenceName: []byte("refs/heads/main")},
			}},
			check: sdk.IsReferenceUpdateConflict,
		},
		{
			name: "custom hook",
			detail: &gitalypb.UserLandError{Error: &gitalypb.UserLandError_CustomHook{
				CustomHook: &gitalypb.CustomHookError{},
			}},
			check: sdk.IsRejectedByHook,
		},
		{
			name: "access check",
			detail: &gitalypb.UserLandError{Error: &gitalypb.UserLandError_AccessCheck{
				AccessCheck: &gitalypb.AccessCheckError{ErrorMessage: "denied"},
			}},
			check: sdk.IsRejectedByHook,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st, err := status.New(codes.FailedPrecondition, tc.name).WithDetails(tc.detail)
			require.NoError(t, err)

			fakes := newFakeServices()
			fakes.operations.landErr = st.Err()
			repo := dialFake(t, fakes.register).Repo("repo")

			_, err = repo.Land(t.Context(), validLand())
			require.True(t, tc.check(err))
		})
	}
}

func TestRepo_Land_ResolveConflicts(t *testing.T) {
	t.Parallel()

	t.Run("sends the option and maps the resolution", func(t *testing.T) {
		t.Parallel()

		fakes := newFakeServices()
		fakes.operations.landResponse = &gitalypb.UserLandResponse{
			LandedCommitId: "merge-sha",
			Rewritten:      true,
			OldTip:         "old-tip-sha",
			Attempts:       1,
			ResolvedPaths:  [][]byte{[]byte("a.go"), []byte("docs/b.md")},
			Resolver:       "graft-resolve/union",
		}
		repo := dialFake(t, fakes.register).Repo("repo")

		opts := validLand()
		opts.ResolveConflicts = true
		result, err := repo.Land(t.Context(), opts)
		require.NoError(t, err)
		require.Equal(t, []string{"a.go", "docs/b.md"}, result.ResolvedPaths)
		require.Equal(t, "graft-resolve/union", result.Resolver)
		require.Equal(t, "merge-sha", result.LandedCommitID)

		require.Len(t, fakes.operations.landRequests, 1)
		require.True(t, fakes.operations.landRequests[0].GetResolveConflicts())
	})

	t.Run("is off by default and reports no resolution", func(t *testing.T) {
		t.Parallel()

		fakes := newFakeServices()
		fakes.operations.landResponse = &gitalypb.UserLandResponse{LandedCommitId: "landed-sha"}
		repo := dialFake(t, fakes.register).Repo("repo")

		result, err := repo.Land(t.Context(), validLand())
		require.NoError(t, err)
		require.Empty(t, result.ResolvedPaths)
		require.Empty(t, result.Resolver)

		require.Len(t, fakes.operations.landRequests, 1)
		require.False(t, fakes.operations.landRequests[0].GetResolveConflicts())
	})
}

func TestRepo_Land_Validation(t *testing.T) {
	t.Parallel()

	repo := dialFake(t, newFakeServices().register).Repo("repo")

	tests := []struct {
		name   string
		modify func(*sdk.LandOptions)
	}{
		{name: "missing branch", modify: func(o *sdk.LandOptions) { o.Branch = "" }},
		{name: "missing commit", modify: func(o *sdk.LandOptions) { o.Commit = "" }},
		{name: "negative attempts", modify: func(o *sdk.LandOptions) { o.MaxAttempts = -1 }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := validLand()
			tc.modify(&opts)

			_, err := repo.Land(t.Context(), opts)
			require.Equal(t, codes.InvalidArgument, sdk.Code(err))
		})
	}
}
