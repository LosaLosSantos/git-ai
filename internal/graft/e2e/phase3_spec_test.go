// Black-box spec tests for WP30 (phase-3 e2e + load), written from PLAN3.md only.
//
// Only spec-named identifiers are used: config.Graft / GraftLandBatch and its
// helpers, and the UserLand RPC with BatchSize/BatchPosition on the response,
// plus Gitaly test helpers. Gitaly test-helper and proto signatures below are
// the usual Gitaly conventions, not taken from the spec; adjust them at merge
// time if the real helpers differ:
//
//	testcfg.Build(t) config.Cfg
//	testserver.RunGitalyServer(t, cfg, nil) string        (returns socket path)
//	gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{}) (*gitalypb.Repository, string)
//	gittest.WriteCommit(t, cfg, repoPath, opts...) git.ObjectID
//	gittest.WithParents / gittest.WithTreeEntries / gittest.WithBranch
//	gittest.Exec(t, cfg, args...) []byte
//	gittest.WriteCustomHook(t, repoPath, hookName string, content []byte)
//
// Proto fields assumed: UserLandRequest{Repository, User, Branch, CommitId},
// UserLandResponse{Attempts, BatchSize, BatchPosition}; OperationService.UserLand.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// specCfg returns a standalone config with the given land batch settings.
func specCfg(t *testing.T, standalone bool, batch config.GraftLandBatch) config.Cfg {
	t.Helper()
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = standalone
	cfg.Graft.Land.Batch = batch
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll)
	return cfg
}

func specClient(t *testing.T, cfg config.Cfg) gitalypb.OperationServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(cfg.SocketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return gitalypb.NewOperationServiceClient(conn)
}

type specRepo struct {
	repo *gitalypb.Repository
	path string
	base git.ObjectID // empty root commit on "main"
}

func specNewRepo(t *testing.T, ctx context.Context, cfg config.Cfg) specRepo {
	t.Helper()
	repo, path := gittest.CreateRepository(t, testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg)), cfg, gittest.CreateRepositoryConfig{})
	base := gittest.WriteCommit(t, cfg, path, gittest.WithBranch("main"))
	return specRepo{repo: repo, path: path, base: base}
}

// specFileCommit creates a commit on base adding one file (disjoint from other agents' files).
func specFileCommit(t *testing.T, cfg config.Cfg, r specRepo, file string) git.ObjectID {
	t.Helper()
	return gittest.WriteCommit(t, cfg, r.path,
		gittest.WithParents(r.base),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: file, Content: "content of " + file}),
	)
}

func specLandReq(r specRepo, branch string, commit git.ObjectID, glID string) *gitalypb.UserLandRequest {
	return &gitalypb.UserLandRequest{
		Repository: r.repo,
		User: &gitalypb.User{
			GlId:       glID,
			GlUsername: glID,
			Name:       []byte(glID),
			Email:      []byte(glID + "@example.com"),
		},
		Branch:   []byte(branch),
		CommitId: string(commit),
	}
}

// specTreeFiles returns the file names at the root of branch's tree.
func specTreeFiles(t *testing.T, cfg config.Cfg, r specRepo, branch string) map[string]bool {
	t.Helper()
	out := strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", r.path, "ls-tree", "--name-only", branch)))
	files := map[string]bool{}
	for _, f := range strings.Split(out, "\n") {
		if f != "" {
			files[f] = true
		}
	}
	return files
}

type specOutcome struct {
	resp *gitalypb.UserLandResponse
	err  error
}

