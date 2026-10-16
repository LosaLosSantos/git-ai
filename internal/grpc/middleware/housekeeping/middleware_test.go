package housekeeping

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	"github.com/feder-cr/git-ai/internal/git/housekeeping/config"
	housekeepingmgr "github.com/feder-cr/git-ai/internal/git/housekeeping/manager"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/git/stats"
	gitalycfg "github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// WaitForWorkers waits for any active housekeeping workers to finish. It is a
// test-only helper for synchronizing on scheduled housekeeping without
// cancelling it, unlike Stop which the production code uses during shutdown.
func (m *Middleware) WaitForWorkers() {
	m.wg.Wait()
}

type testService struct {
	gitalypb.UnimplementedRepositoryServiceServer
}

// Mutator, Unary
func (ts *testService) WriteRef(context.Context, *gitalypb.WriteRefRequest) (*gitalypb.WriteRefResponse, error) {
	return &gitalypb.WriteRefResponse{}, nil
}

func (ts *testService) RemoveRepository(context.Context, *gitalypb.RemoveRepositoryRequest) (*gitalypb.RemoveRepositoryResponse, error) {
	return &gitalypb.RemoveRepositoryResponse{}, nil
}

// Mutator, Unary, FailedPrecondition error
func (ts *testService) CreateRepository(context.Context, *gitalypb.CreateRepositoryRequest) (*gitalypb.CreateRepositoryResponse, error) {
	return nil, status.Error(codes.FailedPrecondition, "designed to error with FailedPrecondition")
}

// Mutator, Unary, ResourceExhausted error
func (ts *testService) FetchRemote(context.Context, *gitalypb.FetchRemoteRequest) (*gitalypb.FetchRemoteResponse, error) {
	return nil, status.Error(codes.ResourceExhausted, "designed to error with ResourceExhausted")
}

func (ts *testService) HasLocalBranches(context.Context, *gitalypb.HasLocalBranchesRequest) (*gitalypb.HasLocalBranchesResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "designed to error with PermissionDenied")
}

// Mutator, Unary, Erroring
func (ts *testService) CreateRepositoryFromBundle(grpc.ClientStreamingServer[gitalypb.CreateRepositoryFromBundleRequest, gitalypb.CreateRepositoryFromBundleResponse]) error {
	return fmt.Errorf("designed to error")
}

// Accessor, Unary
func (ts *testService) RepositoryExists(context.Context, *gitalypb.RepositoryExistsRequest) (*gitalypb.RepositoryExistsResponse, error) {
	return &gitalypb.RepositoryExistsResponse{}, nil
}

// Accessor, Stream
func (ts *testService) GetArchive(*gitalypb.GetArchiveRequest, grpc.ServerStreamingServer[gitalypb.GetArchiveResponse]) error {
	return nil
}

// Accessor, Unary, Erroring
func (ts *testService) RepositoryInfo(context.Context, *gitalypb.RepositoryInfoRequest) (*gitalypb.RepositoryInfoResponse, error) {
	return &gitalypb.RepositoryInfoResponse{}, fmt.Errorf("designed to error")
}

// Maintenance, Unary
func (ts *testService) OptimizeRepository(context.Context, *gitalypb.OptimizeRepositoryRequest) (*gitalypb.OptimizeRepositoryResponse, error) {
	return nil, nil
}

// Maintenance, Unary
func (ts *testService) PruneUnreachableObjects(context.Context, *gitalypb.PruneUnreachableObjectsRequest) (*gitalypb.PruneUnreachableObjectsResponse, error) {
	return nil, nil
}

type testCleanupService struct {
	gitalypb.UnimplementedCleanupServiceServer
}

// RewriteHistory is a stream RPC that forces housekeeping
func (ts *testCleanupService) RewriteHistory(stream grpc.ClientStreamingServer[gitalypb.RewriteHistoryRequest, gitalypb.RewriteHistoryResponse]) error {
	// Receive the first request to get the repository
	_, err := stream.Recv()
	if err != nil {
		return err
	}

	// Return success response
	return stream.SendAndClose(&gitalypb.RewriteHistoryResponse{})
}

type testBackupService struct {
	gitalypb.UnimplementedBackupServiceServer
}

// Storage-scoped, Unary
func (ts *testBackupService) WriteBackupID(context.Context, *gitalypb.WriteBackupIDRequest) (*gitalypb.WriteBackupIDResponse, error) {
	return &gitalypb.WriteBackupIDResponse{}, nil
}

