package syncer_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/corestore/gitops"
	"github.com/feder-cr/open_scion/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/testhelper"
)

// gitOut runs git in the local repository and returns its trimmed output.
func (e *env) gitOut(t *testing.T, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(gittest.Exec(t, e.cfg, append([]string{"-C", e.localPath()}, args...)...)))
}

// localValues reads the named local references with gitops.ReadRefValues
// (immediate symbolic targets, dangling symbolic references included).
func (e *env) localValues(t *testing.T, names ...string) map[string]corestore.Target {
	t.Helper()
	values, err := gitops.ReadRefValues(e.ctx, e.localRepo(), names)
	require.NoError(t, err)
	return values
}

// repairs counts the log entries of a catch-up repairing a local reference.
func repairs(hook testhelper.LoggerHook) int {
	n := 0
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "repairing it") {
			n++
		}
	}
	return n
}

// REFTABLE RULE: a local repository that does not use the reftable backend is
// a stale cache, replaced by a reftable hydration, also when its watermark
// matches the database (with or without newer changesets), and without making
// the sync loop between hydration and catch-up.
func TestSyncerReplacesNonReftableRepository(t *testing.T) {
	for _, tc := range []struct {
		desc      string
		newerWork bool
	}{
		{desc: "with a newer changeset", newerWork: true},
		{desc: "watermark at the head", newerWork: false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
				e := newEnv(t, envOptions{dialect: dialect})
				src := e.newSource(t)
				c1 := e.writeCommit(t, src)
				repo := e.register(t, e.key(localRelPath), src, c1)
				require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
				require.Equal(t, "reftable", e.gitOut(t, "rev-parse", "--show-ref-format"))

				e.gitOut(t, "refs", "migrate", "--ref-format=files")
				require.Equal(t, "files", e.gitOut(t, "rev-parse", "--show-ref-format"))

				if tc.newerWork {
					e.advanceMain(t, repo.ID, src, c1)
				}

				require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
				require.Equal(t, "reftable", e.gitOut(t, "rev-parse", "--show-ref-format"))
				e.verifySynced(t, repo)
				require.Equal(t, float64(2), syncCount(t, e.sy, "hydrated"))
				require.Zero(t, syncCount(t, e.sy, "error"))

				// The replacement is final: the next call finds it fresh.
				require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
				require.Equal(t, float64(1), syncCount(t, e.sy, "fresh"))
			})
		})
	}
}

// Symbolic references are compared by their immediate value: with HEAD ->
// alias -> main, a database change of HEAD back to main is a change of HEAD
// (not "already final" because HEAD resolves to main), and the chain does not
// make every catch-up repair the reference.
func TestSyncerSymbolicChainUsesImmediateTargets(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		hook := testhelper.AddLoggerHook(e.logger.(log.LogrusLogger))
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/alias", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")},
		})
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "HEAD", Expect: corestore.ExpectAny, New: corestore.SymbolicTarget("refs/heads/alias")},
		})
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		require.Equal(t, "refs/heads/alias", e.gitOut(t, "symbolic-ref", "--no-recurse", "HEAD"))
		e.verifySynced(t, repo)

		// HEAD back to main: it resolves to main already, but its value is alias.
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "HEAD", Expect: corestore.ExpectAny, New: corestore.SymbolicTarget("refs/heads/main")},
		})
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		require.Equal(t, "refs/heads/main", e.gitOut(t, "symbolic-ref", "--no-recurse", "HEAD"))
		e.verifySynced(t, repo)

		// A two-level chain (HEAD -> top -> alias -> main) changed at its middle.
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/top", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/alias")},
			{Name: "HEAD", Expect: corestore.ExpectAny, New: corestore.SymbolicTarget("refs/heads/top")},
		})
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/top", Expect: corestore.ExpectAny, New: corestore.SymbolicTarget("refs/heads/main")},
		})
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		require.Equal(t, "refs/heads/top", e.gitOut(t, "symbolic-ref", "--no-recurse", "HEAD"))
		require.Equal(t, "refs/heads/main", e.gitOut(t, "symbolic-ref", "--no-recurse", "refs/heads/top"))
		require.Equal(t, "refs/heads/main", e.gitOut(t, "symbolic-ref", "--no-recurse", "refs/heads/alias"))

		require.Zero(t, repairs(hook), "no catch-up repaired a reference")
	})
}

