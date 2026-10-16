package operations

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/landlane"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

// testAgentHeader is the metadata that names the agent a test request is made as.
const testAgentHeader = "x-test-agent"

// agentInterceptors authenticates each request as the agent named in its test header, the way the
// per-agent token authentication does, so that every request carries its own identity.
func agentInterceptors(log.Logger, localrepo.Factory) ([]grpc.UnaryServerInterceptor, []grpc.StreamServerInterceptor) {
	return []grpc.UnaryServerInterceptor{
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			if md, ok := metadata.FromIncomingContext(ctx); ok {
				if names := md.Get(testAgentHeader); len(names) > 0 {
					ctx = identity.NewContext(ctx, identity.Identity{Name: names[0], Source: identity.SourceAgent})
				}
			}

			return handler(ctx, req)
		},
	}, nil
}

// setupAgentLandService starts an operations service that authenticates requests as the agents they
// name, and that checks the standalone policy of the configuration.
func setupAgentLandService(t *testing.T, ctx context.Context, graft config.Graft) (context.Context, config.Cfg, gitalypb.OperationServiceClient) {
	t.Helper()

	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{Graft: graft}))
	return setupOperationsServiceWithCfg(t, ctx, cfg,
		testserver.WithInterceptors(agentInterceptors),
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(graft, testhelper.NewLogger(t))),
	)
}

// agentBatchingGraft is a standalone configuration with the given policy, in which the lanes collect
// requests for 300ms. That is long enough for landInOrder to put its requests in one batch.
func agentBatchingGraft(policy []config.GraftPolicyRule) config.Graft {
	return config.Graft{
		Standalone: true,
		Policy:     policy,
		Land:       config.GraftLand{Batch: config.GraftLandBatch{Window: "300ms"}},
	}
}

// denyMainFor is a policy that denies the actor updates of main and allows everything else.
func denyMainFor(actor string) []config.GraftPolicyRule {
	return []config.GraftPolicyRule{
		{Actor: actor, Refs: []string{"refs/heads/main"}, Allow: false},
		{Actor: "*", Allow: true},
	}
}

// userWithGLID returns a user that makes requests without an agent identity.
func userWithGLID(glID string) *gitalypb.User {
	return &gitalypb.User{
		GlId:       glID,
		GlUsername: glID,
		Name:       []byte(glID),
		Email:      []byte(glID + "@example.com"),
	}
}

// agentLanding is one UserLand request of a test. An empty agent makes the request without an
// identity, and a nil user is the default test user.
type agentLanding struct {
	agent   string
	user    *gitalypb.User
	commit  git.ObjectID
	resolve bool
}

// landInOrder lands the changes concurrently and starts them 30ms apart in the given order, so that they
// reach their lane in that order. It returns the outcomes in the order of the landings.
func landInOrder(ctx context.Context, client gitalypb.OperationServiceClient, repo *gitalypb.Repository, landings ...agentLanding) ([]*gitalypb.UserLandResponse, []error) {
	responses := make([]*gitalypb.UserLandResponse, len(landings))
	errs := make([]error, len(landings))

	var wg sync.WaitGroup
	for i, landing := range landings {
		if i > 0 {
			time.Sleep(30 * time.Millisecond)
		}

		wg.Add(1)
		go func(i int, landing agentLanding) {
			defer wg.Done()

			req := landRequest(repo, git.DefaultBranch, landing.commit)
			if landing.user != nil {
				req.User = landing.user
			}
			req.ResolveConflicts = landing.resolve

			reqCtx := ctx
			if landing.agent != "" {
				reqCtx = metadata.AppendToOutgoingContext(ctx, testAgentHeader, landing.agent)
			}

			responses[i], errs[i] = client.UserLand(reqCtx, req)
		}(i, landing)
	}
	wg.Wait()

	return responses, errs
}

// resolverPairEntries returns the tree of a branch whose files a and b hold the given contents.
func resolverPairEntries(a, b string) []gittest.TreeEntry {
	return []gittest.TreeEntry{
		{Mode: "100644", Path: "a", Content: a},
		{Mode: "100644", Path: "b", Content: b},
	}
}

// resolverBatchFixture is a repository whose branch main has moved on from base to ours. The change
// disjoint lands by rebase without a conflict, and the change conflicting conflicts with ours in a and b.
type resolverBatchFixture struct {
	ctx         context.Context
	cfg         config.Cfg
	client      gitalypb.OperationServiceClient
	repo        *gitalypb.Repository
	repoPath    string
	dir         string
	disjoint    git.ObjectID
	conflicting git.ObjectID
}

