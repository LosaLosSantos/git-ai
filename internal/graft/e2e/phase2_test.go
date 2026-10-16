package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/verify"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// The phase-2 end-to-end tests run against the example configuration, like
// the phase-1 tests in this package. They cover the resolve protocol with the
// real graft-resolve binary, the post-merge verifier with auto-revert, the
// superfan monitor with a webhook, and the agent RPC allowlist.

// sharedLogPath is the file the landing scenarios conflict on. It sits at the
// top level because writeMain builds a flat tree.
const sharedLogPath = "shared-log.txt"

// sharedLogHeader is the first line of sharedLogPath in the scenarios.
const sharedLogHeader = "# shared log\n"

// TestPhase2AgentAllowlist checks the agent RPC allowlist of WP15 from the
// outside: an agent may call an allowed accessor and is denied a mutator
// that is not on the allowlist.
func TestPhase2AgentAllowlist(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	planner := s.dialAgent(t, exampleTokens["planner"])

	// ServerInfo is an accessor that agents may use.
	info, err := gitalypb.NewServerServiceClient(planner).ServerInfo(s.ctx, &gitalypb.ServerInfoRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, info.GetServerVersion())

	// DeleteRefs is a ref mutator outside the allowlist.
	_, err = gitalypb.NewRefServiceClient(planner).DeleteRefs(s.ctx, &gitalypb.DeleteRefsRequest{
		Repository: &gitalypb.Repository{StorageName: s.storage, RelativePath: s.relPath},
		Refs:       [][]byte{[]byte("refs/heads/anything")},
	})
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "got %v", err)
}

// sharedChange commits on top of base a change by who that writes its own
// file and, if shared is true, overwrites sharedLogPath with a line of its
// own. Two shared changes from the same base conflict when landed in turn.
func sharedChange(ctx context.Context, repo *sdk.Repo, who, base string, seq int, shared bool) (string, error) {
	actions := []sdk.Action{sdk.Put{Path: fmt.Sprintf("agents/%s/%d.txt", who, seq), Content: []byte("change\n")}}
	if shared {
		actions = append(actions, sdk.Put{
			Path:      sharedLogPath,
			Content:   []byte(fmt.Sprintf("%sline of %s %d\n", sharedLogHeader, who, seq)),
			Overwrite: true,
		})
	}

	res, err := repo.CommitFiles(ctx, sdk.CommitOptions{
		Branch:   fmt.Sprintf("feature/%s-%d", who, seq),
		StartRev: base,
		Message:  fmt.Sprintf("change %s %d", who, seq),
		Author:   sdk.Signature{Name: who, Email: who + "@example.com"},
		User:     sdk.User{ID: who, Name: who, Email: who + "@example.com"},
		Actions:  actions,
	})
	if err != nil {
		return "", err
	}

	return res.CommitID, nil
}

// landOn lands commit on branch with rebase, with or without conflict
// resolution, as the planner.
func landOn(ctx context.Context, repo *sdk.Repo, branch, commit string, resolve bool) (*sdk.LandResult, error) {
	return repo.Land(ctx, sdk.LandOptions{
		Branch:           branch,
		Commit:           commit,
		User:             sdk.User{Name: "planner", Email: "planner@example.com"},
		Strategy:         sdk.LandRebase,
		ResolveConflicts: resolve,
	})
}

// TestPhase2LandingScenarioConflictsWithoutResolver is the server-side
// behavior the graft-bench land and land-resolve modes measure. Four shared
// changes and one private change, all made from the same base, are landed
// with rebase. Only the first shared change lands; the others fail with a
// merge conflict. With ResolveConflicts set but the resolver disabled (the
// example configuration), the outcome is the same and nothing is resolved.
func TestPhase2LandingScenarioConflictsWithoutResolver(t *testing.T) {
	t.Parallel()

	for _, resolve := range []bool{false, true} {
		t.Run(fmt.Sprintf("resolve=%t", resolve), func(t *testing.T) {
			t.Parallel()

			s := startExample(t)
			repo := s.repoAs(t, exampleTokens["planner"])
			ctx := s.ctx

			base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
			branch := fmt.Sprintf("bench-resolve-%t", resolve)
			_, err := landAs(ctx, repo, "planner", branch, base)
			require.NoError(t, err)

			var commits []string
			for seq := 1; seq <= 4; seq++ {
				commit, err := sharedChange(ctx, repo, "agent", base, seq, true)
				require.NoError(t, err)
				commits = append(commits, commit)
			}
			private, err := sharedChange(ctx, repo, "agent", base, 5, false)
			require.NoError(t, err)

			res, err := landOn(ctx, repo, branch, private, resolve)
			require.NoError(t, err, "a change on other paths always lands")
			require.False(t, res.AlreadyLanded)

			var landed, conflicts int
			for _, commit := range commits {
				res, err := landOn(ctx, repo, branch, commit, resolve)
				switch {
				case err == nil:
					landed++
					require.Empty(t, res.ResolvedPaths)
				case sdk.IsMergeConflict(err):
					conflicts++
				default:
					require.FailNow(t, "unexpected land error", "%v", err)
				}
			}

			require.Equal(t, 1, landed, "only the first shared change lands")
			require.Equal(t, 3, conflicts, "the other shared changes conflict")

			// The landed branch holds the private change and one shared line.
			files := s.git(t, "ls-tree", "-r", "--name-only", branch)
			require.Contains(t, files, "agents/agent/5.txt")
			shared := s.git(t, "show", branch+":"+sharedLogPath)
			require.True(t, strings.HasPrefix(shared, strings.TrimSpace(sharedLogHeader)))
			require.Len(t, strings.Split(shared, "\n"), 2, "header plus exactly one landed line: %q", shared)
		})
	}
}

