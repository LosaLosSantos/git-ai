package verify

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// fakeOperations answers UserRevert with a fixed response or error and
// records the requests. Other methods are not implemented.
type fakeOperations struct {
	gitalypb.OperationServiceClient

	resp  *gitalypb.UserRevertResponse
	err   error
	calls int
	got   *gitalypb.UserRevertRequest
}

func (f *fakeOperations) UserRevert(_ context.Context, in *gitalypb.UserRevertRequest, _ ...grpc.CallOption) (*gitalypb.UserRevertResponse, error) {
	f.calls++
	f.got = in
	return f.resp, f.err
}

// detailedRevertError returns a FailedPrecondition status error that carries
// a UserRevertError detail, as the server answers it.
func detailedRevertError(t *testing.T, msg string, detail *gitalypb.UserRevertError) error {
	t.Helper()
	st, err := status.New(codes.FailedPrecondition, msg).WithDetails(detail)
	require.NoError(t, err)
	return st.Err()
}

// tipsBackend answers ResolveRevision with a fixed tip for refs/heads/main.
type tipsBackend struct {
	workspace.Backend

	tip string
	err error
	got []string
}

func (b *tipsBackend) ResolveRevision(_ context.Context, rev string) (string, error) {
	b.got = append(b.got, rev)
	return b.tip, b.err
}

