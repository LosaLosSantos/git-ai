package gitalybackup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/backup"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/grpc/metadata"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestCreateSubcommand(t *testing.T) {
	tests := []struct {
		Name               string
		Flags              func(backupRoot string) []string
		ServerOpts         func(ctx context.Context, backupRoot string) []testserver.GitalyServerOpt
		ExpectedErrMessage string
	}{
		{
			Name: "when a local backup is created",
			Flags: func(backupRoot string) []string {
				return []string{"--path", backupRoot, "--id", "the-new-backup"}
			},
			ServerOpts: func(ctx context.Context, backupRoot string) []testserver.GitalyServerOpt {
				return nil
			},
			ExpectedErrMessage: `create: pipeline: 1 failures encountered:\n - invalid: manager: could not dial source: invalid connection string: \"invalid\"\n`,
		},
		{
			Name: "when a server-side backup is created",
			Flags: func(path string) []string {
				return []string{"--server-side", "--id", "the-new-backup"}
			},
			ServerOpts: func(ctx context.Context, backupRoot string) []testserver.GitalyServerOpt {
				backupSink, err := backup.ResolveSink(ctx, backupRoot)
				require.NoError(t, err)

				backupLocator := backup.NewLocator(backupSink)

				return []testserver.GitalyServerOpt{
					testserver.WithBackupSink(backupSink),
					testserver.WithBackupLocator(backupLocator),
				}
			},
			ExpectedErrMessage: `create: pipeline: 1 failures encountered:\n - invalid: server-side create: could not dial source: invalid connection string: \"invalid\"\n`,
		},
		{
			Name: "when a server-side incremental backup is created",
			Flags: func(path string) []string {
				return []string{"--server-side", "--incremental", "--id", "the-new-backup"}
			},
			ServerOpts: func(ctx context.Context, backupRoot string) []testserver.GitalyServerOpt {
				backupSink, err := backup.ResolveSink(ctx, backupRoot)
				require.NoError(t, err)

				backupLocator := backup.NewLocator(backupSink)

				return []testserver.GitalyServerOpt{
					testserver.WithBackupSink(backupSink),
					testserver.WithBackupLocator(backupLocator),
				}
			},
			ExpectedErrMessage: `create: pipeline: 1 failures encountered:\n - invalid: server-side create: could not dial source: invalid connection string: \"invalid\"\n`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.Name, func(t *testing.T) {
			ctx := testhelper.Context(t)
			path := testhelper.TempDir(t)

			cfg := testcfg.Build(t)
			cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll, tc.ServerOpts(ctx, path)...)

			var repos []*gitalypb.Repository
			for i := 0; i < 5; i++ {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
				repos = append(repos, repo)
			}

			var stdin bytes.Buffer
			encoder := json.NewEncoder(&stdin)

			for _, repo := range repos {
				require.NoError(t, encoder.Encode(map[string]string{
					"address":         cfg.SocketPath,
					"token":           cfg.Auth.Token,
					"storage_name":    repo.GetStorageName(),
					"relative_path":   repo.GetRelativePath(),
					"gl_project_path": repo.GetGlProjectPath(),
				}))
			}

			// Partial failure scenario
			require.NoError(t, encoder.Encode(map[string]string{
				"address":       "invalid",
				"token":         "invalid",
				"relative_path": "invalid",
			}))

			// server-side WriteBackupID resolves the Gitaly server from the
			// context, so GITALY_SERVERS must be available.
			injectCtx, err := storage.InjectGitalyServers(ctx, cfg.Storages[0].Name, cfg.SocketPath, cfg.Auth.Token)
			require.NoError(t, err)
			t.Setenv("GITALY_SERVERS", metadata.GetValue(metadata.OutgoingToIncoming(injectCtx), "gitaly-servers"))

			args := append([]string{"create"}, tc.Flags(path)...)

			stdout, stderr, exitCode := runGitalyBackup(t, ctx, cfg, &stdin, args...)
			require.Empty(t, stderr)
			require.Contains(t, stdout, tc.ExpectedErrMessage)
			require.Equal(t, 1, exitCode)

			// The marker is written regardless of partial pipeline failures so
			// that successfully backed-up repos remain discoverable for restore.
			require.FileExists(t, filepath.Join(path, "backup_ids", "the-new-backup"))

			for _, repo := range repos {
				bundlePath := filepath.Join(path, repo.GetStorageName(), repo.GetRelativePath(), "the-new-backup", "001.bundle")
				require.FileExists(t, bundlePath)
			}
		})
	}
}

