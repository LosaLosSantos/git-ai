package gitcmd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func TestDetectReferenceBackend(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	for _, tc := range []struct {
		desc            string
		config          string
		expectedBackend git.ReferenceBackend
		expectedErr     string
	}{
		{
			desc: "no refstorage defaults to files",
			config: `
[core]
repositoryformatversion = 1
`,
			expectedBackend: git.ReferenceBackendFiles,
		},
		{
			desc: "files reference backend",
			config: `
[extensions]
refstorage = files
`,
			expectedBackend: git.ReferenceBackendFiles,
		},
		{
			desc: "reftable reference backend",
			config: `
[extensions]
refstorage = reftable
`,
			expectedBackend: git.ReferenceBackendReftables,
		},
		{
			desc: "mvcc reference backend",
			config: `
[extensions]
refstorage = mvcc
`,
			expectedBackend: git.ReferenceBackendMVCC,
		},
		{
			desc: "mvcc reference backend with matching object storage",
			config: `
[extensions]
refstorage = mvcc
objectstorage = mvcc
`,
			expectedBackend: git.ReferenceBackendMVCC,
		},
		{
			// The object storage extension is intentionally ignored: the reference backend
			// alone selects the Git binary, so an MVCC reference backend is detected even
			// when the object storage differs.
			desc: "mvcc reference backend ignores object storage",
			config: `
[extensions]
	refstorage = mvcc
	objectstorage = files
`,
			expectedBackend: git.ReferenceBackendMVCC,
		},
		{
			desc: "when multiple extensions block defined, pick last refstorage defined in extensions",
			config: `
[extensions]
something = somewhere
refstorage = files
[extensions]
sometime = somehow
refstorage = mvcc
[core]
refstorage = files
`,
			expectedBackend: git.ReferenceBackendMVCC,
		},
		{
			desc: "when multiple extensions block defined, pick last refstorage defined  in extensions (case insensitive)",
			config: `
[extensions]
something = somewhere
refstorage = files
[EXTENSIONS]
sometime = somehow
refstorage = mvcc
[core]
refstorage = files
`,
			expectedBackend: git.ReferenceBackendMVCC,
		},
		{
			desc: "an empty extensions block at the end should be tolerated by the parser",
			config: `
[extensions]
something = somewhere
refstorage = files
[EXTENSIONS]
sometime = somehow
refstorage = mvcc
[extensions]


`,
			expectedBackend: git.ReferenceBackendMVCC,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			repoPath := testhelper.TempDir(t)
			require.NoError(t, os.WriteFile(filepath.Join(repoPath, "config"), []byte(tc.config), mode.File))

			backend, err := gitcmd.DetectReferenceBackend(ctx, repoPath)
			if tc.expectedErr != "" {
				require.EqualError(t, err, tc.expectedErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectedBackend, backend)
		})
	}

	t.Run("missing config file", func(t *testing.T) {
		t.Parallel()

		_, err := gitcmd.DetectReferenceBackend(ctx, testhelper.TempDir(t))
		require.ErrorContains(t, err, "open:")
	})
}

// TestNewRepositoryReferenceBackend covers the ref-format the command factory passes to
// git-init(1). It has to match the backend on the context, which also selects the binary.
func TestNewRepositoryReferenceBackend(t *testing.T) {
	t.Parallel()

	// When GIT_DEFAULT_REF_FORMAT is set, Git honors it directly and the command factory skips
	// its own ref-format selection, so there is nothing to assert.
	testhelper.SkipWithReftable(t, "ref-format is dictated by GIT_DEFAULT_REF_FORMAT")

	cfg := testcfg.Build(t)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)

	for _, tc := range []struct {
		desc            string
		requiresMVCCGit bool
		ctxBackend      git.ReferenceBackend
		expectedBackend git.ReferenceBackend
	}{
		{
			desc:            "no backend on the context",
			expectedBackend: git.ReferenceBackendFiles,
		},
		{
			desc:            "files",
			ctxBackend:      git.ReferenceBackendFiles,
			expectedBackend: git.ReferenceBackendFiles,
		},
		{
			desc:            "reftable",
			ctxBackend:      git.ReferenceBackendReftables,
			expectedBackend: git.ReferenceBackendReftables,
		},
		{
			desc:            "mvcc",
			requiresMVCCGit: true,
			ctxBackend:      git.ReferenceBackendMVCC,
			expectedBackend: git.ReferenceBackendMVCC,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			if tc.requiresMVCCGit {
				gittest.SkipWithoutMVCCGit(t)
			}

			ctx := testhelper.Context(t)
			if tc.ctxBackend.Name != "" {
				ctx = gitcmd.ContextWithReferenceBackend(ctx, tc.ctxBackend)
			}

			repoPath := filepath.Join(testhelper.TempDir(t), "repo.git")

			cmd, err := gitCmdFactory.NewWithoutRepo(ctx, gitcmd.Command{
				Name: "init",
				Flags: []gitcmd.Option{
					gitcmd.Flag{Name: "--bare"},
				},
				Args: []string{repoPath},
			})
			require.NoError(t, err)
			require.NoError(t, cmd.Wait())

			backend, err := gitcmd.DetectReferenceBackend(ctx, repoPath)
			require.NoError(t, err)
			require.Equal(t, tc.expectedBackend.Name, backend.Name)
		})
	}
}
