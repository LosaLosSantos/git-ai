package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

// TestCLICommitReplacesDirectoryWithFile commits a tracked directory that was
// replaced by a regular file. The server reports such a path as "not a file"
// rather than missing, and creates the file only after the directory's files
// are deleted.
func TestCLICommitReplacesDirectoryWithFile(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"dir/f.txt": "f\n", "dir/sub/g.txt": "g\n", "top.txt": "top\n"})
	b := env.backend(t)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	require.NoError(t, os.RemoveAll(filepath.Join(dir, "dir")))
	writeLocal(t, dir, "dir", "now a file\n")

	code, stdout, _ := runCLI(t, "status", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Equal(t, "A dir\nD dir/f.txt\nD dir/sub/g.txt\n", stdout)

	code, stdout, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "dir becomes a file", "-land", "main")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "and landed it on main")

	require.Equal(t, "now a file\n", readRemoteFile(t, env, b, "main", "dir"))
	_, err := b.ReadFile(env.ctx, "main", "dir/f.txt")
	require.ErrorIs(t, err, workspace.ErrNotFound)
	require.Equal(t, "top\n", readRemoteFile(t, env, b, "main", "top.txt"))

	code, stdout, _ = runCLI(t, "status", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Equal(t, "nothing changed\n", stdout)
}

func TestSDKBackendCommitReplacesFileWithDirectory(t *testing.T) {
	t.Parallel()

	env := startServer(t)
	env.seedMain(t, map[string]string{"a": "file\n", "top.txt": "top\n"})
	b := env.backend(t)

	dir, ws := cloneForTest(t, env, b)
	require.NoError(t, os.Remove(filepath.Join(dir, "a")))
	writeLocal(t, dir, "a/b.txt", "nested\n")

	_, err := ws.Commit(env.ctx, workspace.CommitRequest{Message: "a becomes a directory", Author: testAuthor(), LandBranch: "main"})
	require.NoError(t, err)
	require.Equal(t, "nested\n", readRemoteFile(t, env, b, "main", "a/b.txt"))

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
}

func TestSDKBackendRefusesFileReplacingDirectoryWithFilesOutsideSparseSet(t *testing.T) {
	t.Parallel()

	env := startServer(t)
	env.seedMain(t, map[string]string{"dir/a/x.txt": "x\n", "dir/b/y.txt": "y\n"})
	b := env.backend(t)

	dir, ws := cloneForTest(t, env, b, "dir/a")
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "dir")))
	writeLocal(t, dir, "dir", "now a file\n")

	_, err := ws.Commit(env.ctx, workspace.CommitRequest{Message: "m", Author: testAuthor(), LandBranch: "main"})
	require.ErrorIs(t, err, workspace.ErrAlreadyExists)
	require.ErrorContains(t, err, "dir is a directory on the server that holds files outside the sparse set (dir/b/y.txt)")
	require.Nil(t, ws.Summary().Pending)
	require.Equal(t, "y\n", readRemoteFile(t, env, b, "main", "dir/b/y.txt"))
}

// TestCLICloneWithLowServerObjectLimit clones a repository with more files than
// the server allows object IDs per GetObjects request.
func TestCLICloneWithLowServerObjectLimit(t *testing.T) {
	env := startServer(t, func(cfg *config.Cfg) {
		cfg.Graft.ObjectService.MaxObjects = 100
	})
	files := map[string]string{}
	for i := range 150 {
		files[fmt.Sprintf("f%03d.txt", i)] = fmt.Sprintf("file %d\n", i)
	}
	env.seedMain(t, files)

	dir := filepath.Join(t.TempDir(), "ws")
	code, stdout, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "150 files")
	require.Equal(t, "file 149\n", readLocalFile(t, dir, "f149.txt"))

	code, stdout, _ = runCLI(t, "status", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Equal(t, "nothing changed\n", stdout)
}