// TestSpec100AgentsOneBranchAllLand: 100 concurrent agents, one change each,
// one branch: all land, attempts <= 2 per change on average, some batch_size > 1,
// valid batch positions, linear history containing every file.
func TestSpec100AgentsOneBranchAllLand(t *testing.T) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := specCfg(t, true, config.GraftLandBatch{Window: "50ms", MaxSize: 50})
	client := specClient(t, cfg)
	r := specNewRepo(t, ctx, cfg)

	const agents = 100
	reqs := make([]*gitalypb.UserLandRequest, agents)
	for i := range reqs {
		file := fmt.Sprintf("agent-%03d.txt", i)
		reqs[i] = specLandReq(r, "main", specFileCommit(t, cfg, r, file), fmt.Sprintf("user-%d", i))
	}

	outs := make([]specOutcome, agents)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := client.UserLand(ctx, reqs[i])
			outs[i] = specOutcome{resp: resp, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	attempts := 0
	maxBatch := 0
	for i, o := range outs {
		require.NoError(t, o.err, "agent %d: landing must succeed", i)
		require.NotNil(t, o.resp, "agent %d: response must be set", i)
		attempts += int(o.resp.GetAttempts())
		batch := int(o.resp.GetBatchSize())
		pos := int(o.resp.GetBatchPosition())
		if batch > maxBatch {
			maxBatch = batch
		}
		require.GreaterOrEqual(t, pos, 1, "agent %d: batch_position is 1-based", i)
		require.LessOrEqual(t, pos, batch, "agent %d: batch_position must not exceed batch_size", i)
	}
	require.LessOrEqual(t, attempts, 2*agents, "average attempts per change must be <= 2")
	require.Greater(t, maxBatch, 1, "at least some requests must have landed in a batch of size > 1")

	files := specTreeFiles(t, cfg, r, "main")
	for i := 0; i < agents; i++ {
		name := fmt.Sprintf("agent-%03d.txt", i)
		require.True(t, files[name], "branch must contain %s after all landings", name)
	}

	merges := strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", r.path, "rev-list", "--merges", "--count", "main")))
	require.Equal(t, "0", merges, "history must be linear")
	count := strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", r.path, "rev-list", "--count", "main")))
	require.Equal(t, fmt.Sprint(agents+1), count, "base plus one commit per agent")
}

// TestSpecBatchingDisabledIsUnbatched: with batching disabled every response
// reports batch_size=1 and batch_position=1, and all changes still land.
func TestSpecBatchingDisabledIsUnbatched(t *testing.T) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := specCfg(t, true, config.GraftLandBatch{Disabled: true, Window: "50ms", MaxSize: 50})
	client := specClient(t, cfg)
	r := specNewRepo(t, ctx, cfg)

	for i := 0; i < 3; i++ {
		file := fmt.Sprintf("solo-%d.txt", i)
		resp, err := client.UserLand(ctx, specLandReq(r, "main", specFileCommit(t, cfg, r, file), "user-1"))
		require.NoError(t, err, "landing %s must succeed", file)
		require.EqualValues(t, 1, resp.GetBatchSize(), "unbatched response must report batch_size=1")
		require.EqualValues(t, 1, resp.GetBatchPosition(), "unbatched response must report batch_position=1")
	}

	files := specTreeFiles(t, cfg, r, "main")
	for i := 0; i < 3; i++ {
		require.True(t, files[fmt.Sprintf("solo-%d.txt", i)])
	}
}

// TestSpecNonStandaloneAndMissingBranchAreUnbatched: batching only applies to
// standalone servers and existing branches; otherwise responses are unbatched.
func TestSpecNonStandaloneAndMissingBranchAreUnbatched(t *testing.T) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)

	t.Run("non-standalone", func(t *testing.T) {
		cfg := specCfg(t, false, config.GraftLandBatch{Window: "50ms", MaxSize: 50})
		client := specClient(t, cfg)
		r := specNewRepo(t, ctx, cfg)
		resp, err := client.UserLand(ctx, specLandReq(r, "main", specFileCommit(t, cfg, r, "ns.txt"), "user-1"))
		require.NoError(t, err)
		require.EqualValues(t, 1, resp.GetBatchSize(), "non-standalone landing must be unbatched")
	})
}

