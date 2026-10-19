package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/helper/duration"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// The tests of the LOCAL REFERENCE WRITES RULE (doc/graft/PLAN7.md) with real
// binaries: a `gitaly serve` process in authoritative mode is killed with
// SIGKILL at the points of the rule and started again on the same storage,
// database and bucket.

// realNode is a gitaly server process of one node.
type realNode struct {
	// cfg is the configuration the process reads; clientCfg is the same with the
	// socket as a dialable address.
	cfg       config.Cfg
	clientCfg config.Cfg
	bin       string
	cfgPath   string
	logPath   string
	tokens    map[string]string
	ctx       context.Context
	setupCtx  context.Context

	cmd *exec.Cmd
	// exited is closed when the process has exited and cmd.Wait returned.
	exited chan struct{}
}

func newRealNode(t *testing.T, configure ...func(*config.GraftStorage)) *realNode {
	t.Helper()

	graft, tokens := workerGraft(1)
	cfg := testcfg.Build(t)
	cfg.Auth.Token = sharedTestToken
	cfg.Graft = unbatched(graft)
	cfg.Graft.Standalone = true
	cfg.Graft.Storage = config.GraftStorage{
		Mode:     "authoritative",
		Database: coretest.DatabaseURL(t, sqlstore.SQLite),
		Bucket:   coretest.BucketURL(t, false),
		NodeID:   "node-a",
	}
	for _, c := range configure {
		c(&cfg.Graft.Storage)
	}
	cfg.SocketPath = testhelper.GetTemporaryGitalySocketFileName(t)
	cfg.PrometheusListenAddr = freeAddr(t)
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	clientCfg := cfg
	clientCfg.SocketPath = "unix://" + cfg.SocketPath
	// The requests to a separate server process carry no feature flags of the
	// test binary, as a production client's do not: testhelper.Context marks
	// them as explicit, and the server would refuse a flag of its own that the
	// test binary does not define.
	base, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctx := featureflag.OutgoingCtxWithFeatureFlag(base, featureflag.GPGSigning, false)

	return &realNode{
		cfg:       cfg,
		clientCfg: clientCfg,
		bin:       testcfg.BuildGitaly(t, cfg),
		cfgPath:   testcfg.WriteTemporaryGitalyConfigFile(t, cfg),
		logPath:   filepath.Join(t.TempDir(), "gitaly.log"),
		tokens:    tokens,
		ctx:       ctx,
		setupCtx:  testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, clientCfg)),
	}
}

// sibling returns a second server of the same storage, database and bucket on
// its own socket, as the new process of a graceful upgrade is.
func (n *realNode) sibling(t *testing.T) *realNode {
	t.Helper()

	s := *n
	s.cfg.SocketPath = testhelper.GetTemporaryGitalySocketFileName(t)
	s.cfg.PrometheusListenAddr = freeAddr(t)
	s.clientCfg = s.cfg
	s.clientCfg.SocketPath = "unix://" + s.cfg.SocketPath
	s.cfgPath = testcfg.WriteTemporaryGitalyConfigFile(t, s.cfg)
	s.logPath = filepath.Join(t.TempDir(), "gitaly.log")
	s.setupCtx = testhelper.MergeOutgoingMetadata(s.ctx, testcfg.GitalyServersMetadataFromCfg(t, s.clientCfg))
	s.cmd, s.exited = nil, nil
	return &s
}

// start starts the server process with the extra environment env and waits
// until it serves.
func (n *realNode) start(t *testing.T, env ...string) {
	t.Helper()

	logFile, err := os.OpenFile(n.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = logFile.Close() })

	cmd := exec.Command(n.bin, "serve", n.cfgPath)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	require.NoError(t, cmd.Start())

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	n.cmd, n.exited = cmd, exited
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			_ = cmd.Process.Signal(syscall.SIGKILL)
			<-exited
		}
	})

	conn := gittest.DialService(t, n.ctx, n.clientCfg)
	ready, cancel := context.WithTimeout(n.ctx, 2*time.Minute)
	defer cancel()
	// Any answer (Unimplemented included) means the server serves.
	_, err = healthpb.NewHealthClient(conn).Check(ready, &healthpb.HealthCheckRequest{}, grpc.WaitForReady(true))
	if ready.Err() != nil {
		t.Fatalf("the server did not start: %v\n%s", err, n.log(t))
	}
}