// newResolverBatchFixture starts the service with conflict resolution for the given actors.
func newResolverBatchFixture(t *testing.T, ctx context.Context, actors []string) resolverBatchFixture {
	t.Helper()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())

	graft := agentBatchingGraft(nil)
	rcfg := enabledResolver(argv)
	rcfg.Actors = actors
	graft.Land.Resolver = rcfg

	ctx, cfg, client := setupAgentLandService(t, ctx, graft)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(resolverPairEntries("base", "base")...),
	)
	gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(resolverPairEntries("ours", "ours")...),
	)
	conflicting := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithTreeEntries(resolverPairEntries("theirs", "theirs")...),
	)
	disjoint := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base"},
			gittest.TreeEntry{Mode: "100644", Path: "c", Content: "c"},
		),
	)

	return resolverBatchFixture{
		ctx:         ctx,
		cfg:         cfg,
		client:      client,
		repo:        repoProto,
		repoPath:    repoPath,
		dir:         dir,
		disjoint:    disjoint,
		conflicting: conflicting,
	}
}

func TestLandBatch_resolverActorsArePerRequest(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchResolverActorsArePerRequest)
}

func testLandBatchResolverActorsArePerRequest(t *testing.T, ctx context.Context) {
	t.Parallel()

	t.Run("unlisted actor behind a listed one is not resolved", func(t *testing.T) {
		t.Parallel()

		fx := newResolverBatchFixture(t, ctx, []string{"agent-a"})

		responses, errs := landInOrder(fx.ctx, fx.client, fx.repo,
			agentLanding{agent: "agent-a", commit: fx.disjoint},
			agentLanding{agent: "agent-b", commit: fx.conflicting, resolve: true},
		)

		require.NoError(t, errs[0])
		require.NotNil(t, responses[0])
		testhelper.RequireGrpcCode(t, errs[1], codes.FailedPrecondition)
		require.Zero(t, resolverCalls(t, fx.dir), "agent-b may not resolve, so the resolver must not run for it")
	})

	t.Run("listed actor behind an unlisted one is resolved", func(t *testing.T) {
		t.Parallel()

		fx := newResolverBatchFixture(t, ctx, []string{"agent-a"})

		responses, errs := landInOrder(fx.ctx, fx.client, fx.repo,
			agentLanding{agent: "agent-b", commit: fx.disjoint},
			agentLanding{agent: "agent-a", commit: fx.conflicting, resolve: true},
		)

		require.NoError(t, errs[0])
		require.NoError(t, errs[1])
		require.True(t, responses[1].GetRewritten())
		require.Equal(t, "graft-resolve/fake", responses[1].GetResolver())
		require.Equal(t, 1, resolverCalls(t, fx.dir))
		require.Equal(t, "merged-a\n", string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "show", "refs/heads/"+git.DefaultBranch+":a")))
	})
}

func TestLandBatch_policyOfNonAgentIsEvaluatedByGLID(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchPolicyOfNonAgentIsEvaluatedByGLID)
}

func testLandBatchPolicyOfNonAgentIsEvaluatedByGLID(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupAgentLandService(t, ctx, agentBatchingGraft(denyMainFor("eve")))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(baseEntry),
	)
	eve := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
		gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: "e", Content: "e"}))
	alice := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
		gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a"}))

	// Neither request has an agent identity, so the policy must judge the user ID of each request, as
	// the pre-receive hook does for a request that lands alone.
	responses, errs := landInOrder(ctx, client, repoProto,
		agentLanding{user: userWithGLID("eve"), commit: eve},
		agentLanding{user: userWithGLID("alice"), commit: alice},
	)

	testhelper.RequireGrpcCode(t, errs[0], codes.PermissionDenied)
	require.Equal(t, "eve", landErrorDetail(t, errs[0]).GetAccessCheck().GetUserId())
	require.NoError(t, errs[1])
	require.NotNil(t, responses[1])

	// Eve's change did not land, so the branch holds alice's change alone on top of base.
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, alice)
}

func TestLandBatch_deniedAgentDoesNotTakeBatchDown(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchDeniedAgentDoesNotTakeBatchDown)
}

