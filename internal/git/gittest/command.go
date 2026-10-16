package gittest

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// ExecConfig contains configuration for ExecOpts.
type ExecConfig struct {
	// Stdin sets up stdin of the spawned command.
	Stdin io.Reader
	// Stdout sets up stdout of the spawned command. Note that `ExecOpts()` will not return any
	// output anymore if this field is set.
	Stdout io.Writer
	// Stderr sets up stderr of the spawned command. If this field is not set, the error is
	// dumped to test logs.
	Stderr io.Writer
	// Env contains environment variables that should be appended to the spawned command's
	// environment.
	Env []string
	// ExpectedExitCode is used to check the resulting exit code of the command. This can be used in case a command
	// is expected to return an error code.
	ExpectedExitCode int
	// ReferenceBackend, when set, runs the command under a context carrying this
	// reference backend so the command factory selects the matching bundled Git
	// binary (ex: the MVCC backend).
	ReferenceBackend git.ReferenceBackend
}

// Exec runs a git command and returns the standard output, or fails.
func Exec(tb testing.TB, cfg config.Cfg, args ...string) []byte {
	tb.Helper()

	return ExecOpts(tb, cfg, ExecConfig{}, args...)
}

// ExecOpts runs a git command with the given configuration.
func ExecOpts(tb testing.TB, cfg config.Cfg, execCfg ExecConfig, args ...string) []byte {
	tb.Helper()

	// When the command targets a specific repository via "-C <path>" and no reference backend was
	// explicitly requested, detect the repository's backend from disk so the matching Git binary
	// (most importantly the MVCC binary) is selected. This mirrors how the MVCC middleware selects
	// the backend in production and lets helpers such as WriteCommit operate on MVCC repositories
	// transparently.
	if execCfg.ReferenceBackend.Name == "" {
		if repoPath, ok := repoPathFromArgs(args); ok {
			backend, err := gitcmd.DetectReferenceBackend(context.Background(), repoPath)
			if err == nil {
				execCfg.ReferenceBackend = backend
			}
		}
	}

	cmd := createCommand(tb, cfg, execCfg, args...)

	// If the caller has passed an stdout writer to us we cannot use `cmd.Output()`. So
	// we detect this case and call `cmd.Run()` instead.
	if execCfg.Stdout != nil {
		if err := cmd.Run(); err != nil {
			handleExecErr(tb, cfg, execCfg, args, err)
		}

		return nil
	}

	output, err := cmd.Output()
	if err != nil {
		handleExecErr(tb, cfg, execCfg, args, err)
	}

	return output
}

// repoPathFromArgs returns the repository path passed to Git via the "-C <path>" argument, if
// present. Nearly all gittest helpers scope their commands to a repository this way.
func repoPathFromArgs(args []string) (string, bool) {
	for i, arg := range args {
		if arg == "-C" && i+1 < len(args) {
			return args[i+1], true
		}
	}

	return "", false
}

func handleExecErr(tb testing.TB, cfg config.Cfg, execCfg ExecConfig, args []string, err error) {
	var stderr []byte
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if execCfg.ExpectedExitCode == exitErr.ExitCode() {
			return
		}
		stderr = exitErr.Stderr
	}
	tb.Log(args)
	if len(stderr) > 0 {
		tb.Logf("%s\n", stderr)
	}
	tb.Fatal(err)
}

// NewCommand creates a new Git command ready for execution.
func NewCommand(tb testing.TB, cfg config.Cfg, args ...string) *exec.Cmd {
	tb.Helper()
	return createCommand(tb, cfg, ExecConfig{}, args...)
}