// kill kills the server process with SIGKILL and waits for its end.
func (n *realNode) kill(t *testing.T) {
	t.Helper()
	require.NoError(t, n.cmd.Process.Signal(syscall.SIGKILL))
	<-n.exited
}

// waitExit waits for the server process to exit by itself and returns its exit
// code.
func (n *realNode) waitExit(t *testing.T) int {
	t.Helper()

	select {
	case <-n.exited:
	case <-time.After(2 * time.Minute):
		t.Fatalf("the server did not exit\n%s", n.log(t))
	}
	return n.cmd.ProcessState.ExitCode()
}

func (n *realNode) log(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile(n.logPath)
	require.NoError(t, err)
	return string(content)
}

func (n *realNode) storagePath() string { return n.cfg.Storages[0].Path }

// git runs git in the node's local copy of the repository, reading the disk
// directly.
func (n *realNode) git(t *testing.T, relPath string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(gittest.Exec(t, n.cfg, append([]string{"-C", n.repoPath(relPath)}, args...)...)))
}

// freeAddr returns a local TCP address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, addr := testhelper.GetLocalhostListener(t)
	require.NoError(t, l.Close())
	return addr
}

// counter returns the value of the counter name of the server's metrics.
func (n *realNode) counter(t *testing.T, name string) float64 {
	t.Helper()

	resp, err := http.Get("http://" + n.cfg.PrometheusListenAddr + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	for _, line := range strings.Split(string(body), "\n") {
		if value, ok := strings.CutPrefix(line, name+" "); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			require.NoError(t, err)
			return v
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

// createRepository creates an empty repository through the server and returns
// its relative path. In authoritative mode the gate registers it.
func (n *realNode) createRepository(t *testing.T) string {
	t.Helper()
	repo, _ := gittest.CreateRepository(t, n.setupCtx, n.clientCfg)
	return repo.GetRelativePath()
}

func (n *realNode) repoPath(relPath string) string {
	return filepath.Join(n.storagePath(), relPath)
}

// agent returns a handle whose requests carry the worker's token.
func (n *realNode) agent(t *testing.T, relPath string) *sdk.Repo {
	t.Helper()

	conn, err := sdk.Dial(n.ctx, n.clientCfg.SocketPath, sdk.WithStorage(n.cfg.Storages[0].Name), sdk.WithAgentToken(n.tokens[workerName(0)]))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn.Repo(relPath)
}

// operator returns a handle whose requests carry the shared token.
func (n *realNode) operator(t *testing.T, relPath string) *sdk.Repo {
	t.Helper()

	conn, err := sdk.Dial(n.ctx, n.clientCfg.SocketPath, sdk.WithStorage(n.cfg.Storages[0].Name), sdk.WithToken(sharedTestToken))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn.Repo(relPath)
}

// markers returns the dirty markers of the node's storage.
func (n *realNode) markers(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(n.storagePath(), localrefs.MarkerDirName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, filepath.Join(n.storagePath(), localrefs.MarkerDirName, entry.Name()))
		}
	}
	return names
}

// tables returns the tables of the stack of the node's local repository.
func (n *realNode) tables(t *testing.T, relPath string) []string {
	t.Helper()
	list, err := os.ReadFile(filepath.Join(n.repoPath(relPath), "reftable", "tables.list"))
	require.NoError(t, err)
	return strings.Fields(string(list))
}

// requireServed checks, through the server, that every branch of want is
// served with its commit and that the node's local copy equals the database at
// its head.
func (n *realNode) requireServed(t *testing.T, relPath string, want map[string]string) {
	t.Helper()

	onA := n.agent(t, relPath)
	for branch, commit := range want {
		got, err := onA.ResolveRevision(n.ctx, branch)
		require.NoError(t, err, "branch %s", branch)
		require.Equal(t, commit, got, "branch %s", branch)
	}

	verified, err := n.operator(t, relPath).StorageVerify(n.ctx, true)
	require.NoError(t, err)
	require.Empty(t, verified.Divergences, "local references must equal the database references")
	require.True(t, verified.ConnectivityOK, "connectivity: %s", verified.ConnectivityError)
}

// writeBranches commits one file on each of count new branches and returns
// the commits by branch.
func writeBranches(t *testing.T, onA *sdk.Repo, ctx context.Context, base string, prefix string, count int) map[string]string {
	t.Helper()

	written := map[string]string{}
	for i := 0; i < count; i++ {
		branch := fmt.Sprintf("%s/%d", prefix, i)
		commit, err := commitFile(ctx, onA, workerName(0), branch, base, branch+".txt", branch+"\n")
		require.NoError(t, err)
		written[branch] = commit
	}
	return written
}

// TestStorageLocalReferencesAfterAProcessKill kills the server right after
// reference writes made without fsync, before the durable step: the kernel
// kept every write, so the restarted node keeps its repository, serves every
// write and settles the marker.
func TestStorageLocalReferencesAfterAProcessKill(t *testing.T) {
	t.Parallel()

	n := newRealNode(t)
	n.start(t)
	rel := n.createRepository(t)
	onA := n.agent(t, rel)
	base, err := commitFile(n.ctx, onA, workerName(0), "main", "", "README.md", "base\n")
	require.NoError(t, err)
	written := writeBranches(t, onA, n.ctx, base, "before-kill", 5)
	written["main"] = base

	// The last write is less than the idle period old: the marker is there.
	require.Len(t, n.markers(t), 1)
	n.kill(t)
	require.Len(t, n.markers(t), 1, "the marker survives the process")

	n.start(t)
	n.requireServed(t, rel, written)
	require.DirExists(t, n.repoPath(rel), "a process restart keeps the repository")

	// The durable step runs every second and removes the marker of an idle
	// repository.
	require.Eventually(t, func() bool { return len(n.markers(t)) == 0 }, 90*time.Second, 50*time.Millisecond)
	n.requireServed(t, rel, written)
}

// TestStorageLocalReferencesAfterAKillInTheDurableStep kills the server while
// its durable step holds git's reference lock: the restarted node removes the
// lock it left, so writes go on at once, and serves every write.
func TestStorageLocalReferencesAfterAKillInTheDurableStep(t *testing.T) {
	t.Parallel()

	n := newRealNode(t)
	n.start(t)
	rel := n.createRepository(t)
	onA := n.agent(t, rel)
	base, err := commitFile(n.ctx, onA, workerName(0), "main", "", "README.md", "base\n")
	require.NoError(t, err)
	written := writeBranches(t, onA, n.ctx, base, "before-crash", 3)
	written["main"] = base
	require.Len(t, n.markers(t), 1)
	n.kill(t)

	// Restarted with the crash point, the node finds the repository dirty and
	// idle: its first durable step dies holding the lock.
	n.start(t, localrefs.CrashEnv+"=durable_step")
	require.Equal(t, 75, n.waitExit(t), "the process dies in the durable step")
	lock := filepath.Join(n.repoPath(rel), "reftable", "tables.list.lock")
	require.FileExists(t, lock)
	require.Len(t, n.markers(t), 1, "the marker is removed only after the lock")

	n.start(t)
	require.NoFileExists(t, lock, "the restarted node removes the lock its durable step left")
	n.requireServed(t, rel, written)

	more := writeBranches(t, n.agent(t, rel), n.ctx, base, "after-restart", 2)
	for branch, commit := range more {
		written[branch] = commit
	}
	n.requireServed(t, rel, written)
}

// TestStorageLocalReferencesAfterAKernelRestart simulates what a kernel
// restart (power loss) can leave of reference writes made without fsync: the
// newest table of the stack is empty. Read directly, the repository then looks
// empty, without an error. The restarted node moves it aside before serving
// and hydrates it again, so every write is served.
func TestStorageLocalReferencesAfterAKernelRestart(t *testing.T) {
	t.Parallel()

	n := newRealNode(t)
	n.start(t)
	rel := n.createRepository(t)
	onA := n.agent(t, rel)
	base, err := commitFile(n.ctx, onA, workerName(0), "main", "", "README.md", "base\n")
	require.NoError(t, err)
	written := writeBranches(t, onA, n.ctx, base, "before-power-loss", 5)
	written["main"] = base

	markers := n.markers(t)
	require.Len(t, markers, 1)
	n.kill(t)

	// The power loss: the newest table never reached the disk, and the kernel
	// that boots next has another boot identity.
	tables := n.tables(t, rel)
	require.NoError(t, os.Truncate(filepath.Join(n.repoPath(rel), "reftable", tables[len(tables)-1]), 0))
	content, err := os.ReadFile(markers[0])
	require.NoError(t, err)
	var m map[string]string
	require.NoError(t, json.Unmarshal(content, &m))
	m["boot_id"] = "the-boot-before-the-power-loss"
	content, err = json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(markers[0], content, 0o600))

	// What a server without the rule would serve.
	torn := gittest.Exec(t, n.cfg, "-C", n.repoPath(rel), "for-each-ref", "--format=%(refname)")
	require.Empty(t, strings.TrimSpace(string(torn)), "git reads the torn stack as an empty repository")

	n.start(t)
	require.Empty(t, n.markers(t))
	n.requireServed(t, rel, written)
	require.Contains(t, n.log(t), "moved aside a local repository whose references may have lost writes")
}