// TestCreateSubcommand_serverSideUnusedStorage covers GITALY_SERVERS listing a
// storage that no repository is backed up to, and whose server rejects
// storage-scoped RPCs for it. This happens when a placeholder `default` storage
// points at a Praefect that has no virtual storage of that name.
func TestCreateSubcommand_serverSideUnusedStorage(t *testing.T) {
	const placeholderStorage = "default"

	for _, tc := range []struct {
		desc string
		// withRepositoryServer sends the server of each repository on stdin and
		// leaves the backed-up storage out of GITALY_SERVERS. Then GITALY_SERVERS
		// holds only the placeholder storage, so the result doesn't depend on the
		// order in which the servers are picked.
		withRepositoryServer bool
		incremental          bool
	}{
		{
			desc:                 "GITALY_SERVERS lists only the unused storage",
			withRepositoryServer: true,
		},
		{
			desc: "GITALY_SERVERS lists the unused storage and the backed-up storage",
		},
		{
			desc:        "incremental backup with GITALY_SERVERS listing the unused storage and the backed-up storage",
			incremental: true,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := testhelper.Context(t)
			path := testhelper.TempDir(t)

			backupSink, err := backup.ResolveSink(ctx, path)
			require.NoError(t, err)

			cfg := testcfg.Build(t, testcfg.WithStorages("gitaly-cluster"))
			cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
				testserver.WithBackupSink(backupSink),
				testserver.WithBackupLocator(backup.NewLocator(backupSink)),
			)

			// With Praefect in front of Gitaly, the placeholder storage points at
			// Praefect, which has no virtual storage of that name. Without Praefect,
			// a stub server rejects the storage-scoped RPCs the same way.
			placeholderAddress := cfg.SocketPath
			if !testhelper.IsPraefectEnabled() {
				placeholderAddress = runRejectingBackupServer(t)
			}

			servers := storage.GitalyServers{
				placeholderStorage: {Address: placeholderAddress, Token: cfg.Auth.Token},
			}
			if !tc.withRepositoryServer {
				servers[cfg.Storages[0].Name] = storage.ServerInfo{Address: cfg.SocketPath, Token: cfg.Auth.Token}
			}
			serversJSON, err := json.Marshal(servers)
			require.NoError(t, err)
			t.Setenv("GITALY_SERVERS", base64.StdEncoding.EncodeToString(serversJSON))

			var repos []*gitalypb.Repository
			for i := 0; i < 3; i++ {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
				repos = append(repos, repo)
			}

			var stdin bytes.Buffer
			encoder := json.NewEncoder(&stdin)
			for _, repo := range repos {
				job := map[string]string{
					"storage_name":    repo.GetStorageName(),
					"relative_path":   repo.GetRelativePath(),
					"gl_project_path": repo.GetGlProjectPath(),
				}
				if tc.withRepositoryServer {
					job["address"] = cfg.SocketPath
					job["token"] = cfg.Auth.Token
				}
				require.NoError(t, encoder.Encode(job))
			}

			args := []string{"create", "--server-side", "--id", "the-new-backup"}
			if tc.incremental {
				args = append(args, "--incremental")
			}

			stdout, stderr, exitCode := runGitalyBackup(t, ctx, cfg, &stdin, args...)
			require.Empty(t, stderr)
			require.NotContains(t, stdout, "virtual storage does not exist")
			require.Equal(t, 0, exitCode, stdout)

			require.FileExists(t, filepath.Join(path, "backup_ids", "the-new-backup"))
			for _, repo := range repos {
				require.FileExists(t, filepath.Join(path, repo.GetStorageName(), repo.GetRelativePath(), "the-new-backup", "001.bundle"))
			}
		})
	}
}

// rejectingBackupServer answers the storage-scoped BackupService RPCs the way
// Praefect does for a virtual storage it doesn't have.
type rejectingBackupServer struct {
	gitalypb.UnimplementedBackupServiceServer
}

func (rejectingBackupServer) WriteBackupID(context.Context, *gitalypb.WriteBackupIDRequest) (*gitalypb.WriteBackupIDResponse, error) {
	return nil, structerr.NewInvalidArgument("virtual storage does not exist")
}

func (rejectingBackupServer) ReadLatestBackupID(context.Context, *gitalypb.ReadLatestBackupIDRequest) (*gitalypb.ReadLatestBackupIDResponse, error) {
	return nil, structerr.NewInvalidArgument("virtual storage does not exist")
}

func runRejectingBackupServer(tb testing.TB) string {
	socketPath := testhelper.GetTemporaryGitalySocketFileName(tb)
	lc := net.ListenConfig{}
	listener, err := lc.Listen(testhelper.Context(tb), "unix", socketPath)
	require.NoError(tb, err)

	srv := grpc.NewServer()
	gitalypb.RegisterBackupServiceServer(srv, rejectingBackupServer{})
	tb.Cleanup(srv.Stop)
	go func() { assert.NoError(tb, srv.Serve(listener)) }()

	return "unix://" + socketPath
}