// TestPhase2ConcurrentPrivateLandsSucceed checks that concurrent lands of
// non-conflicting changes all succeed on one branch, which the benchmark's
// land mode relies on.
func TestPhase2ConcurrentPrivateLandsSucceed(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	const branch = "bench-concurrent"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)

	const changes = 8
	commits := make([]string, changes)
	for i := range commits {
		commits[i], err = sharedChange(ctx, repo, fmt.Sprintf("agent%d", i), base, 1, false)
		require.NoError(t, err)
	}

	errs := make(chan error, changes)
	for _, commit := range commits {
		go func(commit string) {
			_, err := landOn(ctx, repo, branch, commit, false)
			errs <- err
		}(commit)
	}
	for range commits {
		require.NoError(t, <-errs)
	}

	files := s.git(t, "ls-tree", "-r", "--name-only", branch, "agents")
	require.Len(t, strings.Split(files, "\n"), changes)
}

// buildAgentsTool builds a command of the tools/graft-agents module into a
// temporary directory. The module has its own go.mod, so the server never
// imports it.
func buildAgentsTool(t *testing.T, pkg string) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), filepath.Base(pkg))
	// GOPROXY=off keeps the build from the network: the module of the tools
	// needs its dependencies in the module cache already.
	cmd := exec.Command("go", "-C", "../../../tools/graft-agents", "build", "-o", bin, pkg)
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build %s (run go -C tools/graft-agents mod download first if the module cache is cold): %s", pkg, out)
	return bin
}

// writeResolveConfig writes the graft-resolve configuration of the tests: the
// trivial strategy, then the union strategy for sharedLogPath.
func writeResolveConfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "resolve.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[[strategy]]
type = "trivial"

