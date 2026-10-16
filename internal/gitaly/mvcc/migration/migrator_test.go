package migration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func TestMigrator(t *testing.T) {
	gittest.SkipWithoutMVCCGit(t)
	testhelper.SkipWithReftable(t, "checks migration from the files backend")

	t.Parallel()

	type setupData struct {
		repo     *localrepo.Repo
		repoPath string
		block    func() error
		unblock  func() error
		// expectedLooseRefs is the number of loose references migrate() is
		// expected to report having replayed. Only checked on success.
		expectedLooseRefs int
		expectedErr       error
	}

	for _, tc := range []struct {
		desc  string
		setup func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData
	}{
		{
			desc: "without packed-refs",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("branch"))

				return setupData{
					repo:     localrepo.NewTestRepo(t, cfg, repoProto),
					repoPath: repoPath,
					block:    func() error { return nil },
					unblock:  func() error { return nil },
					// Nothing is packed, so the branch is replayed.
					expectedLooseRefs: 1,
				}
			},
		},
		{
			desc: "empty repository",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				return setupData{
					repo:     localrepo.NewTestRepo(t, cfg, repoProto),
					repoPath: repoPath,
					block:    func() error { return nil },
					unblock:  func() error { return nil },
				}
			},
		},
		{
			desc: "already on mvcc is a no-op",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repoProto, repoPath := gittest.CreateMVCCRepository(t, ctx, cfg)

				return setupData{
					repo:     localrepo.NewTestRepo(t, cfg, repoProto),
					repoPath: repoPath,
					block:    func() error { return nil },
					unblock:  func() error { return nil },
					// migrate() reports this distinctly from a fresh success so the
					// caller can avoid emitting bogus metrics/logs for work that
					// never actually ran.
					expectedErr: errAlreadyMigrated,
				}
			},
		},
		{
			desc: "with packed-refs only",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("branch"))
				gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")

				return setupData{
					repo:     localrepo.NewTestRepo(t, cfg, repoProto),
					repoPath: repoPath,
					block:    func() error { return nil },
					unblock:  func() error { return nil },
					// Everything is packed, so nothing is replayed.
					expectedLooseRefs: 0,
				}
			},
		},
		{
			desc: "with packed-refs and loose refs written after packing",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})
				c1 := gittest.WriteCommit(
					t, cfg, repoPath, gittest.WithBranch("packed"),
					gittest.WithTree(gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
						{Path: "foo", Mode: "100644", Content: "foo"},
					})),
				)
				gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")

				// Write a loose ref after packing; must survive migration.
				gittest.WriteCommit(
					t, cfg, repoPath, gittest.WithParents(c1), gittest.WithBranch("loose"),
					gittest.WithTree(gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
						{Path: "foo", Mode: "100644", Content: "bar"},
					})),
				)

				return setupData{
					repo:     localrepo.NewTestRepo(t, cfg, repoProto),
					repoPath: repoPath,
					block:    func() error { return nil },
					unblock:  func() error { return nil },
					// The single ref written after packing is replayed.
					expectedLooseRefs: 1,
				}
			},
		},
		{
			desc: "loose refs written inside block() are captured (applyLooseRefs runs after block)",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})
				c1 := gittest.WriteCommit(
					t, cfg, repoPath, gittest.WithBranch("packed"),
					gittest.WithTree(gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
						{Path: "foo", Mode: "100644", Content: "foo"},
					})),
				)
				gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")

				// A ref written once the lock is held must still be migrated.
				blockFn := func() error {
					gittest.WriteCommit(
						t, cfg, repoPath, gittest.WithParents(c1),
						gittest.WithBranch("inside-block"),
						gittest.WithTree(gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
							{Path: "foo", Mode: "100644", Content: "baz"},
						})),
					)
					return nil
				}

				return setupData{
					repo:     localrepo.NewTestRepo(t, cfg, repoProto),
					repoPath: repoPath,
					block:    blockFn,
					unblock:  func() error { return nil },
					// The ref block() wrote is picked up by the walk, proving
					// applyLooseRefs runs inside the critical section.
					expectedLooseRefs: 1,
				}
			},
		},
		{
			desc: "packed-refs created between stat and swap aborts",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("branch"))

				return setupData{
					repo:     localrepo.NewTestRepo(t, cfg, repoProto),
					repoPath: repoPath,
					block: func() error {
						gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")
						return nil
					},
					unblock:     func() error { return nil },
					expectedErr: errPackedRefsModified,
				}
			},
		},
		{
			desc: "packed-refs modified between stat and swap aborts",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})
				gittest.WriteCommit(
					t, cfg, repoPath, gittest.WithBranch("branch"),
					gittest.WithTree(gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
						{Path: "foo", Mode: "100644", Content: "foo"},
					})),
				)
				gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")

				blockFn := func() error {
					return os.Chtimes(filepath.Join(repoPath, "packed-refs"), time.Now(), time.Now())
				}

				return setupData{
					repo:        localrepo.NewTestRepo(t, cfg, repoProto),
					repoPath:    repoPath,
					block:       blockFn,
					unblock:     func() error { return nil },
					expectedErr: errPackedRefsModified,
				}
			},
		},
		{
			desc: "leftover mvcc directory from an interrupted attempt is cleared",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("branch"))
				gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")

				// Promoting the staged store would fail on a non-empty directory.
				require.NoError(t, os.MkdirAll(filepath.Join(repoPath, mvccDir, "stale"), mode.Directory))

				return setupData{
					repo:     localrepo.NewTestRepo(t, cfg, repoProto),
					repoPath: repoPath,
					block:    func() error { return nil },
					unblock:  func() error { return nil },
				}
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			data := tc.setup(t, ctx, cfg)

			// Snapshot the ref list before migration. Re-read it after block()
			// so tests that write refs inside block() are compared correctly.
			before := gittest.Exec(t, cfg, "-C", data.repoPath, "refs", "list")
			prev := data.block
			data.block = func() error {
				err := prev()
				before = gittest.Exec(t, cfg, "-C", data.repoPath, "refs", "list")
				return err
			}

			stats, migrateErr := migrate(ctx, testhelper.NewLogger(t), data.repo, data.block, data.unblock)

			if errors.Is(data.expectedErr, errAlreadyMigrated) {
				require.ErrorIs(t, migrateErr, errAlreadyMigrated)
				backend, err := localrepo.NewTestRepo(t, cfg, data.repo.Repository).ReferenceBackend(ctx)
				require.NoError(t, err)
				require.Equal(t, git.ReferenceBackendMVCC.Name, backend.Name, "backend must remain mvcc")
				require.DirExists(t, filepath.Join(data.repoPath, mvccDir), "mvcc/ must not be removed")
				return
			}

			if data.expectedErr != nil {
				require.ErrorContains(t, migrateErr, data.expectedErr.Error())
				backend, err := localrepo.NewTestRepo(t, cfg, data.repo.Repository).ReferenceBackend(ctx)
				require.NoError(t, err)
				require.Equal(t, git.ReferenceBackendFiles, backend, "backend must not have changed on error")

				// No MVCC state or staging directories must be left behind.
				require.NoDirExists(t, filepath.Join(data.repoPath, mvccDir))
				stages, err := filepath.Glob(filepath.Join(data.repoPath, migrationStageGlob))
				require.NoError(t, err)
				require.Empty(t, stages, "staging directories must be cleaned up")

				// refs/ must still look like a files-backend directory (heads is a
				// directory, not the MVCC stub file), confirming swapRefs didn't run.
				headsStat, err := os.Stat(filepath.Join(data.repoPath, "refs", "heads"))
				require.NoError(t, err)
				require.True(t, headsStat.IsDir(), "refs/heads must still be a directory after an abort")
				return
			}

			require.NoError(t, migrateErr)

			repo := localrepo.NewTestRepo(t, cfg, data.repo.Repository)
			backend, err := repo.ReferenceBackend(ctx)
			require.NoError(t, err)
			require.Equal(t, git.ReferenceBackendMVCC.Name, backend.Name)

			require.Equal(t, data.expectedLooseRefs, stats.numLooseRefs, "reported loose ref count")

			after := gittest.Exec(t, cfg, "-C", data.repoPath, "refs", "list")
			require.Equal(t, string(before), string(after), "ref set must be identical after migration")

			// The old store and any staging artefacts must be gone.
			require.NoFileExists(t, filepath.Join(data.repoPath, "packed-refs"))
			require.NoDirExists(t, filepath.Join(data.repoPath, oldRefsBackupDir))
			stages, err := filepath.Glob(filepath.Join(data.repoPath, migrationStageGlob))
			require.NoError(t, err)
			require.Empty(t, stages, "staging directories must be cleaned up")

			// refs/heads must be the MVCC stub file (a regular file, not a directory).
			// If swapRefs didn't run, heads would still be a directory; the refs-list
			// comparison above can't detect this because git reads refs from mvcc/.
			headsStat, err := os.Stat(filepath.Join(data.repoPath, "refs", "heads"))
			require.NoError(t, err)
			require.True(t, headsStat.Mode().IsRegular(), "refs/heads must be the MVCC stub file after migration")
		})
	}
}