func createCommand(tb testing.TB, cfg config.Cfg, execCfg ExecConfig, args ...string) *exec.Cmd {
	tb.Helper()

	ctx := testhelper.Context(tb)

	factory := NewCommandFactory(tb, cfg)

	if execCfg.ReferenceBackend.Name != "" {
		ctx = gitcmd.ContextWithReferenceBackend(ctx, execCfg.ReferenceBackend)
	}

	execEnv := factory.GetExecutionEnvironment(ctx)

	gitConfig, err := factory.GlobalConfiguration(ctx)
	require.NoError(tb, err)
	gitConfig = append(gitConfig,
		gitcmd.ConfigPair{Key: "init.defaultBranch", Value: git.DefaultBranch},
		gitcmd.ConfigPair{Key: "init.templateDir", Value: ""},
		gitcmd.ConfigPair{Key: "user.name", Value: "Your Name"},
		gitcmd.ConfigPair{Key: "user.email", Value: "you@example.com"},
	)

	cmd := exec.CommandContext(ctx, execEnv.BinaryPath, args...)
	cmd.Env = command.AllowedEnvironment(os.Environ())
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE=1572776879 +0100", "GIT_COMMITTER_DATE=1572776879 +0100")
	cmd.Env = append(cmd.Env, gitcmd.ConfigPairsToGitEnvironment(gitConfig)...)
	cmd.Env = append(cmd.Env, execEnv.EnvironmentVariables...)
	cmd.Env = append(cmd.Env, execCfg.Env...)

	// Here we need to read directly from environment variables
	// because the context is not propagated from the tests.
	if val := os.Getenv("GIT_MVCC_MANIFEST"); val != "" {
		cmd.Env = append(cmd.Env, "GIT_MVCC_MANIFEST="+val)
	}

	if val := os.Getenv("GIT_MVCC_MANIFEST_PATH"); val != "" {
		cmd.Env = append(cmd.Env, "GIT_MVCC_MANIFEST_PATH="+val)
	}

	cmd.Stdout = execCfg.Stdout
	cmd.Stdin = execCfg.Stdin
	cmd.Stderr = execCfg.Stderr

	return cmd
}

var _ gitcmd.RepositoryExecutor = RepositoryPathExecutor{}

// RepositoryPathExecutor is a `gitcmd.RepositoryExecutor` that knows to execute commands in a Git repository identified by
// its absolute path.
type RepositoryPathExecutor struct {
	repoPath string
	storage.Repository
	factory gitcmd.CommandFactory
}

// NewRepositoryPathExecutor creates a new RepositoryPathExecutor for the given repository.
func NewRepositoryPathExecutor(tb testing.TB, cfg config.Cfg, repoPath string) RepositoryPathExecutor {
	relativePath, err := filepath.Rel(cfg.Storages[0].Path, repoPath)
	require.NoError(tb, err)

	return RepositoryPathExecutor{
		repoPath: repoPath,
		Repository: &gitalypb.Repository{
			StorageName:  cfg.Storages[0].Name,
			RelativePath: relativePath,
		},
		factory: NewCommandFactory(tb, cfg),
	}
}

// Exec executes a command in the given repository.
func (e RepositoryPathExecutor) Exec(ctx context.Context, cmd gitcmd.Command, opts ...gitcmd.CmdOpt) (*command.Command, error) {
	// Detect the repository's reference backend from disk and carry it on the context, mirroring
	// how the MVCC middleware selects the backend in production. This ensures the command factory
	// picks the Git binary matching the repository (most importantly the MVCC binary) without any
	// global test switch.
	if backend, err := gitcmd.DetectReferenceBackend(ctx, e.repoPath); err == nil {
		ctx = gitcmd.ContextWithReferenceBackend(ctx, backend)
	}

	return e.factory.New(ctx, e.Repository, cmd, opts...)
}

// ExecAndWait executes a command in the given repository and waits for it to exit.
func (e RepositoryPathExecutor) ExecAndWait(ctx context.Context, cmd gitcmd.Command, opts ...gitcmd.CmdOpt) error {
	command, err := e.Exec(ctx, cmd, opts...)
	if err != nil {
		return err
	}

	return command.Wait()
}

// GitVersion determines the Git version used by the executor.
func (e RepositoryPathExecutor) GitVersion(ctx context.Context) (string, error) {
	return e.factory.GitVersion(ctx)
}

// ObjectHash determines the object hash used by the repository.
func (e RepositoryPathExecutor) ObjectHash(ctx context.Context) (git.ObjectHash, error) {
	return gitcmd.DetectObjectHash(ctx, e.repoPath)
}

// ReferenceBackend detects the reference backend used by this repository.
func (e RepositoryPathExecutor) ReferenceBackend(ctx context.Context) (git.ReferenceBackend, error) {
	return gitcmd.DetectReferenceBackend(ctx, e.repoPath)
}
