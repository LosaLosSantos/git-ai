package sdk_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
)

// fakeStorageCore implements StorageCoreService with canned responses and
// records the requests it receives.
type fakeStorageCore struct {
	gitalypb.UnimplementedStorageCoreServiceServer

	importResp *gitalypb.ImportRepositoryResponse
	stateResp  *gitalypb.GetStorageStateResponse
	verifyResp *gitalypb.VerifyRepositoryResponse
	err        error

	mu         sync.Mutex
	importReqs []*gitalypb.ImportRepositoryRequest
	stateReqs  []*gitalypb.GetStorageStateRequest
	verifyReqs []*gitalypb.VerifyRepositoryRequest
}

func (f *fakeStorageCore) ImportRepository(_ context.Context, req *gitalypb.ImportRepositoryRequest) (*gitalypb.ImportRepositoryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.importReqs = append(f.importReqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.importResp, nil
}

func (f *fakeStorageCore) GetStorageState(_ context.Context, req *gitalypb.GetStorageStateRequest) (*gitalypb.GetStorageStateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateReqs = append(f.stateReqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.stateResp, nil
}

func (f *fakeStorageCore) VerifyRepository(_ context.Context, req *gitalypb.VerifyRepositoryRequest) (*gitalypb.VerifyRepositoryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifyReqs = append(f.verifyReqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.verifyResp, nil
}

// storageRepo starts the fake and returns a handle to a repository on it.
func storageRepo(t *testing.T, fake *fakeStorageCore) *sdk.Repo {
	t.Helper()

	client := dialFake(t, func(srv *grpc.Server) {
		gitalypb.RegisterStorageCoreServiceServer(srv, fake)
	}, sdk.WithStorage("default"))
	return client.Repo("@hashed/ab/cd/repo.git")
}

func TestStorageImport(t *testing.T) {
	fake := &fakeStorageCore{importResp: &gitalypb.ImportRepositoryResponse{
		Sequence:         1,
		References:       3,
		PackBytes:        4096,
		ChangelogEntries: 7,
		Reviews:          2,
	}}
	repo := storageRepo(t, fake)

	res, err := repo.StorageImport(t.Context())
	require.NoError(t, err)
	require.Equal(t, &sdk.StorageImport{
		Sequence:         1,
		References:       3,
		PackBytes:        4096,
		ChangelogEntries: 7,
		Reviews:          2,
	}, res)

	require.Len(t, fake.importReqs, 1)
	got := fake.importReqs[0].GetRepository()
	require.Equal(t, "default", got.GetStorageName())
	require.Equal(t, "@hashed/ab/cd/repo.git", got.GetRelativePath())
}

func TestStorageImportErrorIsReturned(t *testing.T) {
	fake := &fakeStorageCore{err: status.Error(codes.AlreadyExists, "already tracked")}
	repo := storageRepo(t, fake)

	_, err := repo.StorageImport(t.Context())
	require.Equal(t, codes.AlreadyExists, status.Code(err))
}

func TestStorageState(t *testing.T) {
	fake := &fakeStorageCore{stateResp: &gitalypb.GetStorageStateResponse{
		Mode:            "authoritative",
		Tracked:         true,
		RepositoryId:    12,
		HeadSequence:    40,
		AppliedSequence: 39,
		NodeId:          "node-b",
	}}
	repo := storageRepo(t, fake)

	state, err := repo.StorageState(t.Context())
	require.NoError(t, err)
	require.Equal(t, &sdk.StorageState{
		Mode:            "authoritative",
		Tracked:         true,
		RepositoryID:    12,
		HeadSequence:    40,
		AppliedSequence: 39,
		NodeID:          "node-b",
	}, state)
	require.Len(t, fake.stateReqs, 1)
}

func TestStorageVerify(t *testing.T) {
	fake := &fakeStorageCore{verifyResp: &gitalypb.VerifyRepositoryResponse{
		HeadSequence: 5,
		Divergences: []*gitalypb.VerifyRepositoryResponse_ReferenceDivergence{{
			Reference:      []byte("refs/heads/main"),
			LocalTarget:    "1111111111111111111111111111111111111111",
			DatabaseTarget: "2222222222222222222222222222222222222222",
		}},
		ConnectivityOk:    false,
		ConnectivityError: "broken link",
	}}
	repo := storageRepo(t, fake)

	res, err := repo.StorageVerify(t.Context(), true)
	require.NoError(t, err)
	require.EqualValues(t, 5, res.HeadSequence)
	require.Equal(t, []sdk.StorageDivergence{{
		Reference:      "refs/heads/main",
		LocalTarget:    "1111111111111111111111111111111111111111",
		DatabaseTarget: "2222222222222222222222222222222222222222",
	}}, res.Divergences)
	require.False(t, res.ConnectivityOK)
	require.Equal(t, "broken link", res.ConnectivityError)

	require.Len(t, fake.verifyReqs, 1)
	require.True(t, fake.verifyReqs[0].GetCheckConnectivity())
}

func TestStorageVerifyWithoutConnectivityOrDivergence(t *testing.T) {
	fake := &fakeStorageCore{verifyResp: &gitalypb.VerifyRepositoryResponse{HeadSequence: 1}}
	repo := storageRepo(t, fake)

	res, err := repo.StorageVerify(t.Context(), false)
	require.NoError(t, err)
	require.Empty(t, res.Divergences)
	require.False(t, fake.verifyReqs[0].GetCheckConnectivity())
}
