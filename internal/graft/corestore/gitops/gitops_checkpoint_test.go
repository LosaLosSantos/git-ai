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

// tagObject writes an annotated tag object of target, not referenced.
func (f fixture) tagObject(t *testing.T, target git.ObjectID) git.ObjectID {
	t.Helper()

	mktag := gittest.NewCommand(t, f.cfg, "-C", f.path, "mktag")
	mktag.Stdin = strings.NewReader("object " + target.String() + "\ntype commit\ntag v1\ntagger T <t@example.com> 1700000000 +0000\n\nv1\n")
	out, err := mktag.Output()
	require.NoError(t, err)
	return git.ObjectID(strings.TrimSpace(string(out)))
}

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

func TestIsReachable(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, format)
			c1 := f.commit(t, nil, "base")
			c2 := f.commit(t, []git.ObjectID{c1}, "tip")
			unrelated := f.commit(t, nil, "unrelated")
			tag := f.tagObject(t, c2)
			blob := gittest.WriteBlob(t, f.cfg, f.path, []byte("a reference may point to a blob"))

			for _, tc := range []struct {
				desc   string
				commit git.ObjectID
				tips   []git.ObjectID
				want   bool
			}{
				{desc: "ancestor of a tip", commit: c1, tips: []git.ObjectID{c2}, want: true},
				{desc: "the tip itself", commit: c2, tips: []git.ObjectID{c2}, want: true},
				{desc: "through an annotated tag", commit: c1, tips: []git.ObjectID{tag}, want: true},
				{desc: "next to a tip that is a blob", commit: c1, tips: []git.ObjectID{blob, c2}, want: true},
				{desc: "unrelated history", commit: unrelated, tips: []git.ObjectID{c2, tag, blob}, want: false},
				{desc: "a descendant is not reachable from its ancestor", commit: c2, tips: []git.ObjectID{c1}, want: false},
				{desc: "no tips", commit: c1, tips: nil, want: false},
				{desc: "zero tips are ignored", commit: c1, tips: []git.ObjectID{f.hash.ZeroOID}, want: false},
			} {
				got, err := gitops.IsReachable(f.ctx, f.repo, tc.commit, tc.tips)
				require.NoError(t, err, tc.desc)
				require.Equal(t, tc.want, got, tc.desc)
			}

			_, err := gitops.IsReachable(f.ctx, f.repo, "--all", []git.ObjectID{c2})
			require.Error(t, err, "the commit must be an object ID")
		})
	}
}