// TestStorageLocalReferencesDuringAGracefulUpgrade runs a second server on
// the same storage while the first one lives, as a graceful upgrade does: the
// first one owns the deferred durability, the second writes with fsync and
// leaves the markers alone. When the first one dies, the second owns, settles
// and defers. Every write is served throughout.
func TestStorageLocalReferencesDuringAGracefulUpgrade(t *testing.T) {
	t.Parallel()

	old := newRealNode(t)
	old.start(t)
	rel := old.createRepository(t)
	base, err := commitFile(old.ctx, old.agent(t, rel), workerName(0), "main", "", "README.md", "base\n")
	require.NoError(t, err)
	written := writeBranches(t, old.agent(t, rel), old.ctx, base, "old", 3)
	written["main"] = base
	require.Len(t, old.markers(t), 1)

	next := old.sibling(t)
	next.start(t)
	require.Contains(t, next.log(t), "another gitaly process owns the deferred durability")
	for branch, commit := range writeBranches(t, next.agent(t, rel), next.ctx, base, "during", 3) {
		written[branch] = commit
	}
	next.requireServed(t, rel, written)
	require.Equal(t, float64(0), next.counter(t, "graft_storage_ref_durability_owner"))
	require.Equal(t, float64(0), next.counter(t, "graft_storage_ref_dirty_marked_total"), "the new process writes with fsync")

	// The old process owns the marker and settles it once the repository is
	// idle; the new process's writes were made with fsync and need none.
	require.Eventually(t, func() bool { return len(old.markers(t)) == 0 }, 30*time.Second, 50*time.Millisecond)

	old.kill(t)
	require.Eventually(t, func() bool {
		return strings.Contains(next.log(t), "this process owns the deferred durability of the storages now")
	}, 30*time.Second, 50*time.Millisecond)

	for branch, commit := range writeBranches(t, next.agent(t, rel), next.ctx, base, "after", 2) {
		written[branch] = commit
	}
	require.Equal(t, float64(1), next.counter(t, "graft_storage_ref_durability_owner"))
	require.GreaterOrEqual(t, next.counter(t, "graft_storage_ref_dirty_marked_total"), float64(1), "the new owner writes without fsync")
	next.requireServed(t, rel, written)
}