func TestOperationReverter_Revert(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "x.git"}
	user := &gitalypb.User{Name: []byte("graft-verify"), Email: []byte("v@example.com")}
	req := RevertRequest{Branch: "main", Commit: "c1", ExpectedOld: "tip", Message: "Revert \"x\"\n"}

	newReverter := func(ops gitalypb.OperationServiceClient) *OperationReverter {
		return &OperationReverter{Operations: ops, Repository: repo, User: user}
	}

	t.Run("sends the request and returns the new commit", func(t *testing.T) {
		ops := &fakeOperations{resp: &gitalypb.UserRevertResponse{
			BranchUpdate: &gitalypb.OperationBranchUpdate{CommitId: "r1"},
		}}

		id, err := newReverter(ops).Revert(ctx, req)
		require.NoError(t, err)
		require.Equal(t, "r1", id)

		require.Equal(t, repo, ops.got.GetRepository())
		require.Equal(t, user, ops.got.GetUser())
		require.Equal(t, "c1", ops.got.GetCommit().GetId())
		require.Equal(t, "main", string(ops.got.GetBranchName()))
		require.Equal(t, "Revert \"x\"\n", string(ops.got.GetMessage()))
		require.Equal(t, "tip", ops.got.GetExpectedOldOid())
	})

	t.Run("committer is the user when no user is configured", func(t *testing.T) {
		ops := &fakeOperations{resp: &gitalypb.UserRevertResponse{
			BranchUpdate: &gitalypb.OperationBranchUpdate{CommitId: "r1"},
		}}
		r := &OperationReverter{Operations: ops, Repository: repo}
		withCommitter := req
		withCommitter.Committer = Signature{Name: "graft-verify", Email: "v@example.com"}

		_, err := r.Revert(ctx, withCommitter)
		require.NoError(t, err)
		require.Equal(t, "graft-verify", string(ops.got.GetUser().GetName()))
		require.Equal(t, "v@example.com", string(ops.got.GetUser().GetEmail()))
	})

	t.Run("no user and no committer is refused", func(t *testing.T) {
		ops := &fakeOperations{}
		_, err := (&OperationReverter{Operations: ops, Repository: repo}).Revert(ctx, req)
		require.ErrorContains(t, err, "committer")
		require.Equal(t, 0, ops.calls)
	})

	t.Run("missing expected old tip is refused without calling the server", func(t *testing.T) {
		ops := &fakeOperations{}
		noOld := req
		noOld.ExpectedOld = ""
		_, err := newReverter(ops).Revert(ctx, noOld)
		require.ErrorContains(t, err, "expected old tip")
		require.Equal(t, 0, ops.calls, "an empty expected old would overwrite the branch")
	})

	t.Run("missing branch or commit is refused", func(t *testing.T) {
		ops := &fakeOperations{}
		noBranch := req
		noBranch.Branch = ""
		_, err := newReverter(ops).Revert(ctx, noBranch)
		require.Error(t, err)

		noCommit := req
		noCommit.Commit = ""
		_, err = newReverter(ops).Revert(ctx, noCommit)
		require.Error(t, err)
		require.Equal(t, 0, ops.calls)
	})

	t.Run("unconfigured reverter is refused", func(t *testing.T) {
		_, err := (&OperationReverter{}).Revert(ctx, req)
		require.Error(t, err)
	})

	structured := []struct {
		name    string
		detail  *gitalypb.UserRevertError
		wantErr error
		wantMsg string
	}{
		{
			name: "merge conflict",
			detail: &gitalypb.UserRevertError{Error: &gitalypb.UserRevertError_MergeConflict{
				MergeConflict: &gitalypb.MergeConflictError{ConflictingFiles: [][]byte{[]byte("a.go")}},
			}},
			wantErr: ErrRevertConflict,
			wantMsg: "conflicting files",
		},
		{
			name: "changes already applied",
			detail: &gitalypb.UserRevertError{Error: &gitalypb.UserRevertError_ChangesAlreadyApplied{
				ChangesAlreadyApplied: &gitalypb.ChangesAlreadyAppliedError{},
			}},
			wantErr: ErrAlreadyReverted,
		},
		{
			name: "branch diverged",
			detail: &gitalypb.UserRevertError{Error: &gitalypb.UserRevertError_NotAncestor{
				NotAncestor: &gitalypb.NotAncestorError{},
			}},
			wantErr: ErrBranchMoved,
			wantMsg: "branch diverged",
		},
		{
			name: "pre-receive hook",
			detail: &gitalypb.UserRevertError{Error: &gitalypb.UserRevertError_CustomHook{
				CustomHook: &gitalypb.CustomHookError{},
			}},
			wantMsg: "pre-receive hook rejected",
		},
	}
	for _, tc := range structured {
		t.Run("structured error: "+tc.name, func(t *testing.T) {
			msg := "revert: failed"
			if tc.wantMsg != "" {
				msg = tc.wantMsg
			}
			ops := &fakeOperations{err: detailedRevertError(t, msg, tc.detail)}
			_, err := newReverter(ops).Revert(ctx, req)
			require.Error(t, err)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NotErrorIs(t, err, ErrRevertConflict)
				require.NotErrorIs(t, err, ErrAlreadyReverted)
			}
			if tc.wantMsg != "" {
				require.ErrorContains(t, err, tc.wantMsg)
			}
		})
	}

	responses := []struct {
		name    string
		resp    *gitalypb.UserRevertResponse
		err     error
		wantErr error
		wantMsg string
	}{
		{
			name:    "empty revert means already reverted",
			resp:    &gitalypb.UserRevertResponse{CreateTreeErrorCode: gitalypb.UserRevertResponse_EMPTY},
			wantErr: ErrAlreadyReverted,
		},
		{
			name:    "conflict in the response",
			resp:    &gitalypb.UserRevertResponse{CreateTreeErrorCode: gitalypb.UserRevertResponse_CONFLICT, CreateTreeError: "conflict in a.go"},
			wantErr: ErrRevertConflict,
			wantMsg: "conflict in a.go",
		},
		{
			name:    "commit error in the response means the branch moved",
			resp:    &gitalypb.UserRevertResponse{CommitError: "reference update conflict"},
			wantErr: ErrBranchMoved,
		},
		{
			name:    "tree error without a code",
			resp:    &gitalypb.UserRevertResponse{CreateTreeError: "boom"},
			wantMsg: "boom",
		},
		{
			name:    "pre-receive rejection in the response",
			resp:    &gitalypb.UserRevertResponse{PreReceiveError: "policy says no"},
			wantMsg: "policy says no",
		},
		{
			name:    "no commit in the answer",
			resp:    &gitalypb.UserRevertResponse{},
			wantMsg: "no commit",
		},
		{
			name:    "rpc failure",
			err:     errors.New("unavailable"),
			wantMsg: "unavailable",
		},
	}
	for _, tc := range responses {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newReverter(&fakeOperations{resp: tc.resp, err: tc.err}).Revert(ctx, req)
			require.Error(t, err)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			}
			if tc.wantMsg != "" {
				require.ErrorContains(t, err, tc.wantMsg)
			}
		})
	}

	t.Run("unstructured failure on a moved branch is a moved branch", func(t *testing.T) {
		ops := &fakeOperations{err: errors.New("update reference: old value mismatch")}
		r := newReverter(ops)
		r.TipOf = func(context.Context, string) (string, error) { return "newer", nil }

		_, err := r.Revert(ctx, req)
		require.ErrorIs(t, err, ErrBranchMoved)
	})

	t.Run("unstructured failure on the expected tip is not a moved branch", func(t *testing.T) {
		ops := &fakeOperations{err: errors.New("update reference: disk full")}
		r := newReverter(ops)
		r.TipOf = func(context.Context, string) (string, error) { return "tip", nil }

		_, err := r.Revert(ctx, req)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrBranchMoved)
	})

	t.Run("unstructured failure with unreadable tip is not a moved branch", func(t *testing.T) {
		ops := &fakeOperations{err: errors.New("boom")}
		r := newReverter(ops)
		r.TipOf = func(context.Context, string) (string, error) { return "", errors.New("unreadable") }

		_, err := r.Revert(ctx, req)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrBranchMoved)
	})
}

func TestOperationReverter_Tip(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	r := &OperationReverter{TipOf: func(_ context.Context, branch string) (string, error) {
		require.Equal(t, "main", branch)
		return "tip", nil
	}}
	tip, err := r.Tip(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, "tip", tip)

	_, err = (&OperationReverter{}).Tip(ctx, "main")
	require.Error(t, err)
}

func TestBranchTip(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	backend := &tipsBackend{tip: "abc"}
	tip, err := BranchTip(backend)(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, "abc", tip)
	require.Equal(t, []string{"refs/heads/main"}, backend.got)

	backend.err = errors.New("missing")
	_, err = BranchTip(backend)(ctx, "gone")
	require.ErrorContains(t, err, "missing")
}
