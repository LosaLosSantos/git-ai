package storagecore

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/git/updateref"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/corestore/gitops"
	"github.com/feder-cr/open_scion/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// indexIntoEmptyRepository indexes the pack read from open into a new empty
// repository, applies refs to it, and returns its path.
func indexIntoEmptyRepository(t *testing.T, ctx context.Context, cfg config.Cfg, open func() (io.ReadCloser, error), refs map[string]corestore.Target,
) string {
	t.Helper()

	proto, path := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, cfg, proto)
	if open != nil {
		r, err := open()
		require.NoError(t, err)
		require.NoError(t, gitops.IndexPack(ctx, repo, r))
		require.NoError(t, r.Close())
	}
	if refs != nil {
		require.NoError(t, gitops.ApplyRefs(ctx, repo, refs))
	}
	return path
}

// hasObject reports whether the repository at path has the object oid.
func hasObject(t *testing.T, cfg config.Cfg, path, oid string) bool {
	t.Helper()

	cmd := gittest.NewCommand(t, cfg, "-C", path, "cat-file", "-e", oid)
	return cmd.Run() == nil
}

// requireConnected checks that every object reachable from the references of
// the repository at path is present.
func requireConnected(t *testing.T, cfg config.Cfg, path string) {
	t.Helper()
	gittest.Exec(t, cfg, "-C", path, "fsck", "--connectivity-only", "--no-dangling")
}

// TestBuildImportPackHoldsTheValuesThatWereRead: the pack of an import holds
// every object reachable from the reference values the import read, even when
// a reference moved before the pack was built and its old value is no longer
// reachable from any reference (shadow mode: writes run during the import).
// Packing what the references reach when the pack is built would lose them.
func TestBuildImportPackHoldsTheValuesThatWereRead(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	proto, path := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, cfg, proto)

	main := gittest.WriteCommit(t, cfg, path, gittest.WithBranch("main"), gittest.WithMessage("main"))
	a := gittest.WriteCommit(t, cfg, path, gittest.WithMessage("orphan a"),
		gittest.WithTreeEntries(gittest.TreeEntry{Path: "a", Mode: "100644", Content: "content of a"}))
	b := gittest.WriteCommit(t, cfg, path, gittest.WithMessage("orphan b"),
		gittest.WithTreeEntries(gittest.TreeEntry{Path: "b", Mode: "100644", Content: "content of b"}))
	blob := gittest.WriteBlob(t, cfg, path, []byte("a blob behind a tag"))
	tag := gittest.WriteTag(t, cfg, path, "v1", main.Revision(), gittest.WriteTagConfig{Message: "annotated"})
	gittest.Exec(t, cfg, "-C", path, "update-ref", "refs/heads/x", a.String())
	gittest.Exec(t, cfg, "-C", path, "update-ref", "refs/tags/blob", blob.String())

	refs, err := gitops.ReadRefs(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, corestore.Target(a.String()), refs["refs/heads/x"])
	require.Equal(t, corestore.Target(tag.String()), refs["refs/tags/v1"])

	// A write moves x after the read: a is reachable from no reference.
	gittest.Exec(t, cfg, "-C", path, "update-ref", "refs/heads/x", b.String())

	// What the defect did: the pack of the references as they are now lacks a.
	full, err := gitops.BuildFullPack(ctx, repo, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = full.Remove() })
	fullPath := indexIntoEmptyRepository(t, ctx, cfg, full.Open, nil)
	require.False(t, hasObject(t, cfg, fullPath, a.String()), "precondition: the moved-away value is in no pack of the current references")

	tmpDir := t.TempDir()
	pack, err := buildImportPack(ctx, repo, refs, tmpDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pack.Remove() })
	require.False(t, pack.Empty())

	// The pack is exactly the objects reachable from the values read.
	tips := []string{"rev-list", "--objects"}
	for _, target := range refs {
		if !target.IsSymbolic() {
			tips = append(tips, string(target))
		}
	}
	reachable := strings.Split(strings.TrimSpace(string(gittest.Exec(t, cfg, append([]string{"-C", path}, tips...)...))), "\n")
	require.Equal(t, len(reachable), pack.Objects)

	// Indexed into an empty repository with the references that were read,
	// it gives a complete repository; b, written after the read, is not in it.
	target := indexIntoEmptyRepository(t, ctx, cfg, pack.Open, refs)
	for _, oid := range []git.ObjectID{main, a, blob, tag} {
		require.True(t, hasObject(t, cfg, target, oid.String()), "object %s", oid)
	}
	require.False(t, hasObject(t, cfg, target, b.String()))
	requireConnected(t, cfg, target)
}

