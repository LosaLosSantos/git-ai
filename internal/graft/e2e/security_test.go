package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
)

// TestAgentCannotCreateRepositoryFromSnapshot checks that an agent cannot
// create a repository from a snapshot. The snapshot is a tar archive that
// Gitaly downloads from a URL the client chooses and extracts into the new
// repository unfiltered, so it could plant a custom pre-receive hook that runs
// as the Gitaly user on the next write. The RPC must be rejected before the
// archive is downloaded, and no repository may be created.
func TestAgentCannotCreateRepositoryFromSnapshot(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	hook := []byte("#!/bin/sh\necho executed-as-gitaly > " + marker + "\n")
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "custom_hooks/", Typeflag: tar.TypeDir, Mode: 0o755}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "custom_hooks/pre-receive", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(hook))}))
	_, err := tw.Write(hook)
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		_, _ = w.Write(archive.Bytes())
	}))
	t.Cleanup(srv.Close)

	const relPath = "evil/agent-made.git"
	repo := &gitalypb.Repository{StorageName: s.storage, RelativePath: relPath}

	builderRepos := gitalypb.NewRepositoryServiceClient(s.dialAgent(t, exampleTokens["builder"]))
	_, err = builderRepos.CreateRepositoryFromSnapshot(s.ctx, &gitalypb.CreateRepositoryFromSnapshotRequest{
		Repository: repo,
		HttpUrl:    srv.URL,
	})
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "got %v", err)
	require.Zero(t, downloads.Load(), "the snapshot must not be downloaded")
	require.NoDirExists(t, filepath.Join(s.cfg.Storages[0].Path, relPath))
	require.NoFileExists(t, marker)
}

// TestAgentCannotMakeServerDialAddress checks that an agent cannot make the
// server connect to an address it chooses. UserCommitFiles with a
// start_repository on another storage dials the address that the
// gitaly-servers metadata names for that storage and fetches from it. An agent
// that sends the metadata must be rejected before anything is dialed, and
// without the metadata the foreign start repository fails without a dial.
func TestAgentCannotMakeServerDialAddress(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	writeMain(t, s, map[string]string{"README.md": "ssrf\n"})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	var accepts atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_ = conn.Close()
		}
	}()

	builder := gitalypb.NewOperationServiceClient(s.dialAgent(t, exampleTokens["builder"]))
	commitFiles := func(ctx context.Context) error {
		stream, err := builder.UserCommitFiles(ctx)
		require.NoError(t, err)

		_ = stream.Send(&gitalypb.UserCommitFilesRequest{
			UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{
				Header: &gitalypb.UserCommitFilesRequestHeader{
					Repository:      &gitalypb.Repository{StorageName: s.storage, RelativePath: s.relPath},
					User:            &gitalypb.User{GlId: "builder", GlUsername: "builder", Name: []byte("builder"), Email: []byte("builder@example.com")},
					BranchName:      []byte("builder/ssrf"),
					CommitMessage:   []byte("ssrf"),
					StartBranchName: []byte("main"),
					StartRepository: &gitalypb.Repository{StorageName: "evil", RelativePath: "x.git"},
				},
			},
		})
		_, err = stream.CloseAndRecv()
		return err
	}

	evilCtx, err := storage.InjectGitalyServers(s.ctx, "evil", "tcp://"+listener.Addr().String(), "attacker-token")
	require.NoError(t, err)

	err = commitFiles(evilCtx)
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "got %v", err)
	require.ErrorContains(t, err, "gitaly-servers")
	require.Zero(t, accepts.Load(), "the server must not dial the address of the agent")

	err = commitFiles(s.ctx)
	require.Error(t, err, "a foreign start repository cannot be resolved without gitaly-servers")
	require.Zero(t, accepts.Load(), "the server must not dial the address of the agent")

	require.Empty(t, s.git(t, "for-each-ref", "refs/heads/builder/ssrf"), "no branch may be created")
}
