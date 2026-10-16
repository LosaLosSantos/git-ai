package gitops_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
)

// A symbolic reference is reported with its immediate target, not with the end
// of its chain (for-each-ref's %(symref) resolves chains).
func TestReadRefsImmediateSymbolicTargets(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, format)
			c1 := f.commit(t, nil, "one")
			gittest.WriteRef(t, f.cfg, f.path, "refs/heads/main", c1)
			f.git(t, "symbolic-ref", "refs/heads/alias", "refs/heads/main")
			f.git(t, "symbolic-ref", "refs/heads/top", "refs/heads/alias")
			f.git(t, "symbolic-ref", "HEAD", "refs/heads/top")

			want := map[string]corestore.Target{
				"HEAD":             corestore.SymbolicTarget("refs/heads/top"),
				"refs/heads/top":   corestore.SymbolicTarget("refs/heads/alias"),
				"refs/heads/alias": corestore.SymbolicTarget("refs/heads/main"),
				"refs/heads/main":  corestore.Target(c1.String()),
			}

			refs, err := gitops.ReadRefs(f.ctx, f.repo)
			require.NoError(t, err)
			require.Equal(t, want, refs)

			values, err := gitops.ReadRefValues(f.ctx, f.repo, []string{"HEAD", "refs/heads/top", "refs/heads/alias", "refs/heads/main"})
			require.NoError(t, err)
			require.Equal(t, want, values)
		})
	}
}

// A symbolic reference whose target does not exist is reported with its
// target by ReadRefValues (for-each-ref leaves it out), also among many absent
// names and at the end of a chain; a dangling HEAD by both functions.
func TestReadRefValuesDanglingSymbolicReferences(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, format)
			c1 := f.commit(t, nil, "one")
			gittest.WriteRef(t, f.cfg, f.path, "refs/heads/main", c1)
			f.git(t, "symbolic-ref", "refs/heads/sym", "refs/heads/nope")
			f.git(t, "symbolic-ref", "refs/heads/chain", "refs/heads/sym")
			f.git(t, "symbolic-ref", "HEAD", "refs/heads/unborn")

			names := []string{"HEAD", "refs/heads/main", "refs/heads/sym", "refs/heads/chain"}
			for i := range 1200 {
				names = append(names, fmt.Sprintf("refs/heads/absent-%d", i))
			}

			values, err := gitops.ReadRefValues(f.ctx, f.repo, names)
			require.NoError(t, err)
			require.Equal(t, map[string]corestore.Target{
				"HEAD":             corestore.SymbolicTarget("refs/heads/unborn"),
				"refs/heads/main":  corestore.Target(c1.String()),
				"refs/heads/sym":   corestore.SymbolicTarget("refs/heads/nope"),
				"refs/heads/chain": corestore.SymbolicTarget("refs/heads/sym"),
			}, values)

			// ReadRefs reports the dangling HEAD; git lists no other dangling
			// symbolic reference.
			refs, err := gitops.ReadRefs(f.ctx, f.repo)
			require.NoError(t, err)
			require.Equal(t, corestore.SymbolicTarget("refs/heads/unborn"), refs["HEAD"])
			require.Equal(t, corestore.Target(c1.String()), refs["refs/heads/main"])

			// Only absent names: nothing is reported.
			values, err = gitops.ReadRefValues(f.ctx, f.repo, []string{"refs/heads/absent", "refs/tags/absent"})
			require.NoError(t, err)
			require.Empty(t, values)
		})
	}
}

// git refuses a symbolic reference to HEAD, so a target ref:HEAD read from the
// database is invalid; ApplyRefs rejects it without touching the repository.
func TestApplyRefsRejectsSymbolicTargetHEAD(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "sha1")
	c1 := f.commit(t, nil, "one")

	err := gitops.ApplyRefs(f.ctx, f.repo, map[string]corestore.Target{
		"refs/heads/main": corestore.Target(c1.String()),
		"refs/heads/sym":  corestore.SymbolicTarget("HEAD"),
	})
	require.Error(t, err)

	refs, err := gitops.ReadRefs(f.ctx, f.repo)
	require.NoError(t, err)
	require.Equal(t, map[string]corestore.Target{"HEAD": corestore.SymbolicTarget("refs/heads/main")}, refs)
	require.Error(t, gitops.ValidateTarget(corestore.SymbolicTarget("HEAD"), git.ObjectHashSHA1))
}
