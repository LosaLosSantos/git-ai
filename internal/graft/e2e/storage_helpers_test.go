package e2e

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// The phase-6 end-to-end tests (doc/graft/PLAN6.md, "Proof and docs") run
// several real Gitaly servers in one process. Every server is a node of the
// storage core: it has its own storage directory, and all nodes of a cluster
// share one database and one bucket, as separate machines would. The tests run
// on SQLite and, when GRAFT_TEST_POSTGRES_DSN is set, on Postgres.

// forEachStorageDialect runs fn as a parallel subtest for every database
// dialect of the storage core tests.
func forEachStorageDialect(t *testing.T, fn func(t *testing.T, dialect sqlstore.Dialect)) {
	t.Helper()

	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			t.Parallel()
			fn(t, dialect)
		})
	}
}

// storageCluster is a set of nodes that share one database and one bucket.
type storageCluster struct {
	dialect     sqlstore.Dialect
	mode        corestore.Mode
	graft       config.Graft
	databaseURL string
	bucketURL   string
	// logger, when set, is the logger of every node, so that a test can read
	// what the storage core logged.
	logger log.Logger
	// shadowTimeout, when set, is the shadow budget of every node instead of
	// the default.
	shadowTimeout time.Duration
	// adjust, when set, may change each node's core before its server starts
	// (wrap its database or bucket); the core is never changed afterwards, while
	// the server reads it.
	adjust func(*corestore.Core)
	nodes  []storageNode
}

// newStorageCluster returns a cluster without nodes, with a new database of
// dialect and a new file bucket. Every node runs graft in standalone mode.
func newStorageCluster(t *testing.T, dialect sqlstore.Dialect, mode corestore.Mode, graft config.Graft) *storageCluster {
	t.Helper()

	return &storageCluster{
		dialect:     dialect,
		mode:        mode,
		graft:       graft,
		databaseURL: coretest.DatabaseURL(t, dialect),
		bucketURL:   coretest.BucketURL(t, false),
	}
}

// storageNode is one Gitaly server of a storage cluster.
type storageNode struct {
	name string
	// ctx is the context of agent calls; setupCtx also carries the
	// gitaly-servers metadata that the setup helpers need.
	ctx      context.Context
	setupCtx context.Context
	cfg      config.Cfg
	core     *corestore.Core
}

// addNode starts a new node with an empty storage directory: node-a, node-b,
// and so on, in the order they are added.
func (c *storageCluster) addNode(t *testing.T) storageNode {
	t.Helper()

	name := fmt.Sprintf("node-%c", 'a'+len(c.nodes))
	core := coretest.Open(t, coretest.Options{
		Mode:          c.mode,
		Dialect:       c.dialect,
		DatabaseURL:   c.databaseURL,
		BucketURL:     c.bucketURL,
		NodeID:        name,
		ShadowTimeout: c.shadowTimeout,
	})
	if c.adjust != nil {
		c.adjust(core)
	}

	cfg := testcfg.Build(t)
	cfg.Auth.Token = sharedTestToken
	cfg.Graft = c.graft
	cfg.Graft.Standalone = true
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	opts := []testserver.GitalyServerOpt{
		testserver.WithGraftStorage(core),
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))),
	}
	if c.logger != nil {
		opts = append(opts, testserver.WithLogger(c.logger))
	}
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll, opts...)

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	node := storageNode{
		name:     name,
		ctx:      ctx,
		setupCtx: testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg)),
		cfg:      cfg,
		core:     core,
	}
	c.nodes = append(c.nodes, node)
	return node
}

// createRepository creates an empty repository through the node's
// RepositoryService. In authoritative mode the gate registers it in the
// database.
func (n storageNode) createRepository(t *testing.T) *gitalypb.Repository {
	t.Helper()

	repo, _ := gittest.CreateRepository(t, n.setupCtx, n.cfg)
	return repo
}

// repository returns the repository at relPath on the node's storage.
func (n storageNode) repository(relPath string) *gitalypb.Repository {
	return &gitalypb.Repository{StorageName: n.cfg.Storages[0].Name, RelativePath: relPath}
}

// repoPath returns the local directory of the repository on the node.
func (n storageNode) repoPath(relPath string) string {
	return filepath.Join(n.cfg.Storages[0].Path, relPath)
}

// handle returns an SDK handle to the repository on the node.
func (n storageNode) handle(t *testing.T, relPath string, auth sdk.DialOption) *sdk.Repo {
	t.Helper()

	conn, err := sdk.Dial(n.ctx, n.cfg.SocketPath, sdk.WithStorage(n.cfg.Storages[0].Name), auth)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn.Repo(relPath)
}

