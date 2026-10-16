package localrepo

// Black-box spec tests for WP32 (streaming merge-tree). Written from
// doc/graft/PLAN4.md only; the implementation was not consulted.
// Import paths for the Gitaly test helpers are assumed to follow the
// usual layout (internal/testhelper, internal/testhelper/testcfg,
// internal/git/gittest).

import (
	"context"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// specFiles builds a list of regular-file tree entries.
func specFiles(files map[string]string) []gittest.TreeEntry {
	entries := make([]gittest.TreeEntry, 0, len(files))
	for name, content := range files {
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: name, Content: content})
	}
	return entries
}

// specCommit writes a commit with the given tree entries and parents.
func specCommit(t *testing.T, cfg config.Cfg, repoPath string, parents []git.ObjectID, entries []gittest.TreeEntry) git.ObjectID {
	t.Helper()
	opts := []gittest.WriteCommitOption{gittest.WithTreeEntries(entries...)}
	if len(parents) > 0 {
		opts = append(opts, gittest.WithParents(parents...))
	}
	return gittest.WriteCommit(t, cfg, repoPath, opts...)
}

// specAssertSameAsMergeTree checks that s.Merge returns exactly what
// (*Repo).MergeTree returns for the same inputs: the same tree OID for a
// clean merge, or the same *MergeTreeConflictError for a conflict.
func specAssertSameAsMergeTree(t *testing.T, ctx context.Context, repo *Repo, s *MergeTreeStream, base, ours, theirs git.ObjectID) {
	t.Helper()

	var mtOpts []MergeTreeOption
	if base != "" {
		mtOpts = append(mtOpts, WithMergeBase(base.Revision()))
	}
	wantTree, wantErr := repo.MergeTree(ctx, ours.String(), theirs.String(), mtOpts...)

	gotTree, gotErr := s.Merge(ctx, base, ours, theirs)

	if wantErr != nil {
		var wantConflict *MergeTreeConflictError
		require.ErrorAs(t, wantErr, &wantConflict, "MergeTree must report a conflict here")

		var gotConflict *MergeTreeConflictError
		require.ErrorAs(t, gotErr, &gotConflict, "stream must report the same conflict type")
		require.Equal(t, wantConflict, gotConflict, "conflict contents must match MergeTree")
		return
	}

	require.NoError(t, gotErr)
	require.Equal(t, wantTree, gotTree, "clean merge must return the same tree OID as MergeTree")
}

