package operations

// Black-box spec tests for WP22 (batch executor in operations), written from
// doc/graft/PLAN3.md only. Assumptions about Gitaly test helpers and proto
// field names are noted inline; adjust them if the gate reports compile errors.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// specWP22Env is a standalone server with land batching enabled (50ms window)
// and one repository whose branch "main" points at a base commit containing
// the file "shared".
type specWP22Env struct {
	cfg      config.Cfg
	repo     *gitalypb.Repository
	repoPath string
	client   gitalypb.OperationServiceClient
	base     string
	md       metadata.MD
}

func specWP22User(name string) *gitalypb.User {
	return &gitalypb.User{
		GlId:       "user-" + name,
		GlUsername: name,
		Name:       []byte(name),
		Email:      []byte(name + "@example.com"),
	}
}

func specWP22NewEnv(t *testing.T, mutate func(cfg *config.Cfg)) *specWP22Env {
	t.Helper()

	// Gate adaptation: the spec writer assumed testcfg.BuildWithRepo and
	// testserver.RunGitalyServer(t, cfg, nil, nil); the package's own helpers are used instead.
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	cfg.Graft.Land.Batch = config.GraftLandBatch{Window: "50ms", MaxSize: 50}
	if mutate != nil {
		mutate(&cfg)
	}

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, true)
	ctx, cfg, client := setupOperationsServiceWithCfg(t, ctx, cfg)
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	md, _ := metadata.FromOutgoingContext(ctx)

	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch("main"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "shared", Content: "base"}),
	)

	return &specWP22Env{
		cfg:      cfg,
		repo:     repo,
		repoPath: repoPath,
		client:   client,
		base:     base.String(),
		md:       md,
	}
}

// specWP22Commit creates an unreferenced commit on top of the base commit that
// adds (or replaces) one file.
func (e *specWP22Env) specWP22Commit(t *testing.T, path, content string) string {
	t.Helper()
	oid := gittest.WriteCommit(t, e.cfg, e.repoPath,
		gittest.WithParents(git.ObjectID(e.base)),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: path, Content: content}),
		gittest.WithMessage("commit "+path),
	)
	return oid.String()
}

func (e *specWP22Env) specWP22Land(ctx context.Context, branch, commit, actor string) (*gitalypb.UserLandResponse, error) {
	ctx = metadata.NewOutgoingContext(ctx, e.md)
	return e.client.UserLand(ctx, &gitalypb.UserLandRequest{
		Repository: e.repo,
		User:       specWP22User(actor),
		Branch:     []byte(branch),
		CommitId:   commit,
	})
}

func (e *specWP22Env) specWP22MainFiles(t *testing.T) []string {
	t.Helper()
	out := gittest.Exec(t, e.cfg, "-C", e.repoPath, "ls-tree", "--name-only", "main")
	return strings.Fields(string(out))
}

func TestSpecBatchDisjointLandingsAllSucceedInBatches(t *testing.T) {
	env := specWP22NewEnv(t, nil)

	const n = 30
	commits := make([]string, n)
	for i := range commits {
		commits[i] = env.specWP22Commit(t, fmt.Sprintf("file-%02d", i), "content")
	}

	type result struct {
		resp *gitalypb.UserLandResponse
		err  error
	}
	results := make([]result, n)

	var wg sync.WaitGroup
	for i := range commits {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := env.specWP22Land(context.Background(), "main", commits[i], fmt.Sprintf("agent%02d", i))
			results[i] = result{resp, err}
		}(i)
	}
	wg.Wait()

	maxBatch := int32(0)
	totalAttempts := int32(0)
	for i, r := range results {
		require.NoError(t, r.err, "request %d must succeed", i)
		require.NotNil(t, r.resp, "request %d must return a response", i)
		if r.resp.GetBatchSize() > maxBatch {
			maxBatch = r.resp.GetBatchSize()
		}
		totalAttempts += r.resp.GetAttempts()
	}

	require.Greater(t, maxBatch, int32(1), "at least one response must report batch_size > 1 with a 50ms window")
	require.LessOrEqual(t, totalAttempts, int32(2*n), "total attempts must be at most 2 per request")

	files := env.specWP22MainFiles(t)
	for i := 0; i < n; i++ {
		require.Contains(t, files, fmt.Sprintf("file-%02d", i), "every landed file must be on main")
	}

	merges := strings.TrimSpace(string(gittest.Exec(t, env.cfg, "-C", env.repoPath, "rev-list", "--merges", "--count", "main")))
	require.Equal(t, "0", merges, "history of main must be linear (no merge commits)")
}