type testInternalService struct {
	gitalypb.UnimplementedInternalGitalyServer
}

// Storage-scoped, Stream
func (ts *testInternalService) WalkRepos(*gitalypb.WalkReposRequest, grpc.ServerStreamingServer[gitalypb.WalkReposResponse]) error {
	return nil
}

type healthServer struct {
	healthpb.UnimplementedHealthServer
}

func (*healthServer) Check(context.Context, *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	return &healthpb.HealthCheckResponse{}, nil
}

type mockHousekeepingManager struct {
	// optimizeRepositoryInvocations allows us to track how many times OptimizeRepository was called for a given repository
	optimizeRepositoryInvocations map[string]int
	// lastEnabledOps allows us to get the last set of operations that were scheduled to run when OptimizeRepository was called
	lastEnabledOps map[string]map[config.OperationType]bool
	// collectedMVCCGarbage tracks whether MVCC garbage was collected for a given repository
	collectedMVCCGarbage map[string]bool
	// lastMVCCCache tracks the MVCC cache found on the context of the last OptimizeRepository call
	lastMVCCCache map[string]mvcc.Cache
	mu            sync.Mutex

	useDelayCh bool
	delayCh    chan struct{}
}

func (m *mockHousekeepingManager) getOptimizeRepositoryInvocations(relativePath string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.optimizeRepositoryInvocations[relativePath]
}

func (m *mockHousekeepingManager) getLastEnabledOps(relativePath string) map[config.OperationType]bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.lastEnabledOps[relativePath]
}

func (m *mockHousekeepingManager) getCollectedMVCCGarbage(relativePath string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.collectedMVCCGarbage[relativePath]
}

func (m *mockHousekeepingManager) getLastMVCCCache(relativePath string) mvcc.Cache {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.lastMVCCCache[relativePath]
}

func (m *mockHousekeepingManager) withDelay() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.delayCh = make(chan struct{})
	m.useDelayCh = true

	return m.delayCh
}

func (m *mockHousekeepingManager) withoutDelay() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.useDelayCh = false
}

func (m *mockHousekeepingManager) CleanStaleData(context.Context, *localrepo.Repo, housekeeping.CleanStaleDataConfig) error {
	return nil
}

