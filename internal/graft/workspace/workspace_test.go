package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seedBackend returns an in-memory repository whose main branch contains the
// given files, committed one at a time in the given order.
func seedBackend(t *testing.T, paths []string, contents []string) *MemoryBackend {
	t.Helper()
	require.Len(t, contents, len(paths))
	m := NewMemoryBackend()
	for i, p := range paths {
		m.Put("main", p, []byte(contents[i]))
	}
	return m
}

func cloneTestWorkspace(t *testing.T, b Backend, sparse ...string) *Workspace {
	t.Helper()
	ws, err := Clone(context.Background(), filepath.Join(t.TempDir(), "ws"), b, CloneOptions{
		Address: "memory",
		Storage: "default",
		Repo:    "repo",
		Branch:  "main",
		Sparse:  sparse,
	})
	require.NoError(t, err)
	return ws
}

func writeWorkspaceFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

func readWorkspaceFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	require.NoError(t, err)
	return string(data)
}

func readRemote(t *testing.T, b Backend, branch, path string) string {
	t.Helper()
	data, err := b.ReadFile(context.Background(), branch, path)
	require.NoError(t, err)
	return string(data)
}

func TestCloneMaterializesOnlySparsePaths(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt", "b/y.txt", "top.txt"}, []string{"ax", "by", "top"})
	ws := cloneTestWorkspace(t, b, "a")

	require.Equal(t, "ax", readWorkspaceFile(t, ws.Root(), "a/x.txt"))
	require.NoDirExists(t, filepath.Join(ws.Root(), "b"))
	require.NoFileExists(t, filepath.Join(ws.Root(), "top.txt"))

	summary := ws.Summary()
	require.Equal(t, 1, summary.Files)
	require.Equal(t, []string{"a"}, summary.Sparse)
	require.Equal(t, "main", summary.Branch)

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
}

func TestCloneFullCheckoutWithoutSparse(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt", "b/y.txt"}, []string{"ax", "by"})
	ws := cloneTestWorkspace(t, b)

	require.Equal(t, "by", readWorkspaceFile(t, ws.Root(), "b/y.txt"))
	require.Equal(t, 2, ws.Summary().Files)
}

func TestCloneRejectsUnsafeSparsePrefix(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt"}, []string{"ax"})
	_, err := Clone(context.Background(), filepath.Join(t.TempDir(), "ws"), b, CloneOptions{
		Branch: "main",
		Sparse: []string{"../escape"},
	})
	require.Error(t, err)
}

func TestCloneRejectsNonEmptyDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeWorkspaceFile(t, dir, "keep.txt", "keep")
	b := seedBackend(t, []string{"a.txt"}, []string{"a"})
	_, err := Clone(context.Background(), dir, b, CloneOptions{Branch: "main"})
	require.ErrorContains(t, err, "not empty")
}

func TestStatusDetectsAddedModifiedDeletedFiles(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt", "b.txt"}, []string{"one", "two"})
	ws := cloneTestWorkspace(t, b)

	writeWorkspaceFile(t, ws.Root(), "a.txt", "one changed")
	writeWorkspaceFile(t, ws.Root(), "new.txt", "new")
	require.NoError(t, os.Remove(filepath.Join(ws.Root(), "b.txt")))

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Equal(t, []Change{
		{Path: "a.txt", Kind: ChangeModified},
		{Path: "b.txt", Kind: ChangeDeleted},
		{Path: "new.txt", Kind: ChangeAdded},
	}, changes)
}

func TestStatusRehashesWhenMtimeChangesButContentDoesNot(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"same"})
	ws := cloneTestWorkspace(t, b)

	later := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(ws.Root(), "a.txt"), later, later))

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes, "a touched file with identical content is not a change")
}

func TestDiffShowsLineChanges(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"keep\nold line\nend\n"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a.txt", "keep\nnewer line\nend\n")

	diff, err := ws.Diff()
	require.NoError(t, err)
	require.Contains(t, diff, "--- a/a.txt\n+++ b/a.txt\n")
	require.Contains(t, diff, " keep\n")
	require.Contains(t, diff, "-old line\n")
	require.Contains(t, diff, "+newer line\n")
	require.Contains(t, diff, " end\n")
}