// agent returns a handle whose requests carry an agent token.
func (n storageNode) agent(t *testing.T, relPath, token string) *sdk.Repo {
	t.Helper()
	return n.handle(t, relPath, sdk.WithAgentToken(token))
}

// operator returns a handle whose requests carry the shared token, as the
// graft storage commands of an operator do.
func (n storageNode) operator(t *testing.T, relPath string) *sdk.Repo {
	t.Helper()
	return n.handle(t, relPath, sdk.WithToken(sharedTestToken))
}

// conn returns a raw connection with the shared token, for RPCs that the SDK
// does not wrap.
func (n storageNode) conn(t *testing.T) *grpc.ClientConn {
	t.Helper()
	return gittest.DialService(t, n.ctx, n.cfg)
}

// git runs git in the node's local copy of the repository. It reads the disk
// directly, so it shows what the node has without catching up first.
func (n storageNode) git(t *testing.T, relPath string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(gittest.Exec(t, n.cfg, append([]string{"-C", n.repoPath(relPath)}, args...)...)))
}

// localRefs returns the references of the node's local copy of the
// repository, read from disk without catching up.
func (n storageNode) localRefs(t *testing.T, relPath string) map[string]corestore.Target {
	t.Helper()

	refs, err := gitops.ReadRefs(n.ctx, localrepo.NewTestRepo(t, n.cfg, n.repository(relPath)))
	require.NoError(t, err)
	return refs
}

// record returns the database row of the repository.
func (n storageNode) record(t *testing.T, relPath string) corestore.Repository {
	t.Helper()

	rec, err := n.core.Meta.GetRepository(n.ctx, corestore.KeyOf(n.repository(relPath)))
	require.NoError(t, err)
	return rec
}

// dbRefs returns the references the database has for the repository.
func (n storageNode) dbRefs(t *testing.T, relPath string) map[string]corestore.Target {
	t.Helper()

	refs, _, err := n.core.Meta.Refs(n.ctx, n.record(t, relPath).ID)
	require.NoError(t, err)
	return refs
}

// requireConsistent checks, through the operator's view (graft storage
// verify and state), that the node's local copy has exactly the database's
// references at the head sequence and that its objects are connected.
func (n storageNode) requireConsistent(t *testing.T, relPath string) {
	t.Helper()

	op := n.operator(t, relPath)
	verified, err := op.StorageVerify(n.ctx, true)
	require.NoError(t, err)
	require.Empty(t, verified.Divergences, "%s: local references must equal the database references", n.name)
	require.True(t, verified.ConnectivityOK, "%s: connectivity: %s", n.name, verified.ConnectivityError)

	state, err := op.StorageState(n.ctx)
	require.NoError(t, err)
	require.True(t, state.Tracked)
	require.Equal(t, n.name, state.NodeID)
	require.Equal(t, verified.HeadSequence, state.HeadSequence, "%s: no write happened meanwhile", n.name)
	require.Equal(t, state.HeadSequence, state.AppliedSequence, "%s: the watermark is at the head", n.name)

	require.Equal(t, n.dbRefs(t, relPath), n.localRefs(t, relPath), "%s: references on disk", n.name)
}

// injectFault makes the voter fail once at point, as if the process had died
// there (doc/graft/PLAN6.md, voter "Faults for tests"). The hook is a package
// variable shared by every server of the process, so only tests that do not
// run in parallel with other storage tests may use it. The returned counter
// tells how often the fault fired.
func injectFault(t *testing.T, point string) *atomic.Int32 {
	t.Helper()

	var fired atomic.Int32
	voter.FaultHook = func(p string) error {
		if p != point || !fired.CompareAndSwap(0, 1) {
			return nil
		}
		return errors.New("injected crash at " + p)
	}
	t.Cleanup(func() { voter.FaultHook = nil })

	return &fired
}

// bucketPacks returns the keys of the packs in a file bucket.
func bucketPacks(t *testing.T, bucketURL string) map[string]bool {
	t.Helper()

	root := strings.TrimPrefix(bucketURL, "file://")
	keys := map[string]bool{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".pack") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		keys[filepath.ToSlash(rel)] = true
		return nil
	}))

	return keys
}

// referencedPacks returns the pack keys of every changeset of the repository.
func (n storageNode) referencedPacks(t *testing.T, relPath string) map[string]bool {
	t.Helper()

	rec := n.record(t, relPath)
	changesets, err := n.core.Meta.ListChangesets(n.ctx, rec.ID, 0, rec.HeadSeq)
	require.NoError(t, err)

	keys := map[string]bool{}
	for _, cs := range changesets {
		if cs.PackKey != "" {
			keys[cs.PackKey] = true
		}
	}
	return keys
}