// TestSpecMergeTreeStreamCleanAndConflictMatchMergeTree covers the basic
// equivalence promise for a clean merge and a conflicting merge, with an
// explicit merge base and with base "" (git computes it).
func TestSpecMergeTreeStreamCleanAndConflictMatchMergeTree(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := NewTestRepo(t, cfg, repoProto)

	base := specCommit(t, cfg, repoPath, nil, specFiles(map[string]string{"a": "1\n", "b": "1\n"}))
	ours := specCommit(t, cfg, repoPath, []git.ObjectID{base}, specFiles(map[string]string{"a": "2\n", "b": "1\n"}))
	theirs := specCommit(t, cfg, repoPath, []git.ObjectID{base}, specFiles(map[string]string{"a": "1\n", "b": "3\n"}))
	conflictTheirs := specCommit(t, cfg, repoPath, []git.ObjectID{base}, specFiles(map[string]string{"a": "9\n", "b": "1\n"}))

	s, err := repo.NewMergeTreeStream(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Clean merge, explicit base.
	specAssertSameAsMergeTree(t, ctx, repo, s, base, ours, theirs)
	// Clean merge, base computed by git.
	specAssertSameAsMergeTree(t, ctx, repo, s, "", ours, theirs)
	// Conflicting merge (both sides changed "a" differently).
	specAssertSameAsMergeTree(t, ctx, repo, s, base, ours, conflictTheirs)
}

// TestSpecMergeTreeStreamManyMergesOneStream checks that one long-lived
// stream handles many merges in sequence, including conflicts in the middle,
// and keeps returning MergeTree's answers afterwards.
func TestSpecMergeTreeStreamManyMergesOneStream(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := NewTestRepo(t, cfg, repoProto)

	base := specCommit(t, cfg, repoPath, nil, specFiles(map[string]string{"f": "0\n"}))

	s, err := repo.NewMergeTreeStream(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < 20; i++ {
		ours := specCommit(t, cfg, repoPath, []git.ObjectID{base}, specFiles(map[string]string{"f": "0\n", "o": string(rune('a' + i))}))
		var theirsContent string
		if i%3 == 0 {
			theirsContent = "conflict-" + string(rune('a'+i)) + "\n"
		} else {
			theirsContent = "0\n"
		}
		theirs := specCommit(t, cfg, repoPath, []git.ObjectID{base}, specFiles(map[string]string{"f": theirsContent, "t": "x\n"}))
		oursFixed := specCommit(t, cfg, repoPath, []git.ObjectID{base}, specFiles(map[string]string{"f": "1\n"}))

		specAssertSameAsMergeTree(t, ctx, repo, s, base, ours, theirs)
		specAssertSameAsMergeTree(t, ctx, repo, s, base, oursFixed, theirs)
	}
}

// TestSpecMergeTreeStreamSeesLaterLooseObjects checks that objects written by
// other processes after the stream started are visible to later merges.
func TestSpecMergeTreeStreamSeesLaterLooseObjects(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := NewTestRepo(t, cfg, repoProto)

	base := specCommit(t, cfg, repoPath, nil, specFiles(map[string]string{"a": "1\n"}))

	s, err := repo.NewMergeTreeStream(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Written only after the stream is running.
	late := specCommit(t, cfg, repoPath, []git.ObjectID{base}, specFiles(map[string]string{"a": "1\n", "late": "yes\n"}))
	lateOurs := specCommit(t, cfg, repoPath, []git.ObjectID{base}, specFiles(map[string]string{"a": "2\n"}))

	specAssertSameAsMergeTree(t, ctx, repo, s, base, late, lateOurs)
}

// TestSpecMergeTreeStreamRenameAndModeChange checks that rename detection and
// mode changes give the same result as MergeTree.
func TestSpecMergeTreeStreamRenameAndModeChange(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := NewTestRepo(t, cfg, repoProto)

	content := "line one\nline two\nline three\nline four\n"
	base := specCommit(t, cfg, repoPath, nil, specFiles(map[string]string{"old.txt": content}))

	// ours renames old.txt to new.txt.
	renamed := specCommit(t, cfg, repoPath, []git.ObjectID{base}, specFiles(map[string]string{"new.txt": content}))
	// theirs changes the mode of old.txt only.
	modeChanged := specCommit(t, cfg, repoPath, []git.ObjectID{base}, []gittest.TreeEntry{
		{Mode: "100755", Path: "old.txt", Content: content},
	})

	s, err := repo.NewMergeTreeStream(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	specAssertSameAsMergeTree(t, ctx, repo, s, base, renamed, modeChanged)
	specAssertSameAsMergeTree(t, ctx, repo, s, base, modeChanged, renamed)
}

// TestSpecMergeTreeStreamCloseEndsProcess checks that after Close the stream
// cannot run further merges.
func TestSpecMergeTreeStreamCloseEndsProcess(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := NewTestRepo(t, cfg, repoProto)

	base := specCommit(t, cfg, repoPath, nil, specFiles(map[string]string{"a": "1\n"}))
	ours := specCommit(t, cfg, repoPath, []git.ObjectID{base}, specFiles(map[string]string{"a": "2\n"}))

	s, err := repo.NewMergeTreeStream(ctx)
	require.NoError(t, err)

	specAssertSameAsMergeTree(t, ctx, repo, s, base, ours, base)

	_ = s.Close()

	_, err = s.Merge(ctx, base, ours, base)
	require.Error(t, err, "Merge after Close must fail")
}

// TestSpecMergeTreeStreamContextCancelEndsStream checks that cancelling the
// context the stream was started with ends it: later merges fail.
func TestSpecMergeTreeStreamContextCancelEndsStream(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := NewTestRepo(t, cfg, repoProto)

	base := specCommit(t, cfg, repoPath, nil, specFiles(map[string]string{"a": "1\n"}))
	ours := specCommit(t, cfg, repoPath, []git.ObjectID{base}, specFiles(map[string]string{"a": "2\n"}))

	streamCtx, cancel := context.WithCancel(ctx)
	s, err := repo.NewMergeTreeStream(streamCtx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	cancel()

	_, err = s.Merge(streamCtx, base, ours, base)
	require.Error(t, err, "Merge after context cancellation must fail")
}