func testLandBatchDeniedAgentDoesNotTakeBatchDown(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupAgentLandService(t, ctx, agentBatchingGraft(denyMainFor("bot")))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(baseEntry),
	)
	change := func(name string) git.ObjectID {
		return gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
			gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: name, Content: name}))
	}

	// The denied agent submits first, so its identity is the one of the first request of the batch.
	responses, errs := landInOrder(ctx, client, repoProto,
		agentLanding{agent: "bot", commit: change("b")},
		agentLanding{agent: "alice", commit: change("a")},
		agentLanding{agent: "carol", commit: change("c")},
	)

	testhelper.RequireGrpcCode(t, errs[0], codes.PermissionDenied)
	require.Equal(t, "bot", landErrorDetail(t, errs[0]).GetAccessCheck().GetUserId())

	for _, i := range []int{1, 2} {
		require.NoError(t, errs[i])
		require.Equal(t, int32(3), responses[i].GetBatchSize(), "the denied agent must not take the batch down")
	}

	files := strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath, "ls-tree", "--name-only", "refs/heads/"+git.DefaultBranch)))
	require.ElementsMatch(t, []string{"base", "a", "c"}, files)
}

func TestLandBatch_changesetRecordsMembersWithRealActors(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchChangesetRecordsMembersWithRealActors)
}

func testLandBatchChangesetRecordsMembersWithRealActors(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupAgentLandService(t, ctx, agentBatchingGraft(nil))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(baseEntry))
	change := func(name string) git.ObjectID {
		return gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
			gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: name, Content: name}))
	}

	// Two agents and a user without an agent identity land in one batch. The combined update is
	// recorded with the synthetic actor, and the members with the actors that really landed.
	responses, errs := landInOrder(ctx, client, repoProto,
		agentLanding{agent: "agent-a", commit: change("a")},
		agentLanding{agent: "agent-b", commit: change("b")},
		agentLanding{user: userWithGLID("u-glid"), commit: change("c")},
	)
	for i := range errs {
		require.NoError(t, errs[i])
		require.NotNil(t, responses[i])
	}

	entries, err := changelog.List(repoPath, 0, 100)
	require.NoError(t, err)

	var batches []changelog.Entry
	for _, entry := range entries {
		if entry.Reference == git.DefaultRef.String() && entry.Operation == changelog.OperationLandBatch {
			batches = append(batches, entry)
		}
	}
	require.NotEmpty(t, batches, "the batched landings must be recorded as land_batch")

	var actors []string
	for _, batch := range batches {
		require.Equal(t, "graft-batch", batch.Actor, "the combined update must not carry a submitter's identity")
		for _, member := range batch.Members {
			actors = append(actors, member.Actor)
		}
	}
	require.ElementsMatch(t, []string{"agent-a", "agent-b", "u-glid"}, actors)
}

func TestLandBatchSubmitError(t *testing.T) {
	t.Parallel()

	testhelper.RequireGrpcCode(t, batchSubmitError(landlane.ErrClosed), codes.Unavailable)
	testhelper.RequireGrpcCode(t, batchSubmitError(context.DeadlineExceeded), codes.DeadlineExceeded)
	testhelper.RequireGrpcCode(t, batchSubmitError(context.Canceled), codes.Canceled)
}

func TestLandBatchIdentityIsPerRequest(t *testing.T) {
	t.Parallel()

	lane := identity.NewContext(context.Background(), identity.Identity{Name: "agent-a", Source: identity.SourceAgent})
	reqCtx := identity.NewContext(context.Background(), identity.Identity{Name: "agent-b", Source: identity.SourceAgent})

	require.Equal(t, "agent-b", identity.Actor(requestIdentityContext(lane, &landBatchRequest{ctx: reqCtx}), ""))
	require.Empty(t, identity.Actor(requestIdentityContext(lane, &landBatchRequest{ctx: context.Background()}), ""))
	require.Empty(t, identity.Actor(batchUpdateContext(lane), ""), "the combined update has no submitter's identity")
}

func TestLandBatchActor(t *testing.T) {
	t.Parallel()

	in := &gitalypb.UserLandRequest{User: &gitalypb.User{GlId: "user-1"}}

	agent := identity.NewContext(context.Background(), identity.Identity{Name: "agent-a", Source: identity.SourceAgent})
	require.Equal(t, "agent-a", landBatchActor(agent, in))

	shared := identity.NewContext(context.Background(), identity.Identity{Name: "shared", Source: identity.SourceSharedToken})
	require.Equal(t, "user-1", landBatchActor(shared, in))

	require.Equal(t, "user-1", landBatchActor(context.Background(), in))
}