func TestSpecBatchBatchPositionIsOneBasedWithinBatch(t *testing.T) {
	env := specWP22NewEnv(t, nil)

	const n = 10
	commits := make([]string, n)
	for i := range commits {
		commits[i] = env.specWP22Commit(t, fmt.Sprintf("pos-%02d", i), "x")
	}

	var wg sync.WaitGroup
	responses := make([]*gitalypb.UserLandResponse, n)
	errs := make([]error, n)
	for i := range commits {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			responses[i], errs[i] = env.specWP22Land(context.Background(), "main", commits[i], "poser")
		}(i)
	}
	wg.Wait()

	for i := range responses {
		require.NoError(t, errs[i])
		require.GreaterOrEqual(t, responses[i].GetBatchPosition(), int32(1), "batch_position is 1-based")
		require.LessOrEqual(t, responses[i].GetBatchPosition(), responses[i].GetBatchSize(),
			"batch_position must not exceed batch_size")
	}
}

func TestSpecBatchCancelledSubmitterBeforeHandOffReturnsCanceled(t *testing.T) {
	env := specWP22NewEnv(t, nil)
	commit := env.specWP22Commit(t, "cancelled", "x")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := env.specWP22Land(ctx, "main", commit, "dave")
	require.Error(t, err)
	require.Equal(t, codes.Canceled, status.Code(err), "a cancelled submitter must see a Canceled status")
	require.NotContains(t, env.specWP22MainFiles(t), "cancelled",
		"a cancelled request must not land")
}

func TestSpecBatchDisabledKeepsUnbatchedBehaviour(t *testing.T) {
	env := specWP22NewEnv(t, func(cfg *config.Cfg) {
		cfg.Graft.Land.Batch.Disabled = true
	})

	for i := 0; i < 5; i++ {
		commit := env.specWP22Commit(t, fmt.Sprintf("solo-%d", i), "x")
		resp, err := env.specWP22Land(context.Background(), "main", commit, "erin")
		require.NoError(t, err)
		require.Equal(t, int32(1), resp.GetBatchSize(), "batching disabled: batch_size must be 1")
		require.Equal(t, int32(1), resp.GetBatchPosition(), "batching disabled: batch_position must be 1")
	}
}

func TestSpecBatchNonStandaloneIsUnbatched(t *testing.T) {
	env := specWP22NewEnv(t, func(cfg *config.Cfg) {
		cfg.Graft.Standalone = false
	})

	var wg sync.WaitGroup
	const n = 8
	responses := make([]*gitalypb.UserLandResponse, n)
	errs := make([]error, n)
	commits := make([]string, n)
	for i := range commits {
		commits[i] = env.specWP22Commit(t, fmt.Sprintf("ns-%d", i), "x")
	}
	for i := range commits {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			responses[i], errs[i] = env.specWP22Land(context.Background(), "main", commits[i], "frank")
		}(i)
	}
	wg.Wait()

	for i := range responses {
		require.NoError(t, errs[i])
		require.Equal(t, int32(1), responses[i].GetBatchSize(),
			"non-standalone servers must not batch: batch_size must be 1")
	}
}

func TestSpecBatchPreReceiveRejectionIsAttributedToEachMember(t *testing.T) {
	env := specWP22NewEnv(t, func(cfg *config.Cfg) {})

	// Reject every ref update through a custom pre-receive hook.
	gittest.WriteCustomHook(t, env.repoPath, "pre-receive", []byte("#!/bin/sh\necho rejected-by-spec >&2\nexit 1\n"))

	before := strings.TrimSpace(string(gittest.Exec(t, env.cfg, "-C", env.repoPath, "rev-parse", "refs/heads/main")))

	const n = 4
	commits := make([]string, n)
	for i := range commits {
		commits[i] = env.specWP22Commit(t, fmt.Sprintf("rej-%d", i), "x")
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range commits {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = env.specWP22Land(context.Background(), "main", commits[i], "hank")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.Error(t, err, "member %d must be rejected by the pre-receive hook", i)
	}
	after := strings.TrimSpace(string(gittest.Exec(t, env.cfg, "-C", env.repoPath, "rev-parse", "refs/heads/main")))
	require.Equal(t, before, after, "a rejected batch must leave the branch unchanged")
}