func (m *mockHousekeepingManager) OptimizeRepository(ctx context.Context, repo *localrepo.Repo, opts ...housekeepingmgr.OptimizeRepositoryOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	relativePath := repo.GetRelativePath()
	if _, ok := m.optimizeRepositoryInvocations[relativePath]; !ok {
		m.optimizeRepositoryInvocations[relativePath] = 0
	}

	m.optimizeRepositoryInvocations[relativePath]++

	if m.lastMVCCCache != nil {
		m.lastMVCCCache[relativePath] = mvcc.CacheFromContext(ctx)
	}

	// Extract enabled operations from the strategy constructor
	var cfg housekeepingmgr.OptimizeRepositoryConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.StrategyConstructor != nil {
		strategy := cfg.StrategyConstructor(stats.RepositoryInfo{})
		if selective, ok := strategy.(*housekeeping.SelectiveOptimizationStrategy); ok {
			if m.lastEnabledOps == nil {
				m.lastEnabledOps = make(map[string]map[config.OperationType]bool)
			}
			m.lastEnabledOps[relativePath] = selective.EnabledOps()
		}
	}

	if m.useDelayCh {
		select {
		case <-m.delayCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

func (m *mockHousekeepingManager) RunMVCCGarbageCollection(_ context.Context, repo *localrepo.Repo, _ housekeeping.MVCCConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.collectedMVCCGarbage == nil {
		m.collectedMVCCGarbage = make(map[string]bool)
	}
	m.collectedMVCCGarbage[repo.GetRelativePath()] = true

	return nil
}

func (m *mockHousekeepingManager) OffloadRepository(context.Context, *localrepo.Repo, config.OffloadingConfig) error {
	return nil
}

func (m *mockHousekeepingManager) RehydrateRepository(ctx context.Context, repo *localrepo.Repo, s string) error {
	return nil
}

// testMiddlewareConfig returns a MiddlewareConfig for testing with all operations
// using the same interval for simpler test assertions.
func testMiddlewareConfig(interval int) MiddlewareConfig {
	thresholds := OperationThreshold{
		RPCInterval: interval,
	}
	return MiddlewareConfig{
		DefaultThresholds: thresholds,
		OperationThresholds: map[config.OperationType]OperationThreshold{
			config.OpRepackRefs:         thresholds,
			config.OpRepackObjects:      thresholds,
			config.OpPruneObjects:       thresholds,
			config.OpWriteCommitGraph:   thresholds,
			config.OpCollectMVCCGarbage: thresholds,
		},
	}
}

func TestInterceptors(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	logger := testhelper.NewLogger(t)
	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)

	localRepoFactory := localrepo.NewFactory(logger, gitalycfg.NewLocator(cfg), gittest.NewCommandFactory(t, cfg), catfileCache)

	housekeepingManager := &mockHousekeepingManager{
		optimizeRepositoryInvocations: make(map[string]int),
		delayCh:                       make(chan struct{}),
	}

	middlewareConfig := testMiddlewareConfig(1)

	housekeepingMiddleware := NewHousekeepingMiddleware(logger, protoregistry.GitalyProtoPreregistered, localRepoFactory, housekeepingManager, middlewareConfig)
	defer housekeepingMiddleware.WaitForWorkers()

	server := grpc.NewServer(
		grpc.StreamInterceptor(housekeepingMiddleware.StreamServerInterceptor()),
		grpc.UnaryInterceptor(housekeepingMiddleware.UnaryServerInterceptor()),
	)
	t.Cleanup(server.Stop)

	service := &testService{}
	cleanupService := &testCleanupService{}

	gitalypb.RegisterRepositoryServiceServer(server, service)
	gitalypb.RegisterCleanupServiceServer(server, cleanupService)
	gitalypb.RegisterBackupServiceServer(server, &testBackupService{})
	gitalypb.RegisterInternalGitalyServer(server, &testInternalService{})
	healthpb.RegisterHealthServer(server, &healthServer{})

	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", ":0")
	require.NoError(t, err)
	go func() {
		testhelper.MustServe(t, server, listener)
	}()

	conn, err := grpc.NewClient(
		listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer testhelper.MustClose(t, conn)

	t.Run("when the RemoveRepository RPC is invoked", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrepo1",
		}

		sendFn := func() {
			_, err = gitalypb.NewRepositoryServiceClient(conn).RemoveRepository(ctx, &gitalypb.RemoveRepositoryRequest{
				Repository: repo,
			})
			require.NoError(t, err)
		}

		for range 2 {
			sendFn()
		}

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "another invocation after the interval")
	})

	t.Run("when unary mutator RPCs are intercepted", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrepo1",
		}

		sendFn := func() {
			_, err = gitalypb.NewRepositoryServiceClient(conn).WriteRef(ctx, &gitalypb.WriteRefRequest{
				Repository: repo,
			})
			require.NoError(t, err)
		}

		sendFn()

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "no invocations under the interval")

		sendFn()

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 1, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "one invocation after the interval")
		require.True(t, housekeepingManager.getCollectedMVCCGarbage(repo.GetRelativePath()),
			"the middleware collects MVCC garbage")

		for range 2 {
			sendFn()
		}

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 2, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "another invocation after the interval")
	})

	t.Run("when unary accessor RPCs are intercepted", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrepo2",
		}

		sendFn := func() {
			_, err = gitalypb.NewRepositoryServiceClient(conn).RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{
				Repository: repo,
			})
			require.NoError(t, err)
		}

		sendFn()

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "no invocations under the interval")

		sendFn()

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "no invocations after the interval")
	})

	t.Run("when stream accessor RPCs are intercepted", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrepo3",
		}

		sendFn := func() {
			stream, err := gitalypb.NewRepositoryServiceClient(conn).GetArchive(ctx, &gitalypb.GetArchiveRequest{
				Repository: repo,
			})
			require.NoError(t, err)
			require.NoError(t, stream.CloseSend())
		}

		sendFn()

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "no invocations under the interval")

		sendFn()

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "no invocations after the interval")
	})

	t.Run("when an erroring RPC is intercepted", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrepo4",
		}

		for range 2 {
			_, err = gitalypb.NewRepositoryServiceClient(conn).RepositoryInfo(ctx, &gitalypb.RepositoryInfoRequest{
				Repository: repo,
			})
			require.EqualError(t, err, "rpc error: code = Unknown desc = designed to error", "middleware preserves the original error")
		}

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "no invocations after the interval")

		for range 2 {
			stream, err := gitalypb.NewRepositoryServiceClient(conn).CreateRepositoryFromBundle(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.CreateRepositoryFromBundleRequest{
				Repository: repo,
			}))

			_, err = stream.CloseAndRecv()
			require.EqualError(t, err, "rpc error: code = Unknown desc = designed to error", "middleware preserves the original error")
		}

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "no invocations after the interval")
	})

	t.Run("when a unary mutator returns a FailedPrecondition error", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrepo-failedprecondition",
		}

		for range 2 {
			_, err := gitalypb.NewRepositoryServiceClient(conn).CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{
				Repository: repo,
			})
			require.EqualError(t, err, "rpc error: code = FailedPrecondition desc = designed to error with FailedPrecondition",
				"middleware preserves the original error")
		}

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()),
			"FailedPrecondition errors should skip housekeeping")
	})

	t.Run("when a unary mutator returns a ResourceExhausted error", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrep-ResourceExhausted",
		}

		sendFn := func() {
			_, err := gitalypb.NewRepositoryServiceClient(conn).FetchRemote(ctx, &gitalypb.FetchRemoteRequest{
				Repository: repo,
			})
			require.EqualError(t, err, "rpc error: code = ResourceExhausted desc = designed to error with ResourceExhausted",
				"middleware preserves the original error")
		}

		sendFn()

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()),
			"no invocations under the interval")

		sendFn()

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()),
			"ResourceExhausted errors should skip housekeeping")
	})

	t.Run("when a unary mutator returns a PermissionDenied error", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrep-PermissionDenied",
		}

		sendFn := func() {
			_, err := gitalypb.NewRepositoryServiceClient(conn).HasLocalBranches(ctx, &gitalypb.HasLocalBranchesRequest{
				Repository: repo,
			})
			require.EqualError(t, err, "rpc error: code = PermissionDenied desc = designed to error with PermissionDenied",
				"middleware preserves the original error")
		}

		sendFn()

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()),
			"no invocations under the interval")

		sendFn()

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()),
			"PermissionDenied errors should skip housekeeping")
	})

	t.Run("when the OptimizeRepository RPC is invoked", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrepo5",
		}

		for range 2 {
			_, err = gitalypb.NewRepositoryServiceClient(conn).OptimizeRepository(ctx, &gitalypb.OptimizeRepositoryRequest{
				Repository: repo,
			})
			require.NoError(t, err)
		}

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "does not schedule further housekeeping")
	})

	t.Run("when the PruneUnreachableObjects RPC is invoked", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrepo6",
		}

		for range 2 {
			_, err = gitalypb.NewRepositoryServiceClient(conn).PruneUnreachableObjects(ctx, &gitalypb.PruneUnreachableObjectsRequest{
				Repository: repo,
			})
			require.NoError(t, err)
		}

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "does not schedule further housekeeping")
	})

	t.Run("when a housekeeping task is active when a maintenance RPC is received", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrepo7",
		}

		ch := housekeepingManager.withDelay()
		defer housekeepingManager.withoutDelay()

		for range 2 {
			_, err = gitalypb.NewRepositoryServiceClient(conn).WriteRef(ctx, &gitalypb.WriteRefRequest{
				Repository: repo,
			})
			require.NoError(t, err)
		}

		_, err = gitalypb.NewRepositoryServiceClient(conn).OptimizeRepository(ctx, &gitalypb.OptimizeRepositoryRequest{
			Repository: repo,
		})

		require.EqualError(t, err, "rpc error: code = AlreadyExists desc = housekeeping already executing for repository")

		close(ch)
	})

	t.Run("when a maintenance RPC is active and the write interval is reached", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrepo8",
		}

		ch := housekeepingManager.withDelay()
		defer housekeepingManager.withoutDelay()

		_, err = gitalypb.NewRepositoryServiceClient(conn).OptimizeRepository(ctx, &gitalypb.OptimizeRepositoryRequest{
			Repository: repo,
		})

		for range 2 {
			_, err = gitalypb.NewRepositoryServiceClient(conn).WriteRef(ctx, &gitalypb.WriteRefRequest{
				Repository: repo,
			})
			require.NoError(t, err)
		}

		close(ch)

		require.Equal(t, 1, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "no invocations under the interval")
	})

	t.Run("when the write interval is reached again when housekeeping is active", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrepo9",
		}

		sendFn := func() {
			_, err = gitalypb.NewRepositoryServiceClient(conn).WriteRef(ctx, &gitalypb.WriteRefRequest{
				Repository: repo,
			})
			require.NoError(t, err)
		}

		ch := housekeepingManager.withDelay()
		defer housekeepingManager.withoutDelay()

		// The first two requests will trigger housekeeping that runs until ch is closed.
		// The next two requests won't trigger housekeeping as there's already an active job.
		for range 4 {
			sendFn()
		}

		// Release the active housekeeping job.
		close(ch)

		// The next request triggers housekeeping as the counter has already incremented past the interval.
		sendFn()

		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 2, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()), "another invocation after the interval")
	})

	t.Run("when an RPC not registered with protoregistry.GitalyProtoPreregistered is intercepted", func(t *testing.T) {
		hook := testhelper.AddLoggerHook(logger)

		_, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
		require.NoError(t, err)

		require.Empty(t, hook.LastEntry(), "it does not log an error")
	})

	t.Run("when RPCs without a target repository are intercepted", func(t *testing.T) {
		hook := testhelper.AddLoggerHook(logger)

		_, err := gitalypb.NewBackupServiceClient(conn).WriteBackupID(ctx, &gitalypb.WriteBackupIDRequest{
			StorageName: "default",
			BackupId:    "backup-id",
		})
		require.NoError(t, err)

		stream, err := gitalypb.NewInternalGitalyClient(conn).WalkRepos(ctx, &gitalypb.WalkReposRequest{StorageName: "default"})
		require.NoError(t, err)
		_, err = stream.Recv()
		require.Equal(t, io.EOF, err)

		require.Empty(t, hook.LastEntry(), "it does not log an error")
	})

	t.Run("when housekeeping is forced", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "myrepo-force",
		}

		// Test RewriteHistory RPC which should force housekeeping immediately
		stream, err := gitalypb.NewCleanupServiceClient(conn).RewriteHistory(ctx)
		require.NoError(t, err)

		err = stream.Send(&gitalypb.RewriteHistoryRequest{
			Repository: repo,
		})
		require.NoError(t, err)

		_, err = stream.CloseAndRecv()
		require.NoError(t, err)

		// Wait for any async housekeeping to complete
		housekeepingMiddleware.WaitForWorkers()

		// Verify that housekeeping was triggered immediately (forced) even on the first call
		require.Equal(t, 1,
			housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()),
			"RewriteHistory should force immediate housekeeping")
	})

	t.Run("when forceHousekeepingRPCs bypass interval compared to regular mutators", func(t *testing.T) {
		forceRepo := &gitalypb.Repository{
			RelativePath: "myrepo-force-bypass",
		}
		regularRepo := &gitalypb.Repository{
			RelativePath: "myrepo-regular-interval",
		}

		// Test that forceHousekeepingRPCs bypass the normal interval constraint
		// First RewriteHistory call should immediately trigger housekeeping (force=true)
		stream, err := gitalypb.NewCleanupServiceClient(conn).RewriteHistory(ctx)
		require.NoError(t, err)

		err = stream.Send(&gitalypb.RewriteHistoryRequest{
			Repository: forceRepo,
			Redactions: [][]byte{[]byte("test-pattern")},
		})
		require.NoError(t, err)

		_, err = stream.CloseAndRecv()
		require.NoError(t, err)

		housekeepingMiddleware.WaitForWorkers()

		// Should trigger housekeeping immediately despite being the first call
		require.Equal(t, 1,
			housekeepingManager.getOptimizeRepositoryInvocations(forceRepo.GetRelativePath()),
			"First RewriteHistory call should force housekeeping immediately")

		// Compare with regular mutator RPCs that respect the interval
		// Single regular mutator call should not trigger housekeeping
		_, err = gitalypb.NewRepositoryServiceClient(conn).WriteRef(ctx, &gitalypb.WriteRefRequest{
			Repository: regularRepo,
		})
		require.NoError(t, err)

		housekeepingMiddleware.WaitForWorkers()

		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(regularRepo.GetRelativePath()),
			"Single regular mutator should not trigger housekeeping (respects interval)")

		// The second regular mutator call should trigger housekeeping (interval=1)
		_, err = gitalypb.NewRepositoryServiceClient(conn).WriteRef(ctx, &gitalypb.WriteRefRequest{
			Repository: regularRepo,
		})
		require.NoError(t, err)

		housekeepingMiddleware.WaitForWorkers()

		require.Equal(t, 1,
			housekeepingManager.getOptimizeRepositoryInvocations(regularRepo.GetRelativePath()),
			"Second regular mutator should trigger housekeeping after reaching interval")
	})
}

