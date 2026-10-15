package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service/setup"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

// missingOID is a well-formed object ID that does not exist in test repositories.
const missingOID = "0123456789abcdef0123456789abcdef01234567"

// serverEnv is a real Gitaly server with every service registered and one
// empty repository on it.
type serverEnv struct {
	ctx      context.Context
	cfg      config.Cfg
	addr     string
	storage  string
	relPath  string
	repoPath string
}

func startServer(t *testing.T, configure ...func(*config.Cfg)) serverEnv {
	t.Helper()

	// The test context marks feature flags as explicit, so every flag a server
	// checks must be set. GPGSigning is checked by UserCommitFiles.
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	// The CLI targets standalone servers, which fill in the acting user of writes
	// from the request identity.
	cfg.Graft.Standalone = true
	for _, c := range configure {
		c(&cfg)
	}
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll)
	cfg.SocketPath = addr

	// Only the setup call carries gitaly-servers metadata. The CLI calls may
	// authenticate as agents, which may not send it.
	setupCtx := testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))
	repoProto, repoPath := gittest.CreateRepository(t, setupCtx, cfg)

	return serverEnv{
		ctx:      ctx,
		cfg:      cfg,
		addr:     addr,
		storage:  cfg.Storages[0].Name,
		relPath:  repoProto.GetRelativePath(),
		repoPath: repoPath,
	}
}

// backend connects a new backend to the test repository.
func (e serverEnv) backend(t *testing.T) *sdkBackend {
	t.Helper()

	b, err := openSDKBackend(e.ctx, target{Address: e.addr, Storage: e.storage, Repo: e.relPath})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// seedMain creates a root commit with exactly files on branch main.
func (e serverEnv) seedMain(t *testing.T, files map[string]string) git.ObjectID {
	t.Helper()

	return gittest.WriteCommit(t, e.cfg, e.repoPath,
		gittest.WithTree(e.writeTree(t, files)),
		gittest.WithBranch("main"),
	)
}

// writeTree writes the nested tree that holds exactly files, keyed by their
// slash-separated paths.
func (e serverEnv) writeTree(t *testing.T, files map[string]string) git.ObjectID {
	t.Helper()

	blobs := map[string]string{}
	dirs := map[string]map[string]string{}
	for path, content := range files {
		name, rest, nested := strings.Cut(path, "/")
		if !nested {
			blobs[name] = content
			continue
		}
		if dirs[name] == nil {
			dirs[name] = map[string]string{}
		}
		dirs[name][rest] = content
	}

	entries := make([]gittest.TreeEntry, 0, len(blobs)+len(dirs))
	for name, content := range blobs {
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: name, Content: content})
	}
	for name, sub := range dirs {
		entries = append(entries, gittest.TreeEntry{Mode: "040000", Path: name, OID: e.writeTree(t, sub)})
	}

	return gittest.WriteTree(t, e.cfg, e.repoPath, entries)
}