func TestCommitLandsLocalChanges(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt", "gone.txt"}, []string{"old", "bye"})
	ws := cloneTestWorkspace(t, b)

	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "new content")
	writeWorkspaceFile(t, ws.Root(), "a/added.txt", "added")
	require.NoError(t, os.Remove(filepath.Join(ws.Root(), "gone.txt")))

	res, err := ws.Commit(context.Background(), CommitRequest{
		Message:    "update things",
		Author:     Signature{Name: "Agent One", Email: "agent@example.com"},
		LandBranch: "main",
	})
	require.NoError(t, err)
	require.True(t, res.Landed)
	require.Equal(t, 3, res.Changes)
	require.Contains(t, res.Branch, "graft/agent-one/")

	require.Equal(t, "new content", readRemote(t, b, "main", "a/x.txt"))
	require.Equal(t, "added", readRemote(t, b, "main", "a/added.txt"))
	_, err = b.ReadFile(context.Background(), "main", "gone.txt")
	require.ErrorIs(t, err, ErrNotFound)

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes, "the workspace is clean after landing")
	require.Nil(t, ws.Summary().Pending)
	require.Equal(t, res.Commit, ws.Summary().Base, "the base moved to the landed commit")
}

func TestCommitWithoutLandIsPendingUntilLanded(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a.txt", "two-two")

	res, err := ws.Commit(context.Background(), CommitRequest{Message: "two", Author: Signature{Name: "dev"}})
	require.NoError(t, err)
	require.False(t, res.Landed)
	require.Equal(t, "one", readRemote(t, b, "main", "a.txt"))
	require.NotNil(t, ws.Summary().Pending)

	writeWorkspaceFile(t, ws.Root(), "a.txt", "three")
	_, err = ws.Commit(context.Background(), CommitRequest{Message: "three", Author: Signature{Name: "dev"}})
	require.ErrorContains(t, err, "pending")

	_, err = ws.Update(context.Background())
	require.ErrorContains(t, err, "pending")

	landed, err := ws.Land(context.Background(), "", "", Signature{Name: "dev"})
	require.NoError(t, err)
	require.Equal(t, res.Commit, landed.Commit)
	require.Equal(t, "two-two", readRemote(t, b, "main", "a.txt"))
	require.Nil(t, ws.Summary().Pending)
}

func TestCommitNothingToCommitAndMessageRequired(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)

	_, err := ws.Commit(context.Background(), CommitRequest{Message: "x"})
	require.ErrorIs(t, err, ErrNothingToCommit)

	writeWorkspaceFile(t, ws.Root(), "a.txt", "two")
	_, err = ws.Commit(context.Background(), CommitRequest{Message: "  "})
	require.Error(t, err)
}

func TestCommitRebasesOntoDivergedTip(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt", "b.txt"}, []string{"a", "b"})
	ws := cloneTestWorkspace(t, b)

	// Another agent lands a change to a different file meanwhile.
	b.Put("main", "other.txt", []byte("other"))

	writeWorkspaceFile(t, ws.Root(), "a.txt", "a changed")
	res, err := ws.Commit(context.Background(), CommitRequest{
		Message:    "change a",
		Author:     Signature{Name: "dev"},
		LandBranch: "main",
	})
	require.NoError(t, err)
	require.True(t, res.Landed)

	require.Equal(t, "a changed", readRemote(t, b, "main", "a.txt"))
	require.Equal(t, "other", readRemote(t, b, "main", "other.txt"))
	require.Equal(t, "other", readWorkspaceFile(t, ws.Root(), "other.txt"), "the landed tree is materialized")
}

func TestLandFailureKeepsCommitPending(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"base"})
	ws := cloneTestWorkspace(t, b)

	// Another agent changes the same file, so the land conflicts.
	b.Put("main", "a.txt", []byte("remote"))
	writeWorkspaceFile(t, ws.Root(), "a.txt", "local")

	res, err := ws.Commit(context.Background(), CommitRequest{
		Message:    "local",
		Author:     Signature{Name: "dev"},
		LandBranch: "main",
	})
	require.ErrorIs(t, err, ErrMergeConflict)
	require.False(t, res.Landed)
	require.NotNil(t, ws.Summary().Pending, "the commit stays pending for a retry")
	require.Equal(t, "remote", readRemote(t, b, "main", "a.txt"))
}