func TestIndependentOperationThresholds(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	logger := testhelper.NewLogger(t)
	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)

	localRepoFactory := localrepo.NewFactory(logger, gitalycfg.NewLocator(cfg), gittest.NewCommandFactory(t, cfg), catfileCache)

	housekeepingManager := &mockHousekeepingManager{
		optimizeRepositoryInvocations: make(map[string]int),
		delayCh:                       make(chan struct{}),
	}

	// Configure different thresholds for different operations
	middlewareConfig := MiddlewareConfig{
		DefaultThresholds: OperationThreshold{
			RPCInterval: 20,
		},
		OperationThresholds: map[config.OperationType]OperationThreshold{
			config.OpRepackRefs: {
				RPCInterval: 2,
			},
			config.OpRepackObjects: {
				RPCInterval: 5,
			},
		},
	}

	housekeepingMiddleware := NewHousekeepingMiddleware(logger, protoregistry.GitalyProtoPreregistered, localRepoFactory, housekeepingManager, middlewareConfig)
	defer housekeepingMiddleware.WaitForWorkers()

	server := grpc.NewServer(
		grpc.StreamInterceptor(housekeepingMiddleware.StreamServerInterceptor()),
		grpc.UnaryInterceptor(housekeepingMiddleware.UnaryServerInterceptor()),
	)
	t.Cleanup(server.Stop)

	service := &testService{}
	gitalypb.RegisterRepositoryServiceServer(server, service)

	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", ":0")
	require.NoError(t, err)
	go func() {
		testhelper.MustServe(t, server, listener)
	}()

	conn, err := grpc.NewClient(
		listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer testhelper.MustClose(t, conn)

	t.Run("pack-refs triggers before repack due to lower threshold", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "independent-thresholds-repo",
		}

		sendFn := func() {
			_, err = gitalypb.NewRepositoryServiceClient(conn).WriteRef(ctx, &gitalypb.WriteRefRequest{
				Repository: repo,
			})
			require.NoError(t, err)
		}

		sendFn()
		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0, housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()),
			"no housekeeping after 1 RPC")

		sendFn()
		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 0,
			housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()),
			"no housekeeping AT threshold")

		sendFn()
		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 1,
			housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()),
			"housekeeping triggered after pack-refs passing threshold")

		sendFn()
		sendFn()
		sendFn()
		housekeepingMiddleware.WaitForWorkers()
		require.Equal(t, 2,
			housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()),
			"housekeeping triggered again after pack-refs threshold")
	})

	t.Run("repack-refs and repack-objects run at independent intervals", func(t *testing.T) {
		repo := &gitalypb.Repository{
			RelativePath: "independent-intervals-repo",
		}

		sendFn := func() {
			_, err = gitalypb.NewRepositoryServiceClient(conn).WriteRef(ctx, &gitalypb.WriteRefRequest{
				Repository: repo,
			})
			require.NoError(t, err)
		}

		// With RPCInterval: 2 for pack-refs and RPCInterval: 5 for repack-objects:
		// pack-refs triggers when writeCount > 2 (after 3 RPCs)
		// repack-objects triggers when writeCount > 5 (after 6 RPCs)

		// Send 3 RPCs - pack-refs threshold exceeded (3 > 2), repack-objects not (3 < 5)
		for i := 0; i < 3; i++ {
			sendFn()
		}
		housekeepingMiddleware.WaitForWorkers()

		require.Equal(t,
			1,
			housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()),
			"housekeeping should trigger after pack-refs threshold (3 RPCs)")

		// Verify pack-refs was requested but NOT repack-objects
		ops := housekeepingManager.getLastEnabledOps(repo.GetRelativePath())
		require.True(t, ops[config.OpRepackRefs],
			"pack-refs should be enabled after 3 RPCs")
		require.False(t, ops[config.OpRepackObjects],
			"repack-objects should NOT be enabled after only 3 RPCs")

		// Send 3 more RPCs
		// pack-refs: 3 > 2, triggers again
		// repack-objects: 6 > 5, triggers now
		for i := 0; i < 3; i++ {
			sendFn()
		}
		housekeepingMiddleware.WaitForWorkers()

		require.Equal(t,
			2,
			housekeepingManager.getOptimizeRepositoryInvocations(repo.GetRelativePath()),
			"housekeeping should trigger again after 6 total RPCs")

		ops = housekeepingManager.getLastEnabledOps(repo.GetRelativePath())
		require.True(t, ops[config.OpRepackRefs],
			"pack-refs should be enabled")
		require.True(t, ops[config.OpRepackObjects],
			"repack-objects should now be enabled after 6 RPCs")
	})
}