[[strategy]]
type = "union"
paths = ["shared-log.txt"]
`), 0o600))
	return path
}

// TestPhase2ResolveConflictThroughGraftResolve lands four changes that
// conflict in the same file. The server asks the real graft-resolve binary,
// configured with the trivial and union strategies, to resolve each
// conflict, and the landed file contains every side.
func TestPhase2ResolveConflictThroughGraftResolve(t *testing.T) {
	t.Parallel()

	ex := loadExample(t)
	ex.graft.Land.Resolver = config.GraftResolver{
		Enabled: true,
		Command: []string{buildAgentsTool(t, "./cmd/graft-resolve"), "--config", writeResolveConfig(t)},
	}
	s := startGitaly(t, ex.graft, ex.sharedToken)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	const branch = "resolve-union"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)

	var resolved int
	for seq := 1; seq <= 4; seq++ {
		commit, err := sharedChange(ctx, repo, "agent", base, seq, true)
		require.NoError(t, err)

		res, err := landOn(ctx, repo, branch, commit, true)
		require.NoError(t, err, "change %d must land with the resolver", seq)
		if len(res.ResolvedPaths) > 0 {
			require.Equal(t, []string{sharedLogPath}, res.ResolvedPaths)
			require.Equal(t, "graft-resolve/union", res.Resolver)
			resolved += len(res.ResolvedPaths)
		}
	}
	require.Equal(t, 3, resolved, "the first change lands clean, the other three are resolved")

	shared := s.git(t, "show", branch+":"+sharedLogPath)
	require.Equal(t, 5, len(strings.Split(shared, "\n")), "header plus four lines: %q", shared)
	for seq := 1; seq <= 4; seq++ {
		require.Contains(t, shared, fmt.Sprintf("line of agent %d", seq))
	}

	tip := s.git(t, "log", "-1", "--format=%B", branch)
	require.Contains(t, tip, "Graft-Resolved-By: graft-resolve/union")
	require.Contains(t, tip, "Graft-Resolved-Path: "+sharedLogPath)
}

// TestPhase2ResolverRejectsDeniedPath checks that a conflict in a path the
// resolver policy denies is not resolved and the land fails as a merge
// conflict, although the resolver is enabled and the strategies could solve
// the conflict.
func TestPhase2ResolverRejectsDeniedPath(t *testing.T) {
	t.Parallel()

	ex := loadExample(t)
	ex.graft.Land.Resolver = config.GraftResolver{
		Enabled:   true,
		Command:   []string{buildAgentsTool(t, "./cmd/graft-resolve"), "--config", writeResolveConfig(t)},
		PathsDeny: []string{sharedLogPath},
	}
	s := startGitaly(t, ex.graft, ex.sharedToken)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	const branch = "resolve-denied"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)

	first, err := sharedChange(ctx, repo, "agent", base, 1, true)
	require.NoError(t, err)
	_, err = landOn(ctx, repo, branch, first, true)
	require.NoError(t, err)

	second, err := sharedChange(ctx, repo, "agent", base, 2, true)
	require.NoError(t, err)
	_, err = landOn(ctx, repo, branch, second, true)
	require.True(t, sdk.IsMergeConflict(err), "a denied path is not resolved: %v", err)

	shared := s.git(t, "show", branch+":"+sharedLogPath)
	require.NotContains(t, shared, "line of agent 2", "the denied change must not land")
}

// findCommit returns the ID of the first commit reachable from rev whose
// subject is subject.
func findCommit(t *testing.T, s gitalyStack, rev, subject string) string {
	t.Helper()

	for _, line := range strings.Split(s.git(t, "log", "--format=%H %s", rev), "\n") {
		if oid, s2, ok := strings.Cut(line, " "); ok && s2 == subject {
			return oid
		}
	}
	require.FailNow(t, "no commit with subject", "%q on %s", subject, rev)
	return ""
}

// TestPhase2VerifyDetectsAndReverts runs the verifier once against a branch
// whose newest commit fails the test command. It bisects to the culprit,
// lands an auto-revert on the branch, and the next run is green. The verifier
// is the library behind graft-verify --once: verify.Open wires the bisector
// that the binary uses.
func TestPhase2VerifyDetectsAndReverts(t *testing.T) {
	t.Parallel()

	ex := loadExample(t)
	s := startGitaly(t, ex.graft, ex.sharedToken)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{"README.md": "base\n"})
	const branch = "verified"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)

	cfg := verify.Config{
		Address:    s.addr,
		Token:      ex.sharedToken,
		Storage:    s.storage,
		Repository: s.relPath,
		Branch:     branch,
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		StateFile:  filepath.Join(t.TempDir(), "state.json"),
		Test: verify.TestConfig{
			// The tree fails as soon as a file named broken exists.
			Command: []string{"sh", "-c", "test ! -e broken"},
		},
		Revert: verify.RevertConfig{Enabled: true, MaxPerHour: 3},
	}
	require.NoError(t, cfg.Finalize())

	v, closer, err := verify.Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = closer.Close() })

	// The first run tests the branch as created and makes it green.
	out, ran, err := v.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.True(t, out.Passed, out.Output)

	for _, change := range []struct{ topic, path, content string }{
		{"good", "good.txt", "fine\n"},
		{"broken", "broken", "x\n"},
		{"d", "d.txt", "four\n"},
	} {
		commit, err := commitFile(ctx, repo, "planner", "topic/"+change.topic, base, change.path, change.content)
		require.NoError(t, err)
		_, err = landOn(ctx, repo, branch, commit, false)
		require.NoError(t, err)
	}

	failed, ran, err := v.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.False(t, failed.Passed, "the newest tip fails the test")
	require.Empty(t, failed.BisectError)

	culprit := findCommit(t, s, branch, "add broken")

	// The revert is a child of the failing tip, with the trailer that names
	// the culprit.
	require.Equal(t, `Revert "add broken"`, s.git(t, "log", "-1", "--format=%s", branch))
	require.Contains(t, s.git(t, "log", "-1", "--format=%B", branch), "Graft-Auto-Revert: "+culprit)
	require.Equal(t, failed.Tip, s.git(t, "rev-parse", branch+"^"))
	require.NotContains(t, strings.Split(s.git(t, "ls-tree", "--name-only", branch), "\n"), "broken")

	// The reverted branch passes again.
	out, ran, err = v.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.True(t, out.Passed, out.Output)
}

// buildGraftCLI builds cmd/graft of the main module into a temporary directory.
func buildGraftCLI(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "graft")
	out, err := exec.Command("go", "-C", "../../..", "build", "-o", bin, "./cmd/graft").CombinedOutput()
	require.NoError(t, err, "build graft: %s", out)
	return bin
}

// runGraft runs the graft CLI with env and returns its standard output.
func runGraft(t *testing.T, bin string, env []string, args ...string) string {
	t.Helper()

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "graft %s: %s", strings.Join(args, " "), stderr.String())
	return stdout.String()
}

// watchJSONLines runs graft watch --json in dir and returns the first n lines
// it prints, then stops it. Watch follows the log and does not end by itself.
func watchJSONLines(t *testing.T, bin string, env []string, dir string, n int) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "watch", "-C", dir, "--json")
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	var lines []string
	scanner := bufio.NewScanner(stdout)
	for len(lines) < n && scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	require.Len(t, lines, n, "graft watch --json printed fewer lines: %s", stderr.String())
	return lines
}

// TestPhase2SuperfanPostsToWebhook runs the pipeline of doc/graft/SUPERFAN.md
// with the real graft binary: graft log --json is the event source, graft show
// --json gives the details of each commit, and both read a workspace cloned
// from the server. Two changes land under services/api and one under docs, so
// the digests map real paths to the components of the configuration. The
// webhook receives one digest per component.
//
// The graft commands run with the shared operator token, as the verifier
// does: a monitor is an operator tool, not an agent.
func TestPhase2SuperfanPostsToWebhook(t *testing.T) {
	t.Parallel()

	ex := loadExample(t)
	s := startGitaly(t, ex.graft, ex.sharedToken)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	const branch = "superfan"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)
	for _, path := range []string{"services/api/server.go", "services/api/client.go", "docs/guide.md"} {
		commit, err := commitFile(ctx, repo, "planner", "topic/"+filepath.Base(path), base, path, "x\n")
		require.NoError(t, err)
		_, err = landOn(ctx, repo, branch, commit, false)
		require.NoError(t, err)
	}
	changesets := allChangesets(ctx, t, repo)
	require.NotEmpty(t, changesets)

	graft := buildGraftCLI(t)
	env := append(os.Environ(),
		"GRAFT_ADDRESS="+s.addr,
		"GRAFT_TOKEN="+ex.sharedToken,
		"GRAFT_STORAGE="+s.storage,
	)
	workspace := filepath.Join(t.TempDir(), "workspace")
	runGraft(t, graft, env, "clone", s.addr, s.relPath, workspace, "--branch", branch)

	// graft watch --json and graft log --json print the same lines, one per
	// changeset.
	logLines := strings.Split(strings.TrimSpace(runGraft(t, graft, env, "log", "-C", workspace, "--json")), "\n")
	require.Len(t, logLines, len(changesets), "graft log --json lists every changeset")
	require.Equal(t, logLines, watchJSONLines(t, graft, env, workspace, len(logLines)))

	var mu sync.Mutex
	var posts [][]byte
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts = append(posts, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(webhook.Close)

	superfanConfig, err := json.Marshal(map[string]any{
		"window":     "1h",
		"references": []string{"refs/heads/" + branch},
		"components": []map[string]any{
			{"name": "api", "paths": []string{"services/api/**"}},
			{"name": "platform", "paths": []string{"**"}},
		},
		"routes":    []map[string]any{{"component": "*", "notifiers": []string{"ops"}}},
		"notifiers": []map[string]any{{"name": "ops", "type": "webhook", "url": webhook.URL}},
		// Post the digest even when the window has no findings.
		"digest": map[string]any{"include_quiet": true},
	})
	require.NoError(t, err)
	configPath := filepath.Join(t.TempDir(), "superfan.json")
	require.NoError(t, os.WriteFile(configPath, superfanConfig, 0o600))

	// The superfan reads the log of the server and fetches the details of each
	// commit. The commands are split on spaces, so the paths must not contain any.
	require.NotContains(t, graft+workspace, " ")
	cmd := exec.CommandContext(ctx, buildAgentsTool(t, "./cmd/graft-superfan"),
		"--config", configPath,
		"--watch-cmd", graft+" log -C "+workspace+" --json --after {after}",
		"--details-cmd", graft+" show -C "+workspace+" --json {oid}",
		"--tick", "0")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "graft-superfan: %s", out)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, posts, 2, "one digest for each of the two components, and no unassigned digest")

	events := map[string]int{}
	for _, body := range posts {
		var digest struct {
			Kind      string `json:"kind"`
			Component string `json:"component"`
			Events    int    `json:"events"`
		}
		require.NoError(t, json.Unmarshal(body, &digest))
		require.Equal(t, notifyKindDigest, digest.Kind)
		events[digest.Component] = digest.Events
	}
	// The references restrict the superfan to the branch. The api component
	// (services/api/**) matches the two API changes only, while the platform
	// component (**) matches every changeset of the branch, the creation of
	// the branch included. The topic branches are not counted.
	var onBranch int
	for _, c := range changesets {
		if string(c.GetReference()) == "refs/heads/"+branch {
			onBranch++
		}
	}
	require.Equal(t, map[string]int{"api": 2, "platform": onBranch}, events)
}

// notifyKindDigest is the kind of the digest messages of tools/graft-agents/notify.
const notifyKindDigest = "digest"