// TestStorageBackgroundMaintenanceCheckpoints runs the real server with the
// background maintenance on (doc/graft/PLAN7.md, "Background maintenance"): a
// repository that gets enough changesets is checkpointed without a request,
// and keeps serving every write.
func TestStorageBackgroundMaintenanceCheckpoints(t *testing.T) {
	t.Parallel()

	changesets := uint64(2)
	n := newRealNode(t, func(s *config.GraftStorage) {
		s.Maintenance = config.GraftStorageMaintenanceAuto
		s.MaintenanceInterval = "1s"
		s.CheckpointChangesets = &changesets
	})
	n.start(t)
	require.Contains(t, n.log(t), "background maintenance enabled")

	rel := n.createRepository(t)
	onA := n.agent(t, rel)
	base, err := commitFile(n.ctx, onA, workerName(0), "main", "", "README.md", "base\n")
	require.NoError(t, err)
	written := writeBranches(t, onA, n.ctx, base, "maintained", 3)
	written["main"] = base

	require.Eventually(t, func() bool { return strings.Contains(n.log(t), "storage core: checkpoint created") },
		60*time.Second, 100*time.Millisecond, "the maintenance checkpoints the repository")
	n.requireServed(t, rel, written)
}

// TestStorageHousekeepingBackfillsReviewRoots runs the daily housekeeping of
// the real server in authoritative mode (doc/graft/PLAN7.md, REVIEW ROOTS RULE,
// the backstop of the daily worker): a review written without the keep-around
// references of its commits (a review from before the rule) gets them, in the
// database and on the node, through the ordinary write path.
func TestStorageHousekeepingBackfillsReviewRoots(t *testing.T) {
	t.Parallel()

	// The daily worker starts at a minute of the clock: the next one that the
	// server is sure to see as future.
	start := time.Now().Add(time.Minute)
	if start.Second() > 40 {
		start = start.Add(time.Minute)
	}
	n := newRealNode(t)
	n.cfg.DailyMaintenance = config.DailyJob{
		Hour:     uint(start.Hour()),
		Minute:   uint(start.Minute()),
		Duration: duration.Duration(5 * time.Minute),
		Storages: []string{n.cfg.Storages[0].Name},
	}
	n.cfgPath = testcfg.WriteTemporaryGitalyConfigFile(t, n.cfg)
	n.start(t)

	rel := n.createRepository(t)
	onA := n.agent(t, rel)
	base, err := commitFile(n.ctx, onA, workerName(0), "main", "", "README.md", "base\n")
	require.NoError(t, err)
	head, err := commitFile(n.ctx, onA, workerName(0), "topic", base, "topic.txt", "topic\n")
	require.NoError(t, err)
	state, err := n.operator(t, rel).StorageState(n.ctx)
	require.NoError(t, err)

	// A review from before the rule: recorded without its roots.
	meta, err := sqlstore.Open(n.ctx, n.cfg.Graft.Storage.Database)
	require.NoError(t, err)
	t.Cleanup(func() { _ = meta.Close() })
	_, err = meta.InsertReview(n.ctx, state.RepositoryID, func(id string) (*gitalypb.Review, error) {
		return &gitalypb.Review{Id: id, TargetBranch: []byte("main"), HeadCommitId: head, BaseCommitId: base, Title: []byte("before the rule")}, nil
	}, false)
	require.NoError(t, err)

	roots := []string{corestore.KeepAroundRef(head), corestore.KeepAroundRef(base)}
	require.Eventually(t, func() bool {
		refs, _, err := meta.Refs(n.ctx, state.RepositoryID)
		require.NoError(t, err)
		for _, root := range roots {
			if refs[root].Absent() {
				return false
			}
		}
		return true
	}, 4*time.Minute, time.Second, "the daily housekeeping backfills the roots")
	require.GreaterOrEqual(t, n.counter(t, "graft_review_roots_backfilled_total"), float64(len(roots)))
	for _, root := range roots {
		require.NotEmpty(t, n.git(t, rel, "rev-parse", "--verify", root), "the node has %s", root)
	}
	n.requireServed(t, rel, map[string]string{"main": base, "topic": head})
}