func TestMigrator_configKeys(t *testing.T) {
	gittest.SkipWithoutMVCCGit(t)
	testhelper.SkipWithReftable(t, "checks migration from the files backend")

	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")

	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	_, err := migrate(ctx, testhelper.NewLogger(t), repo, func() error { return nil }, func() error { return nil })
	require.NoError(t, err)

	// Read config directly; both keys must be present.
	cfgBytes, err := os.ReadFile(filepath.Join(repoPath, "config"))
	require.NoError(t, err)
	cfgStr := string(cfgBytes)
	require.Contains(t, cfgStr, "repositoryformatversion = 1")
	require.Contains(t, cfgStr, "refstorage = mvcc")
}

// TestMigrator_validThroughoutCriticalSection asserts that the
// switch is complete before the write lock is released, so a concurrent RPC
// never observes a half-migrated repository, and that what Git no longer reads
// is removed afterwards.
func TestMigrator_validThroughoutCriticalSection(t *testing.T) {
	gittest.SkipWithoutMVCCGit(t)
	testhelper.SkipWithReftable(t, "checks migration from the files backend")

	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")

	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	unblock := func() error {
		require.DirExists(t, filepath.Join(repoPath, "refs"),
			"refs/ must exist before the write lock is released")
		require.DirExists(t, filepath.Join(repoPath, mvccDir),
			"the MVCC store must be in place before the write lock is released")
		backend, err := gitcmd.DetectReferenceBackend(ctx, repoPath)
		require.NoError(t, err)
		require.Equal(t, git.ReferenceBackendMVCC.Name, backend.Name,
			"config must name MVCC before the write lock is released")
		return nil
	}

	_, err := migrate(ctx, testhelper.NewLogger(t), repo, func() error { return nil }, unblock)
	require.NoError(t, err)

	require.NoFileExists(t, filepath.Join(repoPath, "packed-refs"))
	require.NoDirExists(t, filepath.Join(repoPath, oldRefsBackupDir))
	leftovers, err := filepath.Glob(filepath.Join(repoPath, migrationStageGlob))
	require.NoError(t, err)
	require.Empty(t, leftovers)
}

