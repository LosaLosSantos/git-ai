package gitops_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/stretchr/testify/require"
)

func TestBuildPackFromIsSelfContained(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			source := newFixture(t, format)
			c1 := source.commit(t, nil, "first")
			gittest.WriteRef(t, source.cfg, source.path, "refs/heads/main", c1)
			c2 := source.commit(t, []git.ObjectID{c1}, "second")

			// Unlike BuildPack, the objects that a reference already reaches (c1)
			// are included: the pack is the whole closure of the tips.
			pack, err := gitops.BuildPackFrom(source.ctx, source.repo, []git.ObjectID{c2, source.hash.ZeroOID}, t.TempDir())
			require.NoError(t, err)
			require.Equal(t, 6, pack.Objects, "two commits, two trees and two blobs")
			data := readPack(t, pack)

			target := newFixture(t, format)
			require.NoError(t, gitops.IndexPack(target.ctx, target.repo, bytes.NewReader(data)))
			require.NoError(t, gitops.ApplyRefs(target.ctx, target.repo, map[string]corestore.Target{
				"refs/heads/main": corestore.Target(c2.String()),
			}))
			gittest.Exec(t, target.cfg, "-C", target.path, "fsck", "--connectivity-only")
		})
	}
}

func TestBuildPackFromWithoutTips(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "sha1")
	for _, tips := range [][]git.ObjectID{nil, {f.hash.ZeroOID}} {
		pack, err := gitops.BuildPackFrom(f.ctx, f.repo, tips, t.TempDir())
		require.NoError(t, err)
		require.True(t, pack.Empty())
	}
}

func TestBuildPackFromRejectsBadTips(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "sha1")

	_, err := gitops.BuildPackFrom(f.ctx, f.repo, []git.ObjectID{"--all"}, t.TempDir())
	require.Error(t, err, "a tip that is not an object ID")

	absent := git.ObjectID(strings.Repeat("ab", 20))
	_, err = gitops.BuildPackFrom(f.ctx, f.repo, []git.ObjectID{absent}, t.TempDir())
	require.Error(t, err, "a tip that is not in the repository")
}

func TestMissingObjects(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, format)
			c1 := f.commit(t, nil, "present")
			blob := gittest.WriteBlob(t, f.cfg, f.path, []byte("a blob"))
			absent1 := git.ObjectID(strings.Repeat("1", f.hash.EncodedLen()))
			absent2 := git.ObjectID(strings.Repeat("2", f.hash.EncodedLen()))

			missing, err := gitops.MissingObjects(f.ctx, f.repo, []git.ObjectID{absent2, c1, blob, absent1})
			require.NoError(t, err)
			require.Equal(t, []git.ObjectID{absent2, absent1}, missing, "in the order given")

			missing, err = gitops.MissingObjects(f.ctx, f.repo, []git.ObjectID{c1})
			require.NoError(t, err)
			require.Empty(t, missing)

			missing, err = gitops.MissingObjects(f.ctx, f.repo, nil)
			require.NoError(t, err)
			require.Empty(t, missing)

			_, err = gitops.MissingObjects(f.ctx, f.repo, []git.ObjectID{"HEAD"})
			require.Error(t, err, "only object IDs are looked up")
		})
	}
}
