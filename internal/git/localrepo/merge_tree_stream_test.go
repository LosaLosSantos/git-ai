package localrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// requireSameMergeResult asserts that a merge through MergeTreeStream returned exactly what
// MergeTree returned: the same tree, and for conflicts the same *MergeTreeConflictError.
func requireSameMergeResult(t *testing.T, expectedOID git.ObjectID, expectedErr error, actualOID git.ObjectID, actualErr error) {
	t.Helper()

	require.Equal(t, expectedOID, actualOID)

	var expectedConflict *MergeTreeConflictError
	if errors.As(expectedErr, &expectedConflict) {
		var actualConflict *MergeTreeConflictError
		require.True(t, errors.As(actualErr, &actualConflict), "expected conflict error, got %v", actualErr)
		require.Equal(t, expectedConflict, actualConflict)
		return
	}

	require.Equal(t, expectedErr, actualErr)
}

func TestMergeTreeStream_MatchesMergeTree(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)

	const longContent = "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nline 7\nline 8\nline 9\nline 10\n"

	type setupData struct {
		base   git.ObjectID
		ours   git.ObjectID
		theirs git.ObjectID
		// extraOpts are the MergeTree options that correspond to the base given to the stream.
		extraOpts []MergeTreeOption
	}

	testCases := []struct {
		desc  string
		setup func(t *testing.T, repoPath string) setupData
	}{
		{
			desc: "clean merge computes the merge base",
			setup: func(t *testing.T, repoPath string) setupData {
				base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
				))
				ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
					gittest.TreeEntry{Mode: "100644", Path: "file2", Content: "ours\n"},
				))
				theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
					gittest.TreeEntry{Mode: "100644", Path: "file3", Content: "theirs\n"},
				))
				return setupData{base: "", ours: ours, theirs: theirs}
			},
		},
		{
			desc: "clean merge with explicit merge base",
			setup: func(t *testing.T, repoPath string) setupData {
				base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
				))
				ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
					gittest.TreeEntry{Mode: "100644", Path: "file2", Content: "ours\n"},
				))
				theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
					gittest.TreeEntry{Mode: "100644", Path: "file3", Content: "theirs\n"},
				))
				return setupData{
					base:      base,
					ours:      ours,
					theirs:    theirs,
					extraOpts: []MergeTreeOption{WithMergeBase(git.Revision(base.String()))},
				}
			},
		},
		{
			desc: "conflicting merge",
			setup: func(t *testing.T, repoPath string) setupData {
				base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
				))
				ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "ours\n"},
				))
				theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "theirs\n"},
				))
				return setupData{ours: ours, theirs: theirs}
			},
		},
		{
			desc: "conflicting merge with explicit merge base",
			setup: func(t *testing.T, repoPath string) setupData {
				base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
				))
				ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "ours\n"},
				))
				theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "theirs\n"},
					gittest.TreeEntry{Mode: "100644", Path: "file2", Content: "theirs\n"},
				))
				return setupData{
					base:      base,
					ours:      ours,
					theirs:    theirs,
					extraOpts: []MergeTreeOption{WithMergeBase(git.Revision(base.String()))},
				}
			},
		},
		{
			desc: "unrelated histories without conflicts",
			setup: func(t *testing.T, repoPath string) setupData {
				ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "ours\n"},
				))
				theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file2", Content: "theirs\n"},
				))
				return setupData{ours: ours, theirs: theirs}
			},
		},
		{
			desc: "unrelated histories with conflicts",
			setup: func(t *testing.T, repoPath string) setupData {
				ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "ours\n"},
				))
				theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "theirs\n"},
				))
				return setupData{ours: ours, theirs: theirs}
			},
		},
		{
			desc: "rename detected on ours",
			setup: func(t *testing.T, repoPath string) setupData {
				base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: longContent},
				))
				ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "renamed", Content: longContent},
				))
				theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: longContent + "line 11\n"},
				))
				return setupData{ours: ours, theirs: theirs}
			},
		},
		{
			desc: "mode change on ours",
			setup: func(t *testing.T, repoPath string) setupData {
				base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
					gittest.TreeEntry{Mode: "100644", Path: "file2", Content: "two\n"},
				))
				ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100755", Path: "file1", Content: "one\n"},
					gittest.TreeEntry{Mode: "100644", Path: "file2", Content: "two\n"},
				))
				theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
					gittest.TreeEntry{Mode: "100644", Path: "file2", Content: "changed\n"},
				))
				return setupData{ours: ours, theirs: theirs}
			},
		},
		{
			desc: "mode change on both sides conflicts",
			setup: func(t *testing.T, repoPath string) setupData {
				base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
				))
				ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "100755", Path: "file1", Content: "one\n"},
				))
				theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
					gittest.TreeEntry{Mode: "120000", Path: "file1", Content: "one\n"},
				))
				return setupData{ours: ours, theirs: theirs}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)

			repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})
			repo := NewTestRepo(t, cfg, repoProto)

			data := tc.setup(t, repoPath)

			expectedOID, expectedErr := repo.MergeTree(ctx, data.ours.String(), data.theirs.String(),
				append([]MergeTreeOption{WithAllowUnrelatedHistories()}, data.extraOpts...)...)

			stream, err := repo.NewMergeTreeStream(ctx)
			require.NoError(t, err)
			defer func() { require.NoError(t, stream.Close()) }()

			actualOID, actualErr := stream.Merge(ctx, data.base, data.ours, data.theirs)
			requireSameMergeResult(t, expectedOID, expectedErr, actualOID, actualErr)
		})
	}
}