func TestRollbackPromotedStore(t *testing.T) {
	t.Parallel()

	swapErr := errors.New("swap failed")

	for _, tc := range []struct {
		desc            string
		config          string
		expectStoreKept bool
		expectedErr     error
	}{
		{
			desc:        "config still names the old backend",
			config:      "[core]\n\trepositoryformatversion = 0\n",
			expectedErr: swapErr,
		},
		{
			desc:            "config can't be read",
			expectStoreKept: true,
			expectedErr:     swapErr,
		},
		{
			desc:            "config already names MVCC",
			config:          "[core]\n\trepositoryformatversion = 1\n[extensions]\n\trefstorage = mvcc\n",
			expectStoreKept: true,
			expectedErr:     errPostSwapCleanup,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			repoPath := t.TempDir()
			if tc.config != "" {
				require.NoError(t, os.WriteFile(filepath.Join(repoPath, "config"), []byte(tc.config), mode.File))
			}
			require.NoError(t, os.MkdirAll(filepath.Join(repoPath, mvccDir, "manifests"), mode.Directory))

			err := rollbackPromotedStore(testhelper.Context(t), repoPath, swapErr)
			require.ErrorIs(t, err, tc.expectedErr)
			require.ErrorIs(t, err, swapErr)

			if !errors.Is(tc.expectedErr, errPostSwapCleanup) {
				require.NotErrorIs(t, err, errPostSwapCleanup)
			}

			if tc.expectStoreKept {
				require.DirExists(t, filepath.Join(repoPath, mvccDir))
			} else {
				require.NoDirExists(t, filepath.Join(repoPath, mvccDir))
			}
		})
	}
}

