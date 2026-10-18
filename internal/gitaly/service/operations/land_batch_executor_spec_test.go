package operations

import (
	"context"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/ancestry"
	"github.com/feder-cr/git-ai/internal/graft/landlane"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// Spec: WP35 "Early completion" — CompleteEarly only succeeds from an executor
// context of the landlane package and a valid index. Outside an executor it
// must return false and must not panic.
func TestSpecCompleteEarlyRejectsNonExecutorContext(t *testing.T) {
	ctx := testhelper.Context(t)

	require.False(t, landlane.CompleteEarly(ctx, 0, landlane.Result[string]{}),
		"a plain context is not an executor context: must return false")
	require.False(t, landlane.CompleteEarly(ctx, -1, landlane.Result[string]{}),
		"negative index must return false")
	require.False(t, landlane.CompleteEarly(ctx, 1<<20, landlane.Result[string]{}),
		"out-of-range index must return false")
}

// Spec: WP37 "CompleteEarly ... concurrency-safe" — calling it from many goroutines
// with a non-executor context is safe and always reports false.
func TestSpecCompleteEarlyConcurrentCallsAreSafe(t *testing.T) {
	ctx := testhelper.Context(t)

	const workers = 32
	results := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			results <- landlane.CompleteEarly(ctx, i, landlane.Result[int]{})
		}(i)
	}

	for i := 0; i < workers; i++ {
		require.False(t, <-results)
	}
}

// Spec: WP37/config — TargetLatency "" means DefaultGraftLandBatchTargetLatency (500ms).
func TestSpecTargetLatencyEmptyMeansDefault(t *testing.T) {
	cfg := config.GraftLandBatch{TargetLatency: ""}

	d, err := cfg.TargetLatencyOrDefault()
	require.NoError(t, err)
	require.Equal(t, 500*time.Millisecond, d)
	require.Equal(t, config.DefaultGraftLandBatchTargetLatency, d)
}

// Spec: WP37/config — TargetLatency "0s" disables adaptation, so the parsed value is zero.
func TestSpecTargetLatencyZeroDisablesAdaptation(t *testing.T) {
	cfg := config.GraftLandBatch{TargetLatency: "0s"}

	d, err := cfg.TargetLatencyOrDefault()
	require.NoError(t, err)
	require.Equal(t, time.Duration(0), d)
}

// Spec: WP36 — "New returns Graph when enabled (falling back to Git on any
// construction error, logged), else Git." Whichever checker is returned, its
// answers must be exactly the Git answers, so enabled=true must not change outcomes.
func TestSpecAncestryNewEnabledAgreesWithGit(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	base := gittest.WriteCommit(t, cfg, repoPath)
	left := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))
	right := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))
	merge := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(left, right))

	reference := ancestry.Git(repo)
	checker := ancestry.New(ctx, true, repo, nil, nil, testhelper.NewLogger(t))
	require.NotNil(t, checker)

	// Phase 4b: the checker answers one question, the range of a change (PLAN8).
	pairs := [][2]git.ObjectID{{base, merge}, {merge, base}, {left, right}, {right, left}, {base, base}}
	for _, p := range pairs {
		want, wantErr := reference.Range(ctx, p[0], p[1])
		got, gotErr := checker.Range(ctx, p[0], p[1])
		require.Equal(t, wantErr != nil, gotErr != nil)
		require.ElementsMatch(t, want, got, "Range(%s, %s)", p[0], p[1])
	}
}

// Spec: WP37 — CompleteEarly returns false unless ctx is an executor context of
// the landlane package. An unrelated context value must not make it succeed.
func TestSpecCompleteEarlyIgnoresUnrelatedContextValues(t *testing.T) {
	type otherKey struct{}
	ctx := context.WithValue(testhelper.Context(t), otherKey{}, "not an executor")

	require.False(t, landlane.CompleteEarly(ctx, 0, landlane.Result[bool]{}))
}