// TestMiddleware_StopCancelsInFlightWorkers verifies that Stop cancels detached
// housekeeping workers instead of waiting for them to finish naturally. The
// workers are disowned from the request context, so prior to having a
// cancellation path they would keep optimizing a repository until completion and
// block process shutdown for minutes after the grace period expired.
func TestMiddleware_StopCancelsInFlightWorkers(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	logger := testhelper.NewLogger(t)
	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)

	localRepoFactory := localrepo.NewFactory(logger, gitalycfg.NewLocator(cfg), gittest.NewCommandFactory(t, cfg), catfileCache)

	housekeepingManager := &mockHousekeepingManager{
		optimizeRepositoryInvocations: make(map[string]int),
		delayCh:                       make(chan struct{}),
	}

	m := NewHousekeepingMiddleware(logger, protoregistry.GitalyProtoPreregistered, localRepoFactory, housekeepingManager, testMiddlewareConfig(1))

	// Block the housekeeping worker as if it were optimizing a large repository. The
	// delay channel is intentionally never closed, so only context cancellation
	// originating from Stop is able to unblock the worker.
	housekeepingManager.withDelay()

	// Two mutator RPCs cross the RPCInterval threshold and schedule a single worker
	// that then blocks inside OptimizeRepository.
	repo := &gitalypb.Repository{RelativePath: "stop-repo"}
	m.scheduleHousekeeping(ctx, repo, false)
	m.scheduleHousekeeping(ctx, repo, false)

	done := make(chan struct{})
	go func() {
		m.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return: in-flight housekeeping worker was not cancelled")
	}
}