func TestMigrator_head(t *testing.T) {
	gittest.SkipWithoutMVCCGit(t)
	testhelper.SkipWithReftable(t, "checks migration from the files backend")

	t.Parallel()

	for _, tc := range []struct {
		desc string
		// setup returns block() and the expected symbolic HEAD target, or ""
		// if HEAD is expected to be detached at the commit.
		setup func(t *testing.T, cfg config.Cfg, repoPath string, commitID git.ObjectID) (func() error, string)
	}{
		{
			desc: "changed after the dry-run",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string, commitID git.ObjectID) (func() error, string) {
				return func() error {
					gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "HEAD", "refs/heads/other")
					return nil
				}, "refs/heads/other"
			},
		},
		{
			desc: "unborn",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string, commitID git.ObjectID) (func() error, string) {
				gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "HEAD", "refs/heads/gone")
				return func() error { return nil }, "refs/heads/gone"
			},
		},
		{
			desc: "detached",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string, commitID git.ObjectID) (func() error, string) {
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "--no-deref", "HEAD", commitID.String())
				return func() error { return nil }, ""
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)

			repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})
			commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
			gittest.WriteRef(t, cfg, repoPath, "refs/heads/other", commitID)
			gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")

			block, expectedTarget := tc.setup(t, cfg, repoPath, commitID)

			_, err := migrate(ctx, testhelper.NewLogger(t), localrepo.NewTestRepo(t, cfg, repoProto), block, func() error { return nil })
			require.NoError(t, err)

			backend, err := localrepo.NewTestRepo(t, cfg, repoProto).ReferenceBackend(ctx)
			require.NoError(t, err)
			require.Equal(t, git.ReferenceBackendMVCC.Name, backend.Name)

			if expectedTarget == "" {
				require.Equal(t, "HEAD", text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", "--symbolic-full-name", "HEAD")), "HEAD must stay detached")
				require.Equal(t, commitID.String(), text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", "HEAD")))
				return
			}
			require.Equal(t, expectedTarget, text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "HEAD")))
		})
	}
}

func TestMigrator_stagesOnlyPackedRefs(t *testing.T) {
	gittest.SkipWithoutMVCCGit(t)
	testhelper.SkipWithReftable(t, "checks migration from the files backend")

	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")
	gittest.WriteRef(t, cfg, repoPath, "refs/heads/loose", commitID)
	gittest.WriteRef(t, cfg, repoPath, "refs/heads/deleted", commitID)

	var before []byte
	block := func() error {
		stages, err := filepath.Glob(filepath.Join(repoPath, migrationStageGlob))
		require.NoError(t, err)
		require.Len(t, stages, 1)
		staged := gittest.ExecOpts(t, cfg, gittest.ExecConfig{
			ReferenceBackend: git.ReferenceBackendMVCC,
			Env:              []string{"GIT_REFERENCE_BACKEND=" + git.CreateRefURI(git.ReferenceBackendMVCC, stages[0])},
		}, "-C", repoPath, "for-each-ref", "--format=%(refname)")
		require.Equal(t, "refs/heads/main", text.ChompBytes(staged), "only packed refs must be staged")

		// Deleting a loose-only ref doesn't rewrite packed-refs, so nothing but
		// leaving it unstaged keeps the swap from resurrecting it.
		gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "-d", "refs/heads/deleted")
		before = gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref")
		return nil
	}

	stats, err := migrate(ctx, testhelper.NewLogger(t), localrepo.NewTestRepo(t, cfg, repoProto), block, func() error { return nil })
	require.NoError(t, err)
	require.Equal(t, 1, stats.numLooseRefs, "only the remaining loose ref is replayed")

	require.Equal(t, string(before), string(gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref")))
	tempDir, err := config.NewLocator(cfg).TempDir(cfg.Storages[0].Name)
	require.NoError(t, err)
	standIns, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	require.Empty(t, standIns, "the stand-in must be removed")
}

func TestMigrator_unparsableLooseRefs(t *testing.T) {
	gittest.SkipWithoutMVCCGit(t)
	testhelper.SkipWithReftable(t, "checks migration from the files backend")

	t.Parallel()

	for _, tc := range []struct {
		desc        string
		path        string
		content     string
		expectedErr string
	}{
		{
			desc:        "truncated loose ref",
			path:        "refs/heads/truncated",
			content:     "abc",
			expectedErr: `invalid loose reference "refs/heads/truncated"`,
		},
		{
			desc:        "entry starting with a dot",
			path:        "refs/heads/.nfs0001",
			expectedErr: `unexpected entry "refs/heads/.nfs0001" in refs/`,
		},
		{
			desc:        "unparsable HEAD",
			path:        "HEAD",
			content:     "garbage",
			expectedErr: `applying HEAD: invalid loose reference "HEAD"`,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)

			repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})
			commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
			gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")

			content := tc.content
			if content == "" {
				content = commitID.String()
			}
			// Write the file only once the lock is held, as the repository's
			// own validation would reject an unparsable HEAD before that.
			block := func() error {
				return os.WriteFile(filepath.Join(repoPath, tc.path), []byte(content+"\n"), mode.File)
			}

			_, err := migrate(ctx, testhelper.NewLogger(t), localrepo.NewTestRepo(t, cfg, repoProto), block, func() error { return nil })
			require.ErrorContains(t, err, tc.expectedErr)

			backend, err := gitcmd.DetectReferenceBackend(ctx, repoPath)
			require.NoError(t, err)
			require.Equal(t, git.ReferenceBackendFiles.Name, backend.Name, "the repository must stay on the files backend")
		})
	}
}