// TestStorageLocalReferencesStackDepthIsBounded writes many references
// without fsync from concurrent writers and checks that git's compaction keeps
// the stack shallow: the durable step only makes it durable, compaction stays
// git's.
func TestStorageLocalReferencesStackDepthIsBounded(t *testing.T) {
	t.Parallel()

	const writers, perWriter = 4, 30
	n := newRealNode(t)
	n.start(t)
	rel := n.createRepository(t)
	onA := n.agent(t, rel)
	base, err := commitFile(n.ctx, onA, workerName(0), "main", "", "README.md", "base\n")
	require.NoError(t, err)

	var (
		mu       sync.Mutex
		maxDepth int
		wg       sync.WaitGroup
	)
	errs := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				branch := fmt.Sprintf("load/%d/%d", w, i)
				if _, err := commitFile(n.ctx, onA, workerName(0), branch, base, branch+".txt", branch+"\n"); err != nil {
					errs <- err
					return
				}
				list, err := os.ReadFile(filepath.Join(n.repoPath(rel), "reftable", "tables.list"))
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				maxDepth = max(maxDepth, len(strings.Fields(string(list))))
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	// git compacts the stack geometrically (factor 2): about log2 of the number
	// of updates. Concurrent writers can make a compaction lose its race and
	// leave a few more tables for a while.
	updates := writers*perWriter + 2
	bound := 2*int(math.Ceil(math.Log2(float64(updates)))) + 2
	t.Logf("maximum stack depth %d over %d reference updates (bound %d)", maxDepth, updates, bound)
	require.LessOrEqual(t, maxDepth, bound)
}