// TestBuildImportPackWithoutObjects: references without object IDs (only a
// dangling symbolic HEAD) give an empty pack and leave no file behind.
func TestBuildImportPackWithoutObjects(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	proto, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, cfg, proto)

	tmpDir := t.TempDir()
	pack, err := buildImportPack(ctx, repo, map[string]corestore.Target{
		"HEAD": corestore.SymbolicTarget("refs/heads/main"),
	}, tmpDir)
	require.NoError(t, err)
	require.True(t, pack.Empty())

	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// TestBuildImportPackRejectsInvalidTarget: a value that is not an object ID of
// the repository fails before git runs, and leaves no file behind.
func TestBuildImportPackRejectsInvalidTarget(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	proto, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, cfg, proto)

	tmpDir := t.TempDir()
	_, err := buildImportPack(ctx, repo, map[string]corestore.Target{
		"refs/heads/main": "--all",
	}, tmpDir)
	require.Error(t, err)

	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// TestImportRepositoryReferencesMovingDuringImport is the gate's repro: in
// shadow mode a reference is moved back and forth between two unrelated
// commits while ImportRepository runs. Whatever value changeset 1 records,
// the import pack holds its objects: indexed into an empty repository with the
// database references, the repository is complete.
func TestImportRepositoryReferencesMovingDuringImport(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeShadow, dialect)

		const attempts = 6
		for attempt := 0; attempt < attempts; attempt++ {
			proto, path := gittest.CreateRepository(t, h.ctx, h.cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
			repo := localrepo.NewTestRepo(t, h.cfg, proto)

			parent := gittest.WriteCommit(t, h.cfg, path, gittest.WithBranch("main"))
			for i := 0; i < 20; i++ {
				parent = gittest.WriteCommit(t, h.cfg, path, gittest.WithBranch("main"), gittest.WithParents(parent),
					gittest.WithTreeEntries(gittest.TreeEntry{Path: "f", Mode: "100644", Content: strings.Repeat("x", i+1)}))
			}
			a := gittest.WriteCommit(t, h.cfg, path, gittest.WithMessage("orphan a"),
				gittest.WithTreeEntries(gittest.TreeEntry{Path: "a", Mode: "100644", Content: "a"}))
			b := gittest.WriteCommit(t, h.cfg, path, gittest.WithMessage("orphan b"),
				gittest.WithTreeEntries(gittest.TreeEntry{Path: "b", Mode: "100644", Content: "b"}))
			gittest.Exec(t, h.cfg, "-C", path, "update-ref", "refs/heads/x", a.String())

			moverCtx, stop := context.WithCancel(h.ctx)
			var wg sync.WaitGroup
			var moves int
			var moveErr error
			wg.Add(1)
			go func() {
				defer wg.Done()
				values := []git.ObjectID{b, a}
				for i := 0; moverCtx.Err() == nil; i++ {
					if err := moveRef(moverCtx, repo, "refs/heads/x", values[i%2]); err != nil {
						if moverCtx.Err() == nil {
							moveErr = err
						}
						return
					}
					moves++
				}
			}()

			_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: proto})
			stop()
			wg.Wait()
			require.NoError(t, err)
			require.NoError(t, moveErr)

			state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: proto})
			require.NoError(t, err)
			dbRefs, head, err := h.core.Meta.Refs(h.ctx, state.GetRepositoryId())
			require.NoError(t, err)
			require.EqualValues(t, 1, head)
			require.Contains(t, []corestore.Target{corestore.Target(a.String()), corestore.Target(b.String())}, dbRefs["refs/heads/x"])

			changesets, err := h.core.Meta.ListChangesets(h.ctx, state.GetRepositoryId(), 0, 1)
			require.NoError(t, err)
			require.Len(t, changesets, 1)
			require.NotEmpty(t, changesets[0].PackKey)

			target := indexIntoEmptyRepository(t, h.ctx, h.cfg, func() (io.ReadCloser, error) {
				return h.core.Blobs.Get(h.ctx, changesets[0].PackKey)
			}, dbRefs)
			for name, value := range dbRefs {
				if !value.IsSymbolic() {
					require.True(t, hasObject(t, h.cfg, target, string(value)),
						"attempt %d (%d moves): %s recorded as %s, object not in the import pack", attempt, moves, name, value)
				}
			}
			requireConnected(t, h.cfg, target)
		}
	})
}

// moveRef sets reference to oid without running the reference-transaction
// hook, like a write that the import does not see.
func moveRef(ctx context.Context, repo *localrepo.Repo, reference string, oid git.ObjectID) error {
	updater, err := updateref.New(ctx, repo, updateref.WithDisabledTransactions())
	if err != nil {
		return err
	}
	defer func() { _ = updater.Close() }()

	if err := updater.Start(); err != nil {
		return err
	}
	if err := updater.Update(git.ReferenceName(reference), oid, ""); err != nil {
		return err
	}
	return updater.Commit()
}