func TestUpdateRefusesLocalConflictsWithoutWriting(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt", "b.txt"}, []string{"base", "base-b"})
	ws := cloneTestWorkspace(t, b)

	writeWorkspaceFile(t, ws.Root(), "a.txt", "local edit")
	b.Put("main", "a.txt", []byte("remote edit"))
	b.Put("main", "b.txt", []byte("remote b"))
	base := ws.Summary().Base

	_, err := ws.Update(context.Background())
	var conflict *ConflictError
	require.True(t, errors.As(err, &conflict), "got %v", err)
	require.Equal(t, []string{"a.txt"}, conflict.Paths)

	require.Equal(t, "local edit", readWorkspaceFile(t, ws.Root(), "a.txt"))
	require.Equal(t, "base-b", readWorkspaceFile(t, ws.Root(), "b.txt"), "nothing is written on conflict")
	require.Equal(t, base, ws.Summary().Base)
}

func TestUpdateAppliesRemoteChanges(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"base"})
	ws := cloneTestWorkspace(t, b)

	b.Put("main", "a.txt", []byte("remote"))
	newTip := b.Put("main", "fresh.txt", []byte("fresh"))

	res, err := ws.Update(context.Background())
	require.NoError(t, err)
	require.Equal(t, newTip, res.New)
	require.Equal(t, "remote", readWorkspaceFile(t, ws.Root(), "a.txt"))
	require.Equal(t, "fresh", readWorkspaceFile(t, ws.Root(), "fresh.txt"))

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
}

func TestSparseChangeAddsAndRemovesPaths(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt", "b/y.txt"}, []string{"ax", "by"})
	ws := cloneTestWorkspace(t, b, "a")

	require.NoError(t, ws.Sparse(context.Background(), []string{"b"}))
	require.NoFileExists(t, filepath.Join(ws.Root(), "a", "x.txt"))
	require.NoDirExists(t, filepath.Join(ws.Root(), "a"), "empty directories are pruned")
	require.Equal(t, "by", readWorkspaceFile(t, ws.Root(), "b/y.txt"))
	require.Equal(t, []string{"b"}, ws.Summary().Sparse)
}

func TestSparseChangeRefusesToDropLocalEdits(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt", "b/y.txt"}, []string{"ax", "by"})
	ws := cloneTestWorkspace(t, b, "b")

	writeWorkspaceFile(t, ws.Root(), "b/y.txt", "local")
	err := ws.Sparse(context.Background(), []string{"a"})
	var conflict *ConflictError
	require.True(t, errors.As(err, &conflict), "got %v", err)
	require.Equal(t, "local", readWorkspaceFile(t, ws.Root(), "b/y.txt"))
	require.Equal(t, []string{"b"}, ws.Summary().Sparse)
}

func TestFetchRejectsObjectsThatDoNotMatchTheirID(t *testing.T) {
	t.Parallel()

	inner := seedBackend(t, []string{"a.txt"}, []string{"genuine"})
	_, err := Clone(context.Background(), filepath.Join(t.TempDir(), "ws"), corruptingBackend{Backend: inner}, CloneOptions{Branch: "main"})
	require.ErrorContains(t, err, "does not match")
}

// corruptingBackend returns altered content for every object it serves.
type corruptingBackend struct {
	Backend
}

func (c corruptingBackend) GetObjects(ctx context.Context, oids []string) (map[string][]byte, error) {
	out, err := c.Backend.GetObjects(ctx, oids)
	for oid := range out {
		out[oid] = []byte("tampered")
	}
	return out, err
}

func TestOpenRejectsTamperedStatePaths(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)

	ws.state.Files["../escape.txt"] = FileState{OID: zeroOID}
	require.NoError(t, ws.state.save(ws.Root()))

	_, err := Open(ws.Root(), b)
	require.ErrorContains(t, err, "invalid path")
}

func TestOpenAndFindLocateWorkspace(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)

	nested := filepath.Join(ws.Root(), "deep", "dir")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	root, err := Find(nested)
	require.NoError(t, err)
	require.Equal(t, ws.Root(), root)

	reopened, err := Open(root, b)
	require.NoError(t, err)
	require.Equal(t, "main", reopened.Summary().Branch)

	_, err = Find(t.TempDir())
	require.ErrorIs(t, err, ErrNotWorkspace)
}

