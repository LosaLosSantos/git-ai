package gitcmd_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// bundledGit runs the bundled git with args and returns its trimmed output.
func bundledGit(t *testing.T, cfg config.Cfg, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(gittest.Exec(t, cfg, args...)))
}

// TestRepositoryObjectHashMatchesGit checks that the object hash read from the files of a repository
// is the object format git -C <path> rev-parse --show-object-format reports, for the paths a command
// can run in: a bare repository, the root of a main worktree, the root of a linked worktree of a
// non-bare and of a bare repository, and a repository without a configuration file.
func TestRepositoryObjectHashMatchesGit(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)

	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			dir := testhelper.TempDir(t)

			bare := filepath.Join(dir, "bare.git")
			bundledGit(t, cfg, "init", "--quiet", "--bare", "--object-format="+format, bare)

			main := filepath.Join(dir, "main")
			bundledGit(t, cfg, "init", "--quiet", "--object-format="+format, main)
			bundledGit(t, cfg, "-C", main, "commit", "--quiet", "--allow-empty", "-m", "first")

			// A linked worktree: its .git is a file naming its git directory, whose commondir file
			// names the main git directory, which holds the configuration.
			linked := filepath.Join(dir, "linked")
			bundledGit(t, cfg, "-C", main, "worktree", "add", "--quiet", "--detach", linked, "HEAD")
			require.FileExists(t, filepath.Join(linked, ".git"))

			// The same with a bare repository as the main git directory.
			bundledGit(t, cfg, "-C", main, "push", "--quiet", bare, "HEAD:refs/heads/main")
			bareLinked := filepath.Join(dir, "bare-linked")
			bundledGit(t, cfg, "-C", bare, "worktree", "add", "--quiet", "--detach", bareLinked, "main")

			for name, path := range map[string]string{
				"bare repository":      bare,
				"main worktree":        main,
				"linked worktree":      linked,
				"worktree of the bare": bareLinked,
			} {
				t.Run(name, func(t *testing.T) {
					want := bundledGit(t, cfg, "-C", path, "rev-parse", "--show-object-format")
					require.Equal(t, format, want)

					got, err := gitcmd.RepositoryObjectHash(path)
					require.NoError(t, err)
					require.Equal(t, want, got.Format)

					common, err := gitcmd.CommonGitDir(path)
					require.NoError(t, err)
					wantCommon := bundledGit(t, cfg, "-C", path, "rev-parse", "--path-format=absolute", "--git-common-dir")
					require.Equal(t, filepath.Clean(wantCommon), filepath.Clean(common))
				})
			}
		})
	}

	t.Run("repository without a configuration file", func(t *testing.T) {
		t.Parallel()

		repo := filepath.Join(testhelper.TempDir(t), "repo.git")
		bundledGit(t, cfg, "init", "--quiet", "--bare", repo)
		require.NoError(t, os.Remove(filepath.Join(repo, "config")))

		require.Equal(t, git.ObjectHashSHA1.Format, bundledGit(t, cfg, "-C", repo, "rev-parse", "--show-object-format"))
		got, err := gitcmd.RepositoryObjectHash(repo)
		require.NoError(t, err)
		require.Equal(t, git.ObjectHashSHA1.Format, got.Format)
		require.Equal(t, git.ObjectHashSHA1.EmptyTreeOID, got.EmptyTreeOID)
	})
}

func TestRepositoryObjectHashErrors(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)

	t.Run("invalid .git file", func(t *testing.T) {
		t.Parallel()

		worktree := testhelper.TempDir(t)
		require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("not a gitdir line\n"), 0o600))

		_, err := gitcmd.RepositoryObjectHash(worktree)
		require.ErrorContains(t, err, "invalid .git file")
	})

	t.Run("unreadable configuration", func(t *testing.T) {
		t.Parallel()

		repo := filepath.Join(testhelper.TempDir(t), "repo.git")
		bundledGit(t, cfg, "init", "--quiet", "--bare", repo)
		configPath := filepath.Join(repo, "config")
		require.NoError(t, os.Chmod(configPath, 0o000))
		t.Cleanup(func() { require.NoError(t, os.Chmod(configPath, 0o600)) })

		_, err := gitcmd.RepositoryObjectHash(repo)
		require.Error(t, err)
	})

	t.Run("unknown object format", func(t *testing.T) {
		t.Parallel()

		repo := filepath.Join(testhelper.TempDir(t), "repo.git")
		bundledGit(t, cfg, "init", "--quiet", "--bare", repo)
		require.NoError(t, os.WriteFile(filepath.Join(repo, "config"),
			[]byte("[core]\n\trepositoryformatversion = 1\n[extensions]\n\tobjectformat = sha512\n"), 0o600))

		_, err := gitcmd.RepositoryObjectHash(repo)
		require.Error(t, err)
	})
}

// TestExecCommandFactory_attrTreeErrorFailsTheCommand: a command that needs attr.tree = the empty
// tree fails when the object format of its repository cannot be read, instead of running without
// attr.tree.
func TestExecCommandFactory_attrTreeErrorFailsTheCommand(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})

	configPath := filepath.Join(repoPath, "config")
	require.NoError(t, os.Chmod(configPath, 0o000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(configPath, 0o600)) })

	cf := gittest.NewCommandFactory(t, cfg)
	_, err := cf.New(ctx, repo, gitcmd.Command{Name: "rev-list", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--all"}}})
	require.ErrorContains(t, err, "detecting the object format for attr.tree")
}

// TestExecCommandFactory_attrTreeStartsNoProcess: a command outside the attr.tree whitelist runs one
// process, its own: the object format is read from the repository's files.
func TestExecCommandFactory_attrTreeStartsNoProcess(t *testing.T) {
	t.Parallel()

	ctx := log.InitContextCustomFields(testhelper.Context(t))
	cfg := testcfg.Build(t)
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

	cf := gittest.NewCommandFactory(t, cfg)
	run := func(repo storage.Repository) {
		cmd, err := cf.New(ctx, repo, gitcmd.Command{Name: "rev-list", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--all"}}})
		require.NoError(t, err)
		require.NoError(t, cmd.Wait())
	}

	// The first command also detects the version of git once.
	run(repo)
	before, _ := log.CustomFieldsFromContext(ctx).Fields()["command.count"].(int)
	run(repo)
	after, _ := log.CustomFieldsFromContext(ctx).Fields()["command.count"].(int)
	require.Equal(t, 1, after-before)
}
