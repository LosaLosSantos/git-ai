package operations

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestLandResolveBench measures, under churn, what a land that the server resolves costs the
// lands around it (PLAN10 15.8): resolve lands that all conflict on one file land at once with
// plain lands that conflict with nothing, through the lanes of a standalone server, with a resolver
// that takes GRAFT_LAND_BENCH_RESOLVE_MS to answer. It reports the latency of both kinds of land
// and how long the lane was held (graft_land_batch_phase_seconds, phase total). It runs only with
// GRAFT_LAND_BENCH=1, alone, and builds on the trees before and after the resolution left the lane.
func TestLandResolveBench(t *testing.T) {
	if os.Getenv("GRAFT_LAND_BENCH") != "1" {
		t.Skip("set GRAFT_LAND_BENCH=1 to run the bench")
	}
	resolveLands := benchEnvInt(t, "GRAFT_LAND_BENCH_RESOLVE", 6)
	plainLands := benchEnvInt(t, "GRAFT_LAND_BENCH_PLAIN", 6)
	resolveDelay := time.Duration(benchEnvInt(t, "GRAFT_LAND_BENCH_RESOLVE_MS", 500)) * time.Millisecond

	dir := t.TempDir()
	script := filepath.Join(dir, "resolver.sh")
	answer, err := json.Marshal(answerOfPaths("a"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "answer.json"), answer, 0o600))
	require.NoError(t, os.WriteFile(script, []byte(fmt.Sprintf(`#!/bin/sh
DIR='%s'
cat > /dev/null
echo x >> "$DIR/calls"
sleep %.3f
cat "$DIR/answer.json"
`, dir, resolveDelay.Seconds())), 0o755))

	// resolve_rounds is set through JSON, which the tree before it ignores.
	var rcfg config.GraftResolver
	require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(`{"enabled":true,"command":[%q],"timeout":"10m","resolve_rounds":%d}`,
		script, resolveLands+1)), &rcfg))

	cfg := testcfg.Build(t)
	cfg.Graft = config.Graft{
		Standalone: true,
		Land:       config.GraftLand{Batch: config.GraftLandBatch{Window: "20ms"}, Resolver: rcfg},
	}
	ctx, cfg, client := setupOperationsServiceWithCfg(t, testhelper.Context(t), cfg)
	ctx = featureflag.ContextWithFeatureFlag(ctx, featureflag.GPGSigning, true)
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	file := func(path, content string) gittest.TreeEntry {
		return gittest.TreeEntry{Mode: "100644", Path: path, Content: content}
	}
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(file("a", "base\n")))
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(file("a", "ours\n")))

	type land struct {
		commit  git.ObjectID
		resolve bool
	}
	var lands []land
	for i := 0; i < resolveLands; i++ {
		lands = append(lands, land{resolve: true, commit: gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
			gittest.WithTreeEntries(file("a", fmt.Sprintf("theirs-%d\n", i)), file(fmt.Sprintf("r%d", i), "r\n")))})
	}
	for i := 0; i < plainLands; i++ {
		lands = append(lands, land{commit: gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
			gittest.WithTreeEntries(file("a", "base\n"), file(fmt.Sprintf("p%d", i), "p\n")))})
	}

	laneBefore := benchLaneSeconds(t)
	latencies := map[bool][]time.Duration{}
	failures := map[bool]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := time.Now()
	for i, l := range lands {
		wg.Add(1)
		go func(i int, l land) {
			defer wg.Done()
			// The plain lands start once the resolve lands are on their way, as the churn of a busy
			// branch would.
			if !l.resolve {
				time.Sleep(50 * time.Millisecond)
			}
			began := time.Now()
			_, err := client.UserLand(ctx, &gitalypb.UserLandRequest{
				Repository: repo, User: gittest.TestUser, Branch: []byte(git.DefaultBranch),
				CommitId: l.commit.String(), ResolveConflicts: l.resolve, MergeMessage: []byte(fmt.Sprintf("Land %d", i)),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures[l.resolve]++
				t.Logf("land %d (resolve %v): %v", i, l.resolve, err)
				return
			}
			latencies[l.resolve] = append(latencies[l.resolve], time.Since(began))
		}(i, l)
	}
	wg.Wait()
	wall := time.Since(start)
	lane := benchLaneSeconds(t) - laneBefore
	raw, err := os.ReadFile(filepath.Join(dir, "calls"))
	require.NoError(t, err)
	calls := len(raw) / 2

	t.Logf("BENCH resolve_lands=%d plain_lands=%d resolve_delay=%s wall=%s lane_held=%.2fs resolver_calls=%d",
		resolveLands, plainLands, resolveDelay, wall.Round(time.Millisecond), lane, calls)
	for _, resolve := range []bool{true, false} {
		name := map[bool]string{true: "resolve", false: "plain"}[resolve]
		d := latencies[resolve]
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		if len(d) == 0 {
			t.Logf("BENCH %s: landed 0, failed %d", name, failures[resolve])
			continue
		}
		t.Logf("BENCH %s: landed %d, failed %d, p50 %s, p95 %s, max %s", name, len(d), failures[resolve],
			d[len(d)/2].Round(time.Millisecond), d[(len(d)*95)/100].Round(time.Millisecond), d[len(d)-1].Round(time.Millisecond))
	}
}

// answerOfPaths resolves each of paths to "merged-<path>", by rules, so that it lands.
func answerOfPaths(paths ...string) resolver.Response {
	resp := resolver.Response{Version: resolver.ProtocolVersion, Resolver: "graft-resolve/bench"}
	for _, p := range paths {
		resp.Files = append(resp.Files, resolver.ResponseFile{Origin: resolver.OriginRule,
			Path: p, Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged-" + p + "\n"),
		})
	}
	return resp
}

// benchLaneSeconds is the time the lanes of the process were held so far: the sum of the phase
// total of every executed batch.
func benchLaneSeconds(t *testing.T) float64 {
	var m dto.Metric
	require.NoError(t, landBatchPhaseSeconds.WithLabelValues(landBatchPhaseTotal).(interface{ Write(*dto.Metric) error }).Write(&m))
	return m.GetHistogram().GetSampleSum()
}

func benchEnvInt(t *testing.T, name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	require.NoError(t, err, name)
	return n
}
