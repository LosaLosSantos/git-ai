package mvcc_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/mvcc"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// collectingVisitor is an mvcc.ArtifactVisitor that records every artifact it is handed.
type collectingVisitor struct {
	references []mvcc.Artifact
	objects    []mvcc.Artifact
}

func (v *collectingVisitor) OnReference(_ context.Context, a mvcc.Artifact) error {
	v.references = append(v.references, a)
	return nil
}

func (v *collectingVisitor) OnObject(_ context.Context, a mvcc.Artifact) error {
	v.objects = append(v.objects, a)
	return nil
}

func TestMVCCCache(t *testing.T) {
	commitFile := func(cfg config.Cfg, repoPath, fileName, content string) git.ObjectID {
		return gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithBranch("main"),
			gittest.WithMessage("First commit"),
			gittest.WithTreeEntries(
				gittest.TreeEntry{
					Mode:    "100644",
					Path:    fileName,
					Content: content,
				},
			),
		)
	}

	setMvccEnv := func(mc *mvcc.LocalCache) {
		for _, env := range mc.Environment() {
			t.Setenv(env.Name, env.Value)
		}
	}

	ctx := testhelper.Context(t)
	ctx = gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC)

	cfg := testcfg.Build(t)

	// Create an MVCC repository
	_, fullRepoPath := gittest.CreateMVCCRepository(t, ctx, cfg)

	// Make sure it has the MVCC backend
	backend, err := gitcmd.DetectReferenceBackend(ctx, fullRepoPath)
	require.NoError(t, err)
	require.Equal(t, git.ReferenceBackendMVCC, backend)

	// Localize repo on disk
	repoDir := cfg.Storages[0].Path
	relativePath, ok := strings.CutPrefix(fullRepoPath, repoDir)
	require.True(t, ok)

	// Create a LocalStorage since we are going to run tests against a repository
	// that stores all MVCC artifacts on disk.
	rs := mvcc.NewLocalStorage(repoDir)

	// Commit on the repo and fetch latest manifest pointer
	firstCommit := commitFile(cfg, fullRepoPath, "README.md", "This is a README file")
	p1Pointer, err := rs.ReadManifestPointer(ctx, relativePath)
	require.NoError(t, err)
	p1 := p1Pointer.Hash

	// Commit a second time and fetch latest manifest pointer
	secondCommit := commitFile(cfg, fullRepoPath, "README.md", "This is a README file with more stuff")
	p2Pointer, err := rs.ReadManifestPointer(ctx, relativePath)
	require.NoError(t, err)
	p2 := p2Pointer.Hash

	// Make sure our two commits are different and that the second one updated the manifest pointer
	require.NotEqual(t, firstCommit, secondCommit)
	require.NotEqual(t, p1, p2)

	testCases := []struct {
		name string
		run  func(ctx context.Context)
	}{
		{
			name: "a read-only RPC with no pinned hash should set GIT_MVCC_MANIFEST to latest",
			run: func(ctx context.Context) {
				mc := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, true)
				mc.Prepare(ctx)
				require.NoError(t, mc.WaitUntilReady(ctx))

				require.Len(t, mc.Environment(), 1)
				require.Equal(t, mc.Environment()[0].String(), fmt.Sprintf("GIT_MVCC_MANIFEST=%s", p2))

				setMvccEnv(mc)
				out := gittest.Exec(t, cfg, "-C", filepath.Join(repoDir, relativePath), "rev-parse", "main")
				ref := strings.TrimSpace(string(out))
				require.Equal(t, secondCommit.String(), ref)
			},
		},
		{
			name: "a read-only RPC with a pinned hash should set GIT_MVCC_MANIFEST to said hash",
			run: func(ctx context.Context) {
				mc := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, true)

				// Here we prepare the cache with manifest pointer p1
				mc.PrepareWithManifest(ctx, p1)
				require.NoError(t, mc.WaitUntilReady(ctx))

				envs := mc.Environment()
				require.Len(t, envs, 1)
				require.Equal(t, envs[0].String(), fmt.Sprintf("GIT_MVCC_MANIFEST=%s", p1))

				setMvccEnv(mc)
				out := gittest.Exec(t, cfg, "-C", filepath.Join(repoDir, relativePath), "rev-parse", "main")
				ref := strings.TrimSpace(string(out))
				require.Equal(t, firstCommit.String(), ref)
			},
		},
		{
			name: "a write only RPC with no pinned hash should set GIT_MVCC_MANIFEST_PATH to latest",
			run: func(ctx context.Context) {
				mc := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, false)
				defer mc.Cleanup()

				// Here we prepare the cache with the latest manifest pointer (p2)
				mc.Prepare(ctx)
				require.NoError(t, mc.WaitUntilReady(ctx))

				envs := mc.Environment()
				require.Len(t, envs, 1)
				require.Equal(t, envs[0].String(), fmt.Sprintf("GIT_MVCC_MANIFEST_PATH=%s", mc.ManifestPath()))

				// Validate that the manifest path points to an existing file that contains
				// the expected pointer
				manifestPointerContent, err := os.ReadFile(mc.ManifestPath())
				require.NoError(t, err)
				require.Equal(t, string(manifestPointerContent), p2)

				// Validate that the manifest reference in the pointer exists on disk
				manifestFilePath := filepath.Join(repoDir, relativePath, "mvcc", "manifests", p2)
				manifestFile, err := os.ReadFile(manifestFilePath)
				require.NoError(t, err)

				// Validate it is a valid Manifest
				_, err = mvcc.ParseManifest(manifestFile)
				require.NoError(t, err)
			},
		},
		{
			name: "a write only RPC with a pinned hash should set GIT_MVCC_MANIFEST_PATH to said hash",
			run: func(ctx context.Context) {
				mc := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, false)
				defer mc.Cleanup()

				// Here we prepare the cache with manifest pointer p1
				mc.PrepareWithManifest(ctx, p1)
				require.NoError(t, mc.WaitUntilReady(ctx))

				envs := mc.Environment()
				require.Len(t, envs, 1)
				require.Equal(t, envs[0].String(), fmt.Sprintf("GIT_MVCC_MANIFEST_PATH=%s", mc.ManifestPath()))

				// Validate that the manifest path points to an existing file that contains
				// the expected pointer
				manifestPointerContent, err := os.ReadFile(mc.ManifestPath())
				require.NoError(t, err)
				require.Equal(t, string(manifestPointerContent), p1)

				// Validate that the manifest reference in the pointer exists on disk
				manifestFilePath := filepath.Join(repoDir, relativePath, "mvcc", "manifests", p1)
				manifestFile, err := os.ReadFile(manifestFilePath)
				require.NoError(t, err)

				// Validate it is a valid Manifest
				_, err = mvcc.ParseManifest(manifestFile)
				require.NoError(t, err)
			},
		},
		{
			name: "the base manifest pointer of a read-only RPC is the manifest it resolved",
			run: func(ctx context.Context) {
				latest := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, true)
				latest.Prepare(ctx)
				require.NoError(t, latest.WaitUntilReady(ctx))
				require.Equal(t, p2, latest.BaseManifestPointer())

				pinned := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, true)
				pinned.PrepareWithManifest(ctx, p1)
				require.NoError(t, pinned.WaitUntilReady(ctx))
				require.Equal(t, p1, pinned.BaseManifestPointer())

				// Two RPCs operating on different manifests must report different
				// pointers, otherwise the catfile process cache could hand a
				// git-cat-file(1) process spawned under one manifest to an RPC
				// operating on the other.
				require.NotEqual(t, latest.BaseManifestPointer(), pinned.BaseManifestPointer())

				// Two RPCs operating on the same manifest on the other hand must
				// report the same pointer so that their processes can be reused.
				same := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, true)
				same.PrepareWithManifest(ctx, p1)
				require.NoError(t, same.WaitUntilReady(ctx))
				require.Equal(t, pinned.BaseManifestPointer(), same.BaseManifestPointer())
			},
		},
		{
			name: "the base manifest pointer of a mutator RPC is the manifest it started from",
			run: func(ctx context.Context) {
				latest := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, false)
				latest.Prepare(ctx)
				require.NoError(t, latest.WaitUntilReady(ctx))
				defer latest.Cleanup()

				// A mutator RPC pins its manifest through a pointer file instead of
				// through GIT_MVCC_MANIFEST, but the base it resolved is the very
				// manifest that pointer file starts out holding.
				require.Equal(t, p2, latest.BaseManifestPointer())
				manifestPointerContent, err := os.ReadFile(latest.ManifestPath())
				require.NoError(t, err)
				require.Equal(t, latest.BaseManifestPointer(), string(manifestPointerContent))

				pinned := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, false)
				pinned.PrepareWithManifest(ctx, p1)
				require.NoError(t, pinned.WaitUntilReady(ctx))
				defer pinned.Cleanup()
				require.Equal(t, p1, pinned.BaseManifestPointer())
			},
		},
		{
			name: "the base manifest pointer is empty before the cache is ready",
			run: func(ctx context.Context) {
				mc := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, true)
				require.Empty(t, mc.BaseManifestPointer())
			},
		},
		{
			name: "a pinned hash that does not exist should return an error",
			run: func(ctx context.Context) {
				mc := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, false)
				defer mc.Cleanup()

				// Here we prepare the cache with manifest pointer p1
				mc.PrepareWithManifest(ctx, "abcd")
				require.Error(t, mc.WaitUntilReady(ctx))
			},
		},
		{
			name: "walking the manifest visits the pinned manifest's artifacts in the local cache",
			run: func(ctx context.Context) {
				mc := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, true)
				mc.PrepareWithManifest(ctx, p1)

				// WalkManifest waits for the cache to be ready itself.
				visitor := &collectingVisitor{}
				require.NoError(t, mc.WalkManifest(ctx, visitor))

				manifestFile, err := os.ReadFile(filepath.Join(repoDir, relativePath, "mvcc", "manifests", p1))
				require.NoError(t, err)
				manifest, err := mvcc.ParseManifest(manifestFile)
				require.NoError(t, err)

				expectedArtifacts := func(names []string) []mvcc.Artifact {
					var artifacts []mvcc.Artifact
					for _, name := range names {
						artifacts = append(artifacts, mvcc.Artifact{
							Name: name,
							Path: filepath.Join(repoDir, relativePath, "mvcc", name),
						})
					}
					return artifacts
				}

				require.NotEmpty(t, manifest.RefPaths())
				require.Equal(t, expectedArtifacts(manifest.RefPaths()), visitor.references)
				// The visited artifacts are hydrated in the local cache.
				for _, artifact := range visitor.references {
					require.FileExists(t, artifact.Path)
				}

				require.Equal(t, expectedArtifacts(manifest.ObjPaths()), visitor.objects)
			},
		},
		{
			name: "walking the manifest fails when the cache failed to prepare",
			run: func(ctx context.Context) {
				mc := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, true)
				mc.PrepareWithManifest(ctx, "abcd")

				visitor := &collectingVisitor{}
				require.Error(t, mc.WalkManifest(ctx, visitor))
				require.Empty(t, visitor.references)
				require.Empty(t, visitor.objects)
			},
		},
		{
			name: "cleanup removes the temp manifest pointer file of a mutator RPC",
			run: func(ctx context.Context) {
				mc := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, false)
				mc.Prepare(ctx)
				require.NoError(t, mc.WaitUntilReady(ctx))

				path := mc.ManifestPath()
				require.NotEmpty(t, path)
				require.FileExists(t, path)

				// Cleanup is the failure-path counterpart of Commit: it must remove the
				// per-RPC temp pointer without advancing the canonical pointer.
				mc.Cleanup()
				require.NoFileExists(t, path)
			},
		},
		{
			name: "cleanup is a no-op for a read-only RPC",
			run: func(ctx context.Context) {
				mc := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, true)
				mc.Prepare(ctx)
				require.NoError(t, mc.WaitUntilReady(ctx))

				// A read-only cache has no temp pointer file, so Cleanup must not panic
				// and must leave the manifest pointer untouched.
				require.Empty(t, mc.ManifestPath())
				mc.Cleanup()
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			tc.run(ctx)
		})
	}
}