// A dangling symbolic reference has its target as its value: catch-ups that
// change it do not take it for a diverged reference.
func TestSyncerDanglingSymbolicReference(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		hook := testhelper.AddLoggerHook(e.logger.(log.LogrusLogger))
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/sym", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/nope")},
		})

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		require.Equal(t, "refs/heads/nope", e.gitOut(t, "symbolic-ref", "--no-recurse", "refs/heads/sym"))
		require.Equal(t, map[string]corestore.Target{
			"refs/heads/sym": corestore.SymbolicTarget("refs/heads/nope"),
		}, e.localValues(t, "refs/heads/sym"))

		// Retarget it to another missing branch, then to main, then delete it.
		for _, next := range []corestore.Target{
			corestore.SymbolicTarget("refs/heads/gone"),
			corestore.SymbolicTarget("refs/heads/main"),
			"",
		} {
			e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
				{Name: "refs/heads/sym", Expect: corestore.ExpectAny, New: next},
			})
			require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

			want := map[string]corestore.Target{}
			if next != "" {
				want["refs/heads/sym"] = next
			}
			require.Equal(t, want, e.localValues(t, "refs/heads/sym"))
		}

		e.verifySynced(t, repo)
		require.Zero(t, repairs(hook), "no catch-up repaired a reference")
	})
}

// Every kind of move the catch-up makes, each checked by git: absent to
// symbolic, symbolic to object ID (HEAD and a branch), symbolic to absent,
// object ID to symbolic, symbolic to symbolic.
func TestSyncerAppliesEveryKindOfMove(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		c2 := e.writeCommit(t, src, c1)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/s1", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")},
			{Name: "refs/heads/s2", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")},
			{Name: "refs/heads/o1", Expect: corestore.ExpectValue, New: corestore.Target(c2)},
		}, c2)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)

		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "refs/heads/s1", Expect: corestore.ExpectAny, New: corestore.Target(c2)},
			{Name: "refs/heads/s2", Expect: corestore.ExpectAny},
			{Name: "refs/heads/o1", Expect: corestore.ExpectAny, New: corestore.SymbolicTarget("refs/heads/main")},
			{Name: "HEAD", Expect: corestore.ExpectAny, New: corestore.Target(c1)},
		})
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		require.Equal(t, c1.String(), e.gitOut(t, "rev-parse", "HEAD"))

		// A detached HEAD becomes symbolic again, a symbolic branch changes target.
		e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
			{Name: "HEAD", Expect: corestore.ExpectAny, New: corestore.SymbolicTarget("refs/heads/o1")},
			{Name: "refs/heads/o1", Expect: corestore.ExpectAny, New: corestore.SymbolicTarget("refs/heads/s1")},
		})
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		require.Equal(t, map[string]corestore.Target{
			"HEAD":            corestore.SymbolicTarget("refs/heads/o1"),
			"refs/heads/o1":   corestore.SymbolicTarget("refs/heads/s1"),
			"refs/heads/s1":   corestore.Target(c2),
			"refs/heads/main": corestore.Target(c1),
		}, e.localValues(t, "HEAD", "refs/heads/o1", "refs/heads/s1", "refs/heads/s2", "refs/heads/main"))
	})
}

// ORCHESTRATOR RULING 2: HEAD changed by a local write between the check and
// the apply (a SetDefaultBranch of this node, committed to the database first)
// makes git fail the symbolic compare-and-swap of HEAD; the catch-up does not
// overwrite it, and the watermark only claims what the local repository has.
func TestSyncerHeadChangedBetweenCheckAndApply(t *testing.T) {
	for _, tc := range []struct {
		desc string
		// final is HEAD in the changeset of another node that the catch-up applies.
		final func(c1 corestore.Target) corestore.Target
	}{
		{desc: "symbolic to symbolic", final: func(corestore.Target) corestore.Target {
			return corestore.SymbolicTarget("refs/heads/feature")
		}},
		{desc: "symbolic to object ID", final: func(c1 corestore.Target) corestore.Target { return c1 }},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
				e := newEnv(t, envOptions{dialect: dialect})
				src := e.newSource(t)
				c1 := e.writeCommit(t, src)
				repo := e.register(t, e.key(localRelPath), src, c1)
				require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

				// Changeset 2: another node moves HEAD (main -> the final value).
				e.commitFrom(t, repo.ID, otherNode, src, []corestore.RefUpdate{
					{Name: "refs/heads/feature", Expect: corestore.ExpectValue, New: corestore.Target(c1)},
					{Name: "HEAD", Expect: corestore.ExpectAny, New: tc.final(corestore.Target(c1))},
				})

				// Changeset 3: after the check read HEAD = main, this node sets
				// HEAD to release (database first, then git, then the note).
				e.sy.SetTestHook(once("checked", func() {
					e.localWrite(t, repo.ID, corestore.RefUpdate{
						Name: "HEAD", Expect: corestore.ExpectAny, New: corestore.SymbolicTarget("refs/heads/release"),
					})
				}))

				require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
				require.Equal(t, "refs/heads/release", e.gitOut(t, "symbolic-ref", "--no-recurse", "HEAD"))
				e.verifySynced(t, repo)
			})
		})
	}
}