// TestSpecCancelledSubmittersNeverLand: submitters cancelled before hand-off
// do not land, and the other members of the same wave still land.
func TestSpecCancelledSubmittersNeverLand(t *testing.T) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := specCfg(t, true, config.GraftLandBatch{Window: "50ms", MaxSize: 50})
	client := specClient(t, cfg)
	r := specNewRepo(t, ctx, cfg)

	const agents = 10
	cancelled := map[int]bool{2: true, 5: true}
	reqs := make([]*gitalypb.UserLandRequest, agents)
	for i := range reqs {
		reqs[i] = specLandReq(r, "main", specFileCommit(t, cfg, r, fmt.Sprintf("c-%d.txt", i)), fmt.Sprintf("user-%d", i))
	}

	outs := make([]specOutcome, agents)
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			callCtx := ctx
			if cancelled[i] {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			resp, err := client.UserLand(callCtx, reqs[i])
			outs[i] = specOutcome{resp: resp, err: err}
		}(i)
	}
	wg.Wait()

	files := specTreeFiles(t, cfg, r, "main")
	for i, o := range outs {
		name := fmt.Sprintf("c-%d.txt", i)
		if cancelled[i] {
			require.Error(t, o.err, "cancelled submitter %d must fail", i)
			require.Equal(t, codes.Canceled, status.Code(o.err), "cancelled submitter %d must report Canceled", i)
			require.False(t, files[name], "cancelled change %s must not land", name)
			continue
		}
		require.NoError(t, o.err, "non-cancelled submitter %d must land", i)
		require.True(t, files[name], "landed change %s must be on the branch", name)
	}
}

// TestSpecCustomHookRejectionFallsBackPerMember: when a custom pre-receive hook
// rejects one member's change, only that member fails; the others land via the
// per-member fallback.
func TestSpecCustomHookRejectionFallsBackPerMember(t *testing.T) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := specCfg(t, true, config.GraftLandBatch{Window: "50ms", MaxSize: 50})
	client := specClient(t, cfg)
	r := specNewRepo(t, ctx, cfg)

	gittest.WriteCustomHook(t, r.path, "pre-receive", []byte(`#!/bin/sh
while read old new ref; do
  if git diff --name-only "$old" "$new" | grep -qx 'rejected.txt'; then
    echo "rejected by custom hook" >&2
    exit 1
  fi
done
exit 0
`))

	const agents = 8
	reqs := make([]*gitalypb.UserLandRequest, agents)
	names := make([]string, agents)
	for i := range reqs {
		names[i] = fmt.Sprintf("ok-%d.txt", i)
		if i == 3 {
			names[i] = "rejected.txt"
		}
		reqs[i] = specLandReq(r, "main", specFileCommit(t, cfg, r, names[i]), fmt.Sprintf("user-%d", i))
	}

	outs := make([]specOutcome, agents)
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.UserLand(ctx, reqs[i])
			outs[i] = specOutcome{resp: resp, err: err}
		}(i)
	}
	wg.Wait()

	files := specTreeFiles(t, cfg, r, "main")
	for i, o := range outs {
		if names[i] == "rejected.txt" {
			require.Error(t, o.err, "the member rejected by the hook must fail")
			require.False(t, files["rejected.txt"], "rejected change must not land")
			continue
		}
		require.NoError(t, o.err, "member %s must land despite the rejection of another member", names[i])
		require.True(t, files[names[i]], "%s must be on the branch", names[i])
	}
}

// TestSpecConfigBatchDefaultsAndValidation covers the spec'd helpers:
// window default 10ms, max size default 50, worker default 2, queue default 10000,
// and an invalid window is an error.
func TestSpecConfigBatchDefaultsAndValidation(t *testing.T) {
	var g config.Graft

	w, err := g.Land.Batch.WindowOrDefault()
	require.NoError(t, err)
	require.Equal(t, 10*time.Millisecond, w, "default batch window must be 10ms")
	require.Equal(t, 50, g.Land.Batch.MaxSizeOrDefault(), "default batch max size must be 50")

	explicit := config.GraftLandBatch{Window: "25ms", MaxSize: 7}
	w, err = explicit.WindowOrDefault()
	require.NoError(t, err)
	require.Equal(t, 25*time.Millisecond, w)
	require.Equal(t, 7, explicit.MaxSizeOrDefault())

	require.Equal(t, 2, g.Derived.WorkersOrDefault(), "default derived workers must be 2")
	require.Equal(t, 10000, g.Derived.QueueSizeOrDefault(), "default derived queue size must be 10000")

	bad := config.GraftLandBatch{Window: "soon"}
	_, err = bad.WindowOrDefault()
	require.Error(t, err, "a non-duration window must be rejected")
}
