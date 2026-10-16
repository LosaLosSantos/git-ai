package gitcmd_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func TestDistributedGitEnvironmentConstructor(t *testing.T) {
	constructor := gitcmd.DistributedGitEnvironmentConstructor{}

	testhelper.Unsetenv(t, "GITALY_TESTING_GIT_BINARY")

	t.Run("empty configuration fails", func(t *testing.T) {
		_, err := constructor.Construct(config.Cfg{})
		require.Equal(t, gitcmd.ErrNotConfigured, err)
	})

	t.Run("empty configuration with environment override", func(t *testing.T) {
		t.Setenv("GITALY_TESTING_GIT_BINARY", "/foo/bar")

		execEnv, err := constructor.Construct(config.Cfg{})
		require.NoError(t, err)
		defer func() { require.NoError(t, execEnv.Cleanup()) }()

		require.Equal(t, "/foo/bar", execEnv.BinaryPath)
		require.Equal(t, []string{
			"NO_SET_GIT_TEMPLATE_DIR=YesPlease",
		}, execEnv.EnvironmentVariables)
	})
}

func TestBundledGitEnvironmentConstructor(t *testing.T) {
	testhelper.Unsetenv(t, "GITALY_TESTING_GIT_BINARY")

	constructor := gitcmd.BundledGitConstructors[0]

	t.Run("complete binary directory succeeds", func(t *testing.T) {
		cfg := testcfg.Build(t)
		execEnv, err := constructor.Construct(cfg)
		require.NoError(t, err)
		defer func() { require.NoError(t, execEnv.Cleanup()) }()

		// We create a temporary directory where the symlinks are created, and we cannot
		// predict its exact path.
		require.Equal(t, "git", filepath.Base(execEnv.BinaryPath))

		execPrefix := filepath.Dir(execEnv.BinaryPath)
		require.Equal(t, []string{
			"GIT_EXEC_PATH=" + execPrefix,
		}, execEnv.EnvironmentVariables)

		for _, binary := range []string{"git", "git-remote-http", "git-http-backend"} {
			target, err := filepath.EvalSymlinks(filepath.Join(execPrefix, binary))
			require.NoError(t, err)
			require.Equal(t, filepath.Join(testhelper.SourceRoot(t), "_build", "bin", fmt.Sprintf("gitaly-%s%s", binary, constructor.Suffix)), target)
		}
	})

	t.Run("environment is enabled based on the reference backend in the context", func(t *testing.T) {
		cfg := testcfg.Build(t)

		for _, tc := range []struct {
			desc string
			// constructor is the bundled Git environment under test. We only set the
			// ReferenceBackend field as that is what governs enablement here. The Suffix
			// is set during the test so that the bundled binaries can be located.
			referenceBackend git.ReferenceBackend
			// ctxBackend is the reference backend injected into the context, if any.
			ctxBackend *git.ReferenceBackend
			expected   bool
		}{
			{
				desc:             "MVCC environment with MVCC ref-backend in context is enabled",
				referenceBackend: git.ReferenceBackendMVCC,
				ctxBackend:       &git.ReferenceBackendMVCC,
				expected:         true,
			},
			{
				desc:             "MVCC environment with files ref-backend in context is disabled",
				referenceBackend: git.ReferenceBackendMVCC,
				ctxBackend:       &git.ReferenceBackendFiles,
				expected:         false,
			},
			{
				desc:             "MVCC environment with reftable ref-backend in context is disabled",
				referenceBackend: git.ReferenceBackendMVCC,
				ctxBackend:       &git.ReferenceBackendReftables,
				expected:         false,
			},
			{
				desc:             "MVCC environment without a backend in context is disabled",
				referenceBackend: git.ReferenceBackendMVCC,
				ctxBackend:       nil,
				expected:         false,
			},

			// `files` is not a dedicated environment.
			// Therefore, when no reference backend is specified on the environment (ex: `ReferenceBackend`)
			// the only acceptable values in the context are `nil` or `files`.
			{
				desc:             "general-purpose environment with a backend lacking a dedicated environment (files) is enabled",
				referenceBackend: git.ReferenceBackend{},
				ctxBackend:       &git.ReferenceBackendFiles,
				expected:         true,
			},
			{
				desc:             "general-purpose environment with a backend lacking a dedicated environment (reftables) is enabled",
				referenceBackend: git.ReferenceBackend{},
				ctxBackend:       &git.ReferenceBackendReftables,
				expected:         true,
			},
			{
				desc:             "general-purpose environment with a nil backend in context is enabled",
				referenceBackend: git.ReferenceBackend{},
				ctxBackend:       nil,
				expected:         true,
			},
			{
				desc:             "general-purpose environment with a mvcc backend in context is enabled",
				referenceBackend: git.ReferenceBackend{},
				ctxBackend:       &git.ReferenceBackendMVCC,
				expected:         false,
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				// Create the bundled git environment
				execEnv, err := gitcmd.BundledGitEnvironmentConstructor{
					Suffix:           constructor.Suffix,
					ReferenceBackend: tc.referenceBackend,
				}.Construct(cfg)
				require.NoError(t, err)

				// Cleanup on exit
				defer func() { require.NoError(t, execEnv.Cleanup()) }()

				// Now assert if the environment is enabled or not
				// based on the reference backend in the context
				ctx := testhelper.Context(t)
				if tc.ctxBackend != nil {
					ctx = gitcmd.ContextWithReferenceBackend(ctx, *tc.ctxBackend)
				}
				require.Equal(t, tc.expected, execEnv.IsEnabled(ctx))
			})
		}
	})
}

func TestFallbackGitEnvironmentConstructor(t *testing.T) {
	constructor := gitcmd.FallbackGitEnvironmentConstructor{}

	t.Run("failing lookup of executable causes failure", func(t *testing.T) {
		t.Setenv("PATH", "/does/not/exist")

		_, err := constructor.Construct(config.Cfg{})
		require.Equal(t, fmt.Errorf("%w: no git executable found in PATH", gitcmd.ErrNotConfigured), err)
	})

	t.Run("successfully resolved executable", func(t *testing.T) {
		tempDir := testhelper.TempDir(t)
		gitPath := filepath.Join(tempDir, "git")
		require.NoError(t, os.WriteFile(gitPath, nil, mode.Executable))

		t.Setenv("PATH", "/does/not/exist:"+tempDir)

		execEnv, err := constructor.Construct(config.Cfg{})
		require.NoError(t, err)
		defer func() { require.NoError(t, execEnv.Cleanup()) }()

		require.Equal(t, gitPath, execEnv.BinaryPath)
		require.Equal(t, []string(nil), execEnv.EnvironmentVariables)
	})
}
