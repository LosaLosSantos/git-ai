package operations

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// untrustedMigrations returns graft_quarantine_untrusted_migrations_total.
func untrustedMigrations(t *testing.T) float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == "graft_quarantine_untrusted_migrations_total" {
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}

// commitAFile writes one file on branch with UserCommitFiles.
func commitAFile(t *testing.T, ctx context.Context, client gitalypb.OperationServiceClient, repo *gitalypb.Repository, branch, path, content string) {
	t.Helper()

	stream, err := client.UserCommitFiles(ctx)
	require.NoError(t, err)
	header := headerRequest(repo, gittest.TestUser, branch, []byte("commit message"), "", "")
	setAuthorAndEmail(header, []byte("Author Name"), []byte("author.email@example.com"))
	require.NoError(t, stream.Send(header))
	require.NoError(t, stream.Send(createFileHeaderRequest(path)))
	require.NoError(t, stream.Send(actionContentRequest(content)))
	_, err = stream.CloseAndRecv()
	require.NoError(t, err)
}

// writeLooseObjectHook writes a custom pre-receive hook that writes a loose object into the
// quarantine without git: it copies a zlib-compressed object, so no git configuration applies to the
// write. It returns the ID of the object.
func writeLooseObjectHook(t *testing.T, cfg config.Cfg, repoPath string) string {
	t.Helper()

	content := "written by a custom hook\n"
	oid := strings.TrimSpace(string(gittest.ExecOpts(t, cfg, gittest.ExecConfig{Stdin: strings.NewReader(content)},
		"-C", repoPath, "hash-object", "--stdin")))

	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, err := fmt.Fprintf(zw, "blob %d\x00%s", len(content), content)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	objectFile := filepath.Join(testhelper.TempDir(t), "object")
	require.NoError(t, os.WriteFile(objectFile, compressed.Bytes(), 0o644))

	gittest.WriteCustomHook(t, repoPath, "pre-receive", []byte(fmt.Sprintf(`#!/bin/sh
cat >/dev/null
mkdir -p "$GIT_OBJECT_DIRECTORY/%[1]s" || exit 1
cp %[3]q "$GIT_OBJECT_DIRECTORY/%[1]s/%[2]s" || exit 1
`, oid[:2], oid[2:], objectFile)))

	return oid
}

// TestUserCommitFiles_migrationOfAnUntrustedWriter is not parallel: it reads a global counter. A
// custom pre-receive hook writes a loose object into the quarantine of UserCommitFiles without git,
// so neither the object nor its directory is durable. The migration of that quarantine fsyncs its
// files, and the object ends up in the repository.
func TestUserCommitFiles_migrationOfAnUntrustedWriter(t *testing.T) {
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, func(t *testing.T, ctx context.Context) {
		ctx, cfg, client := setupOperationsService(t, ctx)
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		oid := writeLooseObjectHook(t, cfg, repoPath)

		before := untrustedMigrations(t)
		commitAFile(t, ctx, client, repo, "untrusted", "file", "content\n")
		require.InDelta(t, 1, untrustedMigrations(t)-before, 0, "the migration fsyncs the files of the quarantine")

		gittest.Exec(t, cfg, "-C", repoPath, "cat-file", "-e", oid)
	})
}

// TestUserCommitFiles_migrationWithoutAnUntrustedWriter is not parallel: it reads a global counter.
// Without a custom hook, the migration trusts the writers of the quarantine.
func TestUserCommitFiles_migrationWithoutAnUntrustedWriter(t *testing.T) {
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, func(t *testing.T, ctx context.Context) {
		ctx, cfg, client := setupOperationsService(t, ctx)
		repo, _ := gittest.CreateRepository(t, ctx, cfg)

		before := untrustedMigrations(t)
		commitAFile(t, ctx, client, repo, "trusted", "file", "content\n")
		require.InDelta(t, 0, untrustedMigrations(t)-before, 0, "the migration does not fsync the files of the quarantine")
	})
}