// TestMigrator_packedAnnotatedTagPeel verifies that packed annotated tags have
// their peeled commit stored in the reftable after migration, not just computed
// on the fly. Without objects/info/alternates in the stand-in repository,
// peel_object() fails and the tag is stored as VAL1 (no cached peel); then
// upload-pack has to re-read the tag object on every clone/fetch, and if the
// object is unavailable the ^{} line is simply absent.
func TestMigrator_packedAnnotatedTagPeel(t *testing.T) {
	gittest.SkipWithoutMVCCGit(t)
	testhelper.SkipWithReftable(t, "checks migration from the files backend")

	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	tagID := gittest.WriteTag(t, cfg, repoPath, "v1.0", commitID.Revision(), gittest.WriteTagConfig{Message: "release"})
	gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")

	_, err := migrate(ctx, testhelper.NewLogger(t),
		localrepo.NewTestRepo(t, cfg, repoProto),
		func() error { return nil },
		func() error { return nil })
	require.NoError(t, err)

	tagObjPath := filepath.Join(repoPath, "objects", tagID.String()[:2], tagID.String()[2:])
	require.FileExists(t, tagObjPath, "precondition: tag object must exist")

	// Remove the tag object to prove the peel comes from the reftable (VAL2),
	// not from re-reading the object on the fly.
	require.NoError(t, os.Remove(tagObjPath))

	out := gittest.ExecOpts(t, cfg, gittest.ExecConfig{
		ReferenceBackend: git.ReferenceBackendMVCC,
	}, "-C", repoPath, "upload-pack", "--advertise-refs", ".")
	require.Contains(t, string(out), commitID.String()+" refs/tags/v1.0^{}",
		"peel must be cached in the reftable so upload-pack doesn't need the tag object")
}