func TestUpdateRefusesToWriteThroughSymbolicLink(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws.Root(), "linked")); err != nil {
		t.Skipf("symbolic links not available: %v", err)
	}
	b.Put("main", "linked/evil.txt", []byte("evil"))

	_, err := ws.Update(context.Background())
	require.ErrorContains(t, err, "symbolic link")
	require.NoFileExists(t, filepath.Join(outside, "evil.txt"))
}

func TestDiffAfterCommitAndLandUsesCachedContent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"f.txt"}, []string{"A\n"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "f.txt", "B\n")

	_, err := ws.Commit(ctx, CommitRequest{Message: "B", Author: Signature{Name: "dev"}, LandBranch: "main"})
	require.NoError(t, err)

	writeWorkspaceFile(t, ws.Root(), "f.txt", "C\n")
	diff, err := ws.Diff()
	require.NoError(t, err)
	require.Contains(t, diff, "-B\n+C\n")

	reopened, err := Open(ws.Root(), b)
	require.NoError(t, err)
	diff, err = reopened.Diff()
	require.NoError(t, err)
	require.Contains(t, diff, "-B\n+C\n")
}

func TestLandDoesNotCreateMissingBranch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"a.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a.txt", "two")

	_, err := ws.Commit(ctx, CommitRequest{Message: "two", Author: Signature{Name: "dev"}, LandBranch: "mian"})
	require.Error(t, err)
	_, err = b.ReadFile(ctx, "mian", "a.txt")
	require.Error(t, err, "a mistyped branch is not created")
	require.NotNil(t, ws.Summary().Pending, "the commit stays pending for a retry")

	_, err = ws.Land(ctx, "main", "", Signature{Name: "dev"})
	require.NoError(t, err)
	require.Equal(t, "two", readRemote(t, b, "main", "a.txt"))
}

func TestCommitReportsAddedPathThatExistsOutsideSparseSet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"a/x.txt", "b/y.txt"}, []string{"x", "y"})
	ws := cloneTestWorkspace(t, b, "a")
	writeWorkspaceFile(t, ws.Root(), "b/y.txt", "local")

	_, err := ws.Commit(ctx, CommitRequest{Message: "m", Author: Signature{Name: "dev"}, LandBranch: "main"})
	require.ErrorContains(t, err, "outside the sparse set")
	require.Equal(t, "y", readRemote(t, b, "main", "b/y.txt"))
	require.Nil(t, ws.Summary().Pending)
}

func TestRefreshFailureKeepsLandPendingUntilRetried(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	author := Signature{Name: "dev"}
	b := seedBackend(t, []string{"f.txt", "g.txt"}, []string{"A", "G0"})
	ws := cloneTestWorkspace(t, b)

	writeWorkspaceFile(t, ws.Root(), "f.txt", "B")
	_, err := ws.Commit(ctx, CommitRequest{Message: "f=B", Author: author})
	require.NoError(t, err)

	// Local edits to g.txt block the refresh, and another agent changes it.
	writeWorkspaceFile(t, ws.Root(), "g.txt", "G1")
	b.Put("main", "g.txt", []byte("G2"))

	_, err = ws.Land(ctx, "", "", author)
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	require.NotNil(t, ws.Summary().Pending, "the landed commit stays pending until the refresh succeeds")

	_, err = ws.Commit(ctx, CommitRequest{Message: "f=C", Author: author})
	require.ErrorContains(t, err, "pending")

	writeWorkspaceFile(t, ws.Root(), "g.txt", "G0")
	_, err = ws.Land(ctx, "", "", author)
	require.NoError(t, err)
	require.Nil(t, ws.Summary().Pending)
	require.Equal(t, "B", readRemote(t, b, "main", "f.txt"))
	require.Equal(t, "G2", readWorkspaceFile(t, ws.Root(), "g.txt"))

	writeWorkspaceFile(t, ws.Root(), "f.txt", "C")
	_, err = ws.Commit(ctx, CommitRequest{Message: "f=C", Author: author, LandBranch: "main"})
	require.NoError(t, err)
	require.Equal(t, "C", readRemote(t, b, "main", "f.txt"))
}