// TestMiddleware_detachedHousekeepingDoesNotInheritMVCCCache verifies that a
// scheduled housekeeping worker never runs with the MVCC cache of the RPC that
// triggered it. Today the mvcc middleware is registered inside this one, so its
// cache never reaches this code — but that is an ordering detail that could change
// silently, and the worker outlives the RPC: it must commit against whichever
// manifest is current when it runs, not the one the RPC pinned.
func TestMiddleware_detachedHousekeepingDoesNotInheritMVCCCache(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	logger := testhelper.NewLogger(t)
	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)

	localRepoFactory := localrepo.NewFactory(logger, gitalycfg.NewLocator(cfg), gittest.NewCommandFactory(t, cfg), catfileCache)

	housekeepingManager := &mockHousekeepingManager{
		optimizeRepositoryInvocations: make(map[string]int),
		lastMVCCCache:                 make(map[string]mvcc.Cache),
	}

	m := NewHousekeepingMiddleware(logger, protoregistry.GitalyProtoPreregistered, localRepoFactory, housekeepingManager, testMiddlewareConfig(1))

	cache := mvcc.NewLocalCache(mvcc.NewLocalStorage(cfg.Storages[0].Path), cfg.RuntimeDir, cfg.Storages[0].Path, "mvcc-repo", false)
	require.NotNil(t, mvcc.CacheFromContext(mvcc.ContextWithCache(ctx, cache)),
		"the request context must carry a cache for this test to be meaningful")

	repo := &gitalypb.Repository{RelativePath: "mvcc-repo"}
	m.scheduleHousekeeping(mvcc.ContextWithCache(ctx, cache), repo, true)
	m.Stop()

	require.Equal(t, 1, housekeepingManager.getOptimizeRepositoryInvocations("mvcc-repo"))
	require.Nil(t, housekeepingManager.getLastMVCCCache("mvcc-repo"),
		"detached housekeeping must not inherit the RPC's MVCC cache")
}
