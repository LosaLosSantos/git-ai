package sdk_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// fakeStorageCore implements StorageCoreService with canned responses and
// records the requests it receives.
type fakeStorageCore struct {
	gitalypb.UnimplementedStorageCoreServiceServer

	importResp     *gitalypb.ImportRepositoryResponse
	stateResp      *gitalypb.GetStorageStateResponse
	verifyResp     *gitalypb.VerifyRepositoryResponse
	checkpointResp *gitalypb.CreateCheckpointResponse
	gcResp         *gitalypb.CollectGarbageResponse
	removedResp    *gitalypb.CollectRemovedRepositoriesResponse
	err            error

	mu             sync.Mutex
	importReqs     []*gitalypb.ImportRepositoryRequest
	stateReqs      []*gitalypb.GetStorageStateRequest
	verifyReqs     []*gitalypb.VerifyRepositoryRequest
	checkpointReqs []*gitalypb.CreateCheckpointRequest
	gcReqs         []*gitalypb.CollectGarbageRequest
	removedReqs    []*gitalypb.CollectRemovedRepositoriesRequest
}

func (f *fakeStorageCore) CreateCheckpoint(_ context.Context, req *gitalypb.CreateCheckpointRequest) (*gitalypb.CreateCheckpointResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkpointReqs = append(f.checkpointReqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.checkpointResp, nil
}

func (f *fakeStorageCore) CollectGarbage(_ context.Context, req *gitalypb.CollectGarbageRequest) (*gitalypb.CollectGarbageResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gcReqs = append(f.gcReqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.gcResp, nil
}

func (f *fakeStorageCore) CollectRemovedRepositories(_ context.Context, req *gitalypb.CollectRemovedRepositoriesRequest) (*gitalypb.CollectRemovedRepositoriesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedReqs = append(f.removedReqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.removedResp, nil
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

func TestStorageCheckpoint(t *testing.T) {
	fake := &fakeStorageCore{checkpointResp: &gitalypb.CreateCheckpointResponse{
		CheckpointSequence: 42,
		Created:            true,
		PackBytes:          8192,
		References:         5,
	}}
	repo := storageRepo(t, fake)

	res, err := repo.StorageCheckpoint(t.Context())
	require.NoError(t, err)
	require.Equal(t, &sdk.StorageCheckpoint{Sequence: 42, Created: true, PackBytes: 8192, References: 5}, res)
	require.Len(t, fake.checkpointReqs, 1)
	require.Equal(t, "@hashed/ab/cd/repo.git", fake.checkpointReqs[0].GetRepository().GetRelativePath())
}

func TestStorageGC(t *testing.T) {
	fake := &fakeStorageCore{gcResp: &gitalypb.CollectGarbageResponse{
		Boundary:           7,
		CoveredDeleted:     3,
		CheckpointsDropped: 1,
		OrphansDeleted:     2,
		OrphansObserved:    4,
		OrphansInGrace:     5,
		PacksDeleted:       6,
		BytesReclaimed:     1 << 20,
		DryRun:             true,
	}}
	repo := storageRepo(t, fake)

	res, err := repo.StorageGC(t.Context(), true)
	require.NoError(t, err)
	require.Equal(t, &sdk.StorageGC{
		Boundary:           7,
		CoveredDeleted:     3,
		CheckpointsDropped: 1,
		OrphansDeleted:     2,
		OrphansObserved:    4,
		OrphansInGrace:     5,
		PacksDeleted:       6,
		BytesReclaimed:     1 << 20,
		DryRun:             true,
	}, res)
	require.Len(t, fake.gcReqs, 1)
	require.True(t, fake.gcReqs[0].GetDryRun())
	require.Equal(t, "default", fake.gcReqs[0].GetRepository().GetStorageName())
}

func TestStorageGCRemoved(t *testing.T) {
	fake := &fakeStorageCore{removedResp: &gitalypb.CollectRemovedRepositoriesResponse{
		Repositories:   2,
		PacksDeleted:   9,
		BytesReclaimed: 4096,
	}}
	client := dialFake(t, func(srv *grpc.Server) {
		gitalypb.RegisterStorageCoreServiceServer(srv, fake)
	}, sdk.WithStorage("default"))

	res, err := client.StorageGCRemoved(t.Context(), "", false)
	require.NoError(t, err)
	require.Equal(t, &sdk.StorageGCRemoved{Repositories: 2, PacksDeleted: 9, BytesReclaimed: 4096}, res)

	_, err = client.StorageGCRemoved(t.Context(), "other", true)
	require.NoError(t, err)

	require.Len(t, fake.removedReqs, 2)
	require.Equal(t, "default", fake.removedReqs[0].GetStorageName(), "the client's storage by default")
	require.False(t, fake.removedReqs[0].GetDryRun())
	require.Equal(t, "other", fake.removedReqs[1].GetStorageName())
	require.True(t, fake.removedReqs[1].GetDryRun())
}

func TestStorageMaintenanceErrorsAreReturned(t *testing.T) {
	fake := &fakeStorageCore{err: status.Error(codes.FailedPrecondition, "the storage core is off")}
	repo := storageRepo(t, fake)

	_, err := repo.StorageCheckpoint(t.Context())
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = repo.StorageGC(t.Context(), false)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
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