func TestStatusRecordsStatsOfVerifiedFiles(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"same"})
	ws := cloneTestWorkspace(t, b)

	path := filepath.Join(ws.Root(), "a.txt")
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(path, old, old))
	info, err := os.Stat(path)
	require.NoError(t, err)

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
	require.Equal(t, info.ModTime().UnixNano(), ws.state.Files["a.txt"].MTimeNS, "the verified stats are recorded")

	reopened, err := Open(ws.Root(), b)
	require.NoError(t, err)
	require.Equal(t, info.ModTime().UnixNano(), reopened.state.Files["a.txt"].MTimeNS)
}

func TestCloneSkipsNestedGraftStateSoFindIgnoresIt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	planted := `{"version":1,"address":"tcp://attacker.example:9999","storage":"default","repo":"evil.git","branch":"main","files":{}}`
	b := seedBackend(t,
		[]string{"sub/code.go", "sub/.graft/state.json", "tools/.GRAFT/x.txt", "a.txt"},
		[]string{"package sub", planted, "nested", "top"})
	ws := cloneTestWorkspace(t, b)

	require.Equal(t, "package sub", readWorkspaceFile(t, ws.Root(), "sub/code.go"))
	require.NoDirExists(t, filepath.Join(ws.Root(), "sub", ".graft"))
	require.NoDirExists(t, filepath.Join(ws.Root(), "tools"))
	skipped := ws.Skipped()
	require.Len(t, skipped, 2)
	require.Contains(t, skipped[0]+skipped[1], `"sub/.graft/state.json"`)
	require.Contains(t, skipped[0]+skipped[1], `"tools/.GRAFT/x.txt"`)

	found, err := Find(filepath.Join(ws.Root(), "sub"))
	require.NoError(t, err)
	require.Equal(t, ws.Root(), found, "commands run in sub/ use the enclosing workspace")
	meta, err := ReadMeta(found)
	require.NoError(t, err)
	require.Equal(t, "memory", meta.Address)
	require.Equal(t, "repo", meta.Repo)

	// A nested metadata directory written locally is ignored, not committed.
	writeWorkspaceFile(t, ws.Root(), "sub/.graft/state.json", planted)
	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)

	// Commits leave the skipped paths on the server unchanged.
	writeWorkspaceFile(t, ws.Root(), "a.txt", "top2")
	_, err = ws.Commit(ctx, CommitRequest{Message: "a", Author: Signature{Name: "dev"}, LandBranch: "main"})
	require.NoError(t, err)
	require.Equal(t, planted, readRemote(t, b, "main", "sub/.graft/state.json"))
	require.Equal(t, "nested", readRemote(t, b, "main", "tools/.GRAFT/x.txt"))
}