func TestLocalCache_repositoryLock(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	ctx = gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC)

	cfg := testcfg.Build(t)

	_, fullRepoPath := gittest.CreateMVCCRepository(t, ctx, cfg)

	// A commit gives the repository a manifest to prepare against.
	gittest.WriteCommit(t, cfg, fullRepoPath, gittest.WithBranch("main"))

	storagePath := cfg.Storages[0].Path
	relativePath, ok := strings.CutPrefix(fullRepoPath, storagePath)
	require.True(t, ok)

	rs := mvcc.NewLocalStorage(storagePath)

	// Collection locks the repository under the path the housekeeping side resolves it to, so
	// the two have to agree. A mismatch would not fail anything, it would just stop the lock
	// from excluding anything.
	locator := config.NewLocator(cfg)
	repoPath, err := locator.GetRepoPath(ctx, &gitalypb.Repository{
		StorageName:  cfg.Storages[0].Name,
		RelativePath: relativePath,
	})
	require.NoError(t, err)
	require.Equal(t, fullRepoPath, repoPath,
		"the path a cache locks under must be the one housekeeping collects under")

	// Both cases assert on the exclusive garbage collection lock for the same repository, so
	// they must not run in parallel.
	for _, tc := range []struct {
		desc     string
		readOnly bool
	}{
		{desc: "mutator locks the repository", readOnly: false},
		{desc: "read-only cache does not lock the repository", readOnly: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			cache := mvcc.NewLocalCache(rs, cfg.RuntimeDir, storagePath, relativePath, tc.readOnly)
			// The lock is process-global, so a failing assertion must not leave it held.
			t.Cleanup(cache.Cleanup)

			cache.Prepare(ctx)
			require.NoError(t, cache.WaitUntilReady(ctx))

			collecting := mvcc.TryLockRepositoryForCollection(fullRepoPath)
			if tc.readOnly {
				require.NotNil(t, collecting)
				collecting.Unlock()
			} else {
				require.Nil(t, collecting)
			}

			cache.Cleanup()

			collecting = mvcc.TryLockRepositoryForCollection(fullRepoPath)
			require.NotNil(t, collecting, "cleaning up the cache must unlock the repository")
			collecting.Unlock()
		})
	}
}

// TestLocalCache_cleanupWithoutPrepare covers cleaning up a cache that was never prepared, which
// the mvcc interceptor can do because it registers the cleanup before it prepares.
func TestLocalCache_cleanupWithoutPrepare(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	storagePath := cfg.Storages[0].Path

	cache := mvcc.NewLocalCache(mvcc.NewLocalStorage(storagePath), cfg.RuntimeDir, storagePath, "repository", false)

	cleaned := make(chan struct{})
	go func() {
		cache.Cleanup()
		close(cleaned)
	}()

	select {
	case <-cleaned:
	case <-time.After(time.Minute):
		require.FailNow(t, "cleaning up a cache that was never prepared must not block")
	}
}