// TestRepairMissingRefs covers the pure filesystem repair logic directly: no
// git process is involved, so this exercises every source (and its absence)
// cheaply and precisely.
func TestRepairMissingRefs(t *testing.T) {
	t.Parallel()

	t.Run("refs/ already present is a no-op", func(t *testing.T) {
		t.Parallel()

		repoPath := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(repoPath, "refs"), mode.Directory))
		require.NoError(t, os.WriteFile(filepath.Join(repoPath, "refs", "marker"), []byte("x"), mode.File))

		require.NoError(t, repairMissingRefs(repoPath))

		require.FileExists(t, filepath.Join(repoPath, "refs", "marker"),
			"an existing refs/ directory must be left untouched")
	})

	t.Run("prefers a leftover migration staging directory's refs/", func(t *testing.T) {
		t.Parallel()

		repoPath := t.TempDir()

		// A leftover from an interrupted attempt: swapRefs never got to
		// promote the refs/ it still holds.
		stagePath := filepath.Join(repoPath, "ref_migration.abc123")
		require.NoError(t, os.MkdirAll(filepath.Join(stagePath, "refs"), mode.Directory))
		require.NoError(t, os.WriteFile(filepath.Join(stagePath, "refs", "from-stage"), []byte("x"), mode.File))

		// Also present: the old refs/ swapRefs moved aside before
		// attempting the promotion. The staging directory must win, since
		// it's closer to the swap's intended end state.
		require.NoError(t, os.MkdirAll(filepath.Join(repoPath, oldRefsBackupDir), mode.Directory))
		require.NoError(t, os.WriteFile(filepath.Join(repoPath, oldRefsBackupDir, "from-old"), []byte("x"), mode.File))

		require.NoError(t, repairMissingRefs(repoPath))

		require.FileExists(t, filepath.Join(repoPath, "refs", "from-stage"))
		require.NoDirExists(t, stagePath, "the staging directory must be removed once its refs/ is promoted")
	})

	t.Run("falls back to the pre-migration refs/ backup", func(t *testing.T) {
		t.Parallel()

		repoPath := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(repoPath, oldRefsBackupDir), mode.Directory))
		require.NoError(t, os.WriteFile(filepath.Join(repoPath, oldRefsBackupDir, "from-old"), []byte("x"), mode.File))

		require.NoError(t, repairMissingRefs(repoPath))

		require.FileExists(t, filepath.Join(repoPath, "refs", "from-old"))
		require.NoDirExists(t, filepath.Join(repoPath, oldRefsBackupDir))
	})

	t.Run("falls back to an empty directory when nothing survived", func(t *testing.T) {
		t.Parallel()

		repoPath := t.TempDir()

		require.NoError(t, repairMissingRefs(repoPath))

		require.DirExists(t, filepath.Join(repoPath, "refs"))
	})

	t.Run("ignores a staging directory whose own refs/ is also missing", func(t *testing.T) {
		t.Parallel()

		repoPath := t.TempDir()
		// A staging directory can exist without a refs/ subdirectory if the
		// crash landed even earlier, e.g. mid dry-run. It must not be treated
		// as a usable source.
		require.NoError(t, os.MkdirAll(filepath.Join(repoPath, "ref_migration.abc123"), mode.Directory))
		require.NoError(t, os.MkdirAll(filepath.Join(repoPath, oldRefsBackupDir), mode.Directory))
		require.NoError(t, os.WriteFile(filepath.Join(repoPath, oldRefsBackupDir, "from-old"), []byte("x"), mode.File))

		require.NoError(t, repairMissingRefs(repoPath))

		require.FileExists(t, filepath.Join(repoPath, "refs", "from-old"),
			"must fall through to the old refs/ backup rather than an empty staging directory")
	})
}

// TestMigrator_repairsInterruptedSwap simulates a crash between swapBackend
// committing the MVCC backend to config and swapRefs finishing the on-disk
// swap: config already says mvcc, but refs/ is missing. migrate() must repair
// this the next time it sees the repository, even though it otherwise treats
// an mvcc-backend repository as already done and returns immediately.
func TestMigrator_repairsInterruptedSwap(t *testing.T) {
	gittest.SkipWithoutMVCCGit(t)
	testhelper.SkipWithReftable(t, "checks migration from the files backend")

	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	for _, tc := range []struct {
		desc  string
		setup func(t *testing.T, repoPath string)
	}{
		{
			desc: "nothing survived the crash",
			setup: func(t *testing.T, repoPath string) {
				require.NoError(t, os.RemoveAll(filepath.Join(repoPath, "refs")))
			},
		},
		{
			desc: "the staging directory survived, still holding the refs/ about to be promoted",
			setup: func(t *testing.T, repoPath string) {
				// Recreate the exact on-disk shape swapRefs leaves behind
				// between moving refs/ aside and promoting the staged one.
				stagePath := filepath.Join(repoPath, "ref_migration.crash1")
				require.NoError(t, os.Rename(filepath.Join(repoPath, "refs"), filepath.Join(repoPath, oldRefsBackupDir)))
				require.NoError(t, os.MkdirAll(filepath.Join(stagePath, "refs"), mode.Directory))
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			repoProto, repoPath := gittest.CreateMVCCRepository(t, ctx, cfg)
			repo := localrepo.NewTestRepo(t, cfg, repoProto)

			tc.setup(t, repoPath)
			require.NoDirExists(t, filepath.Join(repoPath, "refs"), "precondition: the crash left refs/ missing")

			_, err := migrate(ctx, testhelper.NewLogger(t), repo, func() error { return nil }, func() error { return nil })
			require.ErrorIs(t, err, errAlreadyMigrated, "the repository is still on mvcc; nothing was actually migrated")

			require.DirExists(t, filepath.Join(repoPath, "refs"),
				"refs/ must be repaired even though migrate() treats mvcc as already done")
			require.NoDirExists(t, filepath.Join(repoPath, oldRefsBackupDir), "leftovers must be cleaned up")
			leftovers, globErr := filepath.Glob(filepath.Join(repoPath, migrationStageGlob))
			require.NoError(t, globErr)
			require.Empty(t, leftovers, "leftovers must be cleaned up")

			backend, err := repo.ReferenceBackend(ctx)
			require.NoError(t, err)
			require.Equal(t, git.ReferenceBackendMVCC.Name, backend.Name, "the repair must not touch the backend")
		})
	}
}