func TestMergeTreeStream_ManyMergesOneProcess(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	ctx := testhelper.Context(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := NewTestRepo(t, cfg, repoProto)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "shared", Content: "base\n"},
	))

	stream, err := repo.NewMergeTreeStream(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, stream.Close()) }()

	// Alternate clean and conflicting merges through the same process. A conflict record
	// must not desynchronize the following records.
	for i := 0; i < 30; i++ {
		oursShared := "base\n"
		theirsShared := "base\n"
		if i%2 == 1 {
			oursShared = fmt.Sprintf("ours %d\n", i)
			theirsShared = fmt.Sprintf("theirs %d\n", i)
		}

		ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "shared", Content: oursShared},
			gittest.TreeEntry{Mode: "100644", Path: fmt.Sprintf("ours-%d", i), Content: "ours\n"},
		))
		theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "shared", Content: theirsShared},
			gittest.TreeEntry{Mode: "100644", Path: fmt.Sprintf("theirs-%d", i), Content: "theirs\n"},
		))

		expectedOID, expectedErr := repo.MergeTree(ctx, ours.String(), theirs.String(), WithAllowUnrelatedHistories())
		actualOID, actualErr := stream.Merge(ctx, "", ours, theirs)
		requireSameMergeResult(t, expectedOID, expectedErr, actualOID, actualErr)
	}

	// Nothing the process caches changed, so it was never restarted.
	require.Equal(t, 1, stream.starts)
}

// A git process caches the gitattributes and the configuration it read first. The stream
// must still return what MergeTree returns after they change.
func TestMergeTreeStream_FollowsAttributeAndConfigChanges(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)

	testCases := []struct {
		desc   string
		change func(t *testing.T, repoPath string, base git.ObjectID)
	}{
		{
			desc: "info/attributes",
			change: func(t *testing.T, repoPath string, base git.ObjectID) {
				require.NoError(t, os.MkdirAll(filepath.Join(repoPath, "info"), mode.Directory))
				require.NoError(t, os.WriteFile(filepath.Join(repoPath, "info", "attributes"),
					[]byte("a.txt merge=union\n"), mode.File))
			},
		},
		{
			desc: ".gitattributes of HEAD",
			change: func(t *testing.T, repoPath string, base git.ObjectID) {
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithParents(base),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Mode: "100644", Path: ".gitattributes", Content: "a.txt merge=union\n"},
						gittest.TreeEntry{Mode: "100644", Path: "a.txt", Content: "1\n"},
					))
			},
		},
		{
			desc: "repository config",
			change: func(t *testing.T, repoPath string, base git.ObjectID) {
				gittest.Exec(t, cfg, "-C", repoPath, "config", "merge.conflictStyle", "diff3")
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)

			repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})
			repo := NewTestRepo(t, cfg, repoProto)

			gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "HEAD", "refs/heads/main")
			base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithTreeEntries(
				gittest.TreeEntry{Mode: "100644", Path: "a.txt", Content: "1\n"},
			))
			ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
				gittest.TreeEntry{Mode: "100644", Path: "a.txt", Content: "2\n"},
			))
			theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
				gittest.TreeEntry{Mode: "100644", Path: "a.txt", Content: "3\n"},
			))

			mergeTree := func() (git.ObjectID, error) {
				return repo.MergeTree(ctx, ours.String(), theirs.String(),
					WithAllowUnrelatedHistories(), WithMergeBase(git.Revision(base.String())))
			}

			stream, err := repo.NewMergeTreeStream(ctx)
			require.NoError(t, err)
			defer func() { require.NoError(t, stream.Close()) }()

			// The first merge makes the process read the attributes and the configuration.
			beforeOID, beforeErr := mergeTree()
			require.Error(t, beforeErr)
			actualOID, actualErr := stream.Merge(ctx, base, ours, theirs)
			requireSameMergeResult(t, beforeOID, beforeErr, actualOID, actualErr)

			tc.change(t, repoPath, base)

			afterOID, afterErr := mergeTree()
			require.NotEqual(t, beforeOID, afterOID, "the change must change the merge result")

			actualOID, actualErr = stream.Merge(ctx, base, ours, theirs)
			requireSameMergeResult(t, afterOID, afterErr, actualOID, actualErr)
			require.Equal(t, 2, stream.starts)

			// Without a further change the new process is kept.
			actualOID, actualErr = stream.Merge(ctx, base, ours, theirs)
			requireSameMergeResult(t, afterOID, afterErr, actualOID, actualErr)
			require.Equal(t, 2, stream.starts)
		})
	}
}

