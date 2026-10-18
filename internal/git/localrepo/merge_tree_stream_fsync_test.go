package localrepo

import (
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// effectiveConfig returns the value that git uses for key from the -c switches of args: the last
// one wins.
func effectiveConfig(args []string, key string) (string, bool) {
	value, found := "", false
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-c" {
			continue
		}
		if v, ok := strings.CutPrefix(args[i+1], key+"="); ok {
			value, found = v, true
		}
	}
	return value, found
}

func TestHelpersWithoutObjectFsync(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := NewTestRepo(t, cfg, repoProto)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base\n"},
	))
	ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base\n"},
		gittest.TreeEntry{Mode: "100644", Path: "ours", Content: "ours\n"},
	))
	theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base\n"},
		gittest.TreeEntry{Mode: "100644", Path: "theirs", Content: "theirs\n"},
	))

	t.Run("merge-tree stream", func(t *testing.T) {
		t.Parallel()

		synced, err := repo.NewMergeTreeStream(ctx)
		require.NoError(t, err)
		defer func() { require.NoError(t, synced.Close()) }()

		unsynced, err := repo.NewMergeTreeStream(ctx, WithoutObjectFsync())
		require.NoError(t, err)
		defer func() { require.NoError(t, unsynced.Close()) }()

		// The server's configuration fsyncs objects; the option replaces it for this helper only.
		value, found := effectiveConfig(synced.cmd.Args(), "core.fsync")
		require.True(t, found)
		require.Contains(t, value, "objects")

		value, found = effectiveConfig(unsynced.cmd.Args(), "core.fsync")
		require.True(t, found)
		require.Equal(t, "none", value)

		// Only durability changes: the merge is the same.
		wantOID, wantErr := synced.Merge(ctx, base, ours, theirs)
		gotOID, gotErr := unsynced.Merge(ctx, base, ours, theirs)
		requireSameMergeResult(t, wantOID, wantErr, gotOID, gotErr)
	})

	t.Run("commit writer", func(t *testing.T) {
		t.Parallel()

		synced, err := repo.NewCommitWriter(ctx)
		require.NoError(t, err)
		defer func() { require.NoError(t, synced.Close()) }()

		unsynced, err := repo.NewCommitWriter(ctx, WithoutObjectFsync())
		require.NoError(t, err)
		defer func() { require.NoError(t, unsynced.Close()) }()

		value, found := effectiveConfig(synced.process.cmd.Args(), "core.fsync")
		require.True(t, found)
		require.Contains(t, value, "objects")

		value, found = effectiveConfig(unsynced.process.cmd.Args(), "core.fsync")
		require.True(t, found)
		require.Equal(t, "none", value)

		when := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
		writeCfg := WriteCommitConfig{
			TreeID:         gittest.ResolveRevision(t, cfg, repoPath, ours.String()+"^{tree}"),
			Parents:        []git.ObjectID{base},
			AuthorName:     "Ada",
			AuthorEmail:    "ada@example.com",
			AuthorDate:     when,
			CommitterName:  "Ada",
			CommitterEmail: "ada@example.com",
			CommitterDate:  when,
			Message:        "same commit\n",
		}

		want, err := synced.Write(ctx, writeCfg)
		require.NoError(t, err)
		got, err := unsynced.Write(ctx, writeCfg)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})
}