func TestMigrator_reftable(t *testing.T) {
	gittest.SkipWithoutMVCCGit(t)

	t.Parallel()

	for _, tc := range []struct {
		desc  string
		setup func(t *testing.T, repoPath string)
		// writeInBlock writes a ref once the lock is held, after the unlocked
		// dry-run, which makes the staged store outdated.
		writeInBlock bool
	}{
		{
			desc:  "migrates using the unlocked dry-run",
			setup: func(t *testing.T, repoPath string) {},
		},
		{
			desc:         "gives up after a write during the dry-run",
			setup:        func(t *testing.T, repoPath string) {},
			writeInBlock: true,
		},
		{
			desc: "clears a leftover mvcc directory from an attempt interrupted before the config commit",
			setup: func(t *testing.T, repoPath string) {
				require.NoError(t, os.MkdirAll(filepath.Join(repoPath, mvccDir, "stale"), mode.Directory))
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)

			repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})
			commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
			gittest.WriteTag(t, cfg, repoPath, "v1", commitID.Revision())
			if !testhelper.IsReftableEnabled() {
				gittest.Exec(t, cfg, "-C", repoPath, "refs", "migrate", "--ref-format=reftable")
			}
			require.DirExists(t, filepath.Join(repoPath, "reftable"), "precondition: the repository uses reftable")
			tc.setup(t, repoPath)

			var before []byte
			var unlockedStage string
			block := func() error {
				stages, err := filepath.Glob(filepath.Join(repoPath, migrationStageGlob))
				require.NoError(t, err)
				require.Len(t, stages, 1, "the dry-run must have run before the lock is taken")
				unlockedStage = stages[0]

				if tc.writeInBlock {
					gittest.WriteRef(t, cfg, repoPath, "refs/heads/inside-block", commitID)
				}
				before = gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref")
				return nil
			}
			unblock := func() error {
				if tc.writeInBlock {
					return nil
				}
				require.DirExists(t, filepath.Join(repoPath, "refs"), "refs/ must exist before the lock is released")
				require.DirExists(t, filepath.Join(repoPath, mvccDir), "the MVCC store must be in place before the lock is released")
				backend, err := gitcmd.DetectReferenceBackend(ctx, repoPath)
				require.NoError(t, err)
				require.Equal(t, git.ReferenceBackendMVCC.Name, backend.Name, "config must name MVCC before the lock is released")

				require.NoDirExists(t, filepath.Join(unlockedStage, mvccDir), "the unlocked dry-run must have been promoted")
				return nil
			}

			repo := localrepo.NewTestRepo(t, cfg, repoProto)
			_, err := migrate(ctx, testhelper.NewLogger(t), repo, block, unblock)
			if tc.writeInBlock {
				require.ErrorIs(t, err, errReftableModified)
				require.DirExists(t, filepath.Join(repoPath, "reftable"), "the repository must stay on reftable")
				require.Equal(t, string(before), string(gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref")))
				leftovers, err := filepath.Glob(filepath.Join(repoPath, migrationStageGlob))
				require.NoError(t, err)
				require.Empty(t, leftovers, "the outdated dry-run must be removed")
				return
			}
			require.NoError(t, err)

			backend, err := localrepo.NewTestRepo(t, cfg, repoProto).ReferenceBackend(ctx)
			require.NoError(t, err)
			require.Equal(t, git.ReferenceBackendMVCC.Name, backend.Name)

			require.Equal(t, string(before), string(gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref")), "ref set must be identical after migration")
			require.Equal(t, "refs/heads/main", text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "HEAD")))

			require.NoDirExists(t, filepath.Join(repoPath, "reftable"))
			leftovers, err := filepath.Glob(filepath.Join(repoPath, migrationStageGlob))
			require.NoError(t, err)
			require.Empty(t, leftovers)
		})
	}
}