// TestLandRefreshSkipsPathsUnsafeOnThisSystem covers a land whose refresh meets
// a path another agent added that the workspace cannot write: the refresh
// leaves it out instead of failing, so the workspace is not stuck with a landed
// commit that every command refuses to get past.
func TestLandRefreshSkipsPathsUnsafeOnThisSystem(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := seedBackend(t, []string{"ok.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)

	// ':' is not safe on Windows. Elsewhere it is a valid file name character,
	// and a reserved path stands in for an unsafe one.
	unsafe, skipped := "a:b.txt", 2
	if runtime.GOOS != "windows" {
		unsafe, skipped = "x/.git/hooks/pre-commit", 1
	}
	b.Put("main", unsafe, []byte("other agent"))
	b.Put("main", "docs/c:d.txt", []byte("colon"))

	writeWorkspaceFile(t, ws.Root(), "ok.txt", "two")
	res, err := ws.Commit(ctx, CommitRequest{Message: "change", Author: Signature{Name: "dev"}, LandBranch: "main"})
	require.NoError(t, err)
	require.True(t, res.Landed)
	require.Nil(t, ws.Summary().Pending)
	require.Len(t, ws.Skipped(), skipped)
	require.Contains(t, ws.Skipped()[0], unsafe)
	require.NoFileExists(t, filepath.Join(ws.Root(), filepath.FromSlash(unsafe)))
	if runtime.GOOS != "windows" {
		require.Equal(t, "colon", readWorkspaceFile(t, ws.Root(), "docs/c:d.txt"))
	}

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
	_, err = ws.Update(ctx)
	require.NoError(t, err)
	_, err = Clone(ctx, filepath.Join(t.TempDir(), "again"), b, CloneOptions{Branch: "main"})
	require.NoError(t, err, "a repository with such paths can be cloned")
}

// TestSparseFinishesRefreshThatKeepsFailing shows the way out of a refresh after
// a land that fails on some path every time: graft sparse refreshes the
// workspace to the landed commit with a sparse set that leaves the path out.
func TestSparseFinishesRefreshThatKeepsFailing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	author := Signature{Name: "dev"}
	b := seedBackend(t, []string{"a/x.txt", "b/y.txt"}, []string{"x0", "y0"})
	ws := cloneTestWorkspace(t, b)

	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "x1")
	_, err := ws.Commit(ctx, CommitRequest{Message: "x", Author: author})
	require.NoError(t, err)

	// Another agent changes b/y.txt, and the workspace cannot fetch any object:
	// the refresh fails with an error that no local change can resolve.
	b.Put("main", "b/y.txt", []byte("y1"))
	broken, err := Open(ws.Root(), failingObjectsBackend{Backend: b})
	require.NoError(t, err)
	_, err = broken.Land(ctx, "", "", author)
	require.ErrorContains(t, err, "refreshing the workspace failed")
	require.ErrorContains(t, err, "graft sparse")
	require.NotContains(t, err.Error(), "revert or restore the conflicting local changes")
	landed := broken.Summary().Pending.Landed
	require.NotEmpty(t, landed)

	_, err = broken.Land(ctx, "", "", author)
	require.Error(t, err, "the retry fails the same way")
	require.ErrorContains(t, broken.Abort(ctx), "graft sparse")

	// Leaving b/ out of the sparse set finishes the refresh: nothing has to be
	// fetched, since the landed a/x.txt is in the local cache.
	require.NoError(t, broken.Sparse(ctx, []string{"a"}))
	require.Nil(t, broken.Summary().Pending)
	require.Equal(t, landed, broken.Summary().Base)
	require.Equal(t, []string{"a"}, broken.Summary().Sparse)
	require.NoDirExists(t, filepath.Join(ws.Root(), "b"))
	require.Equal(t, "x1", readWorkspaceFile(t, ws.Root(), "a/x.txt"))

	// With a working connection the whole repository comes back.
	ws, err = Open(ws.Root(), b)
	require.NoError(t, err)
	require.NoError(t, ws.Sparse(ctx, nil))
	require.Equal(t, "y1", readWorkspaceFile(t, ws.Root(), "b/y.txt"))
	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "x2")
	_, err = ws.Commit(ctx, CommitRequest{Message: "x2", Author: author, LandBranch: "main"})
	require.NoError(t, err)
	require.Equal(t, "x2", readRemote(t, b, "main", "a/x.txt"))
}

func TestTemporaryBranchKeepsValidUserNames(t *testing.T) {
	t.Parallel()

	for user, owner := range map[string]string{
		"build_bot": "build_bot",
		"Planner":   "Planner",
		"ci.bot":    "ci.bot",
		"alice":     "alice",
		"agent-7":   "agent-7",
		// Names that are not valid reference name components become a slug.
		"Agent One": "agent-one",
		".hidden":   "hidden",
		"a..b":      "a-b",
		"x.lock":    "x-lock",
		"a/b":       "a-b",
		"a:b":       "a-b",
		"trail.":    "trail",
		"@":         "agent",
		"a@{b":      "a-b",
		"":          "agent",
	} {
		branch, err := temporaryBranch(user)
		require.NoError(t, err, user)
		require.Regexp(t, `^graft/`+regexp.QuoteMeta(owner)+`/[0-9a-f]{16}$`, branch, user)
	}
}

func TestCommitUsesUserVerbatimInTemporaryBranch(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a.txt", "two")

	res, err := ws.Commit(context.Background(), CommitRequest{Message: "two", Author: Signature{Name: "Build Bot"}, User: "build_bot"})
	require.NoError(t, err)
	require.Regexp(t, `^graft/build_bot/[0-9a-f]{16}$`, res.Branch, "a policy on graft/build_bot/** matches")
}

func TestDiffWithBinaryFile(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"bin.dat"}, []string{"\x00\x01"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "bin.dat", "\x00\x02\x03")

	diff, err := ws.Diff()
	require.NoError(t, err)
	require.Contains(t, diff, "Binary files differ")
}