func TestMergeTreeStream_SeesObjectsWrittenAfterStart(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	ctx := testhelper.Context(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := NewTestRepo(t, cfg, repoProto)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
	))
	ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
		gittest.TreeEntry{Mode: "100644", Path: "file2", Content: "ours\n"},
	))

	stream, err := repo.NewMergeTreeStream(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, stream.Close()) }()

	// The stream is already running when these commits are written by another process
	// (git commit-tree writes loose objects).
	theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "one\n"},
		gittest.TreeEntry{Mode: "100644", Path: "file3", Content: "theirs\n"},
	))

	expectedOID, expectedErr := repo.MergeTree(ctx, ours.String(), theirs.String(), WithAllowUnrelatedHistories())
	require.NoError(t, expectedErr)

	actualOID, actualErr := stream.Merge(ctx, "", ours, theirs)
	require.NoError(t, actualErr)
	require.Equal(t, expectedOID, actualOID)
	gittest.RequireTree(t, cfg, repoPath, actualOID.String(), []gittest.TreeEntry{
		{Mode: "100644", Path: "file1", Content: "one\n"},
		{Mode: "100644", Path: "file2", Content: "ours\n"},
		{Mode: "100644", Path: "file3", Content: "theirs\n"},
	})
}

func TestMergeTreeStream_InvalidInputKeepsStreamUsable(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	ctx := testhelper.Context(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := NewTestRepo(t, cfg, repoProto)

	ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "ours\n"},
	))
	theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "file2", Content: "theirs\n"},
	))

	stream, err := repo.NewMergeTreeStream(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, stream.Close()) }()

	_, err = stream.Merge(ctx, "", git.ObjectID("not-an-oid"), theirs)
	require.Error(t, err)

	_, err = stream.Merge(ctx, "", "", theirs)
	require.Error(t, err)

	// Input rejected before it reached git must not break the stream.
	actualOID, err := stream.Merge(ctx, "", ours, theirs)
	require.NoError(t, err)

	expectedOID, expectedErr := repo.MergeTree(ctx, ours.String(), theirs.String(), WithAllowUnrelatedHistories())
	require.NoError(t, expectedErr)
	require.Equal(t, expectedOID, actualOID)
}

func TestMergeTreeStream_FatalInputBreaksStream(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	ctx := testhelper.Context(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := NewTestRepo(t, cfg, repoProto)

	ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "ours\n"},
	))
	missing := git.ObjectID(strings.Repeat("1", len(ours.String())))

	stream, err := repo.NewMergeTreeStream(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, stream.Close()) }()

	// git aborts on a missing object, which ends the process.
	_, err = stream.Merge(ctx, "", missing, ours)
	require.Error(t, err)

	// The stream is broken from now on.
	_, err = stream.Merge(ctx, "", ours, ours)
	require.Error(t, err)
}

func TestMergeTreeStream_ContextCancellationEndsProcess(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	ctx := testhelper.Context(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := NewTestRepo(t, cfg, repoProto)

	ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "ours\n"},
	))
	theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "file2", Content: "theirs\n"},
	))

	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := repo.NewMergeTreeStream(streamCtx)
	require.NoError(t, err)
	defer func() { require.NoError(t, stream.Close()) }()

	cancel()
	// Wait until the process is gone so that the merge cannot race with the kill.
	_ = stream.cmd.Wait()

	_, err = stream.Merge(ctx, "", ours, theirs)
	require.Error(t, err)
}

func TestMergeTreeStream_Close(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	ctx := testhelper.Context(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := NewTestRepo(t, cfg, repoProto)

	ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "file1", Content: "ours\n"},
	))

	stream, err := repo.NewMergeTreeStream(ctx)
	require.NoError(t, err)

	require.NoError(t, stream.Close())
	require.NoError(t, stream.Close())

	_, err = stream.Merge(ctx, "", ours, ours)
	require.Error(t, err)
}
