package rebase

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// specSetup creates a fresh repository and returns a localrepo handle to it.
func specSetup(t *testing.T) (context.Context, config.Cfg, string, *localrepo.Repo) {
	t.Helper()
	ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	return ctx, cfg, repoPath, repo
}

// specCommit writes a commit whose tree holds exactly the given files.
func specCommit(t *testing.T, cfg config.Cfg, repoPath string, parents []git.ObjectID, files map[string]string) git.ObjectID {
	t.Helper()
	entries := make([]gittest.TreeEntry, 0, len(files))
	for path, content := range files {
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: path, Content: content})
	}
	return gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(parents...),
		gittest.WithTreeEntries(entries...),
	)
}

func specGit(t *testing.T, cfg config.Cfg, repoPath string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(gittest.Exec(t, cfg, append([]string{"-C", repoPath}, args...)...)))
}

func specFiles(t *testing.T, cfg config.Cfg, repoPath string, commit git.ObjectID) []string {
	t.Helper()
	return strings.Fields(specGit(t, cfg, repoPath, "ls-tree", "-r", "--name-only", commit.String()))
}

func specParents(t *testing.T, cfg config.Cfg, repoPath string, commit git.ObjectID) []string {
	t.Helper()
	return strings.Fields(specGit(t, cfg, repoPath, "log", "-1", "--format=%P", commit.String()))
}

// TestSpecRebaseFastForward: when upstream is already an ancestor of the
// branch, landing returns the branch unchanged.
func TestSpecRebaseFastForward(t *testing.T) {
	ctx, cfg, repoPath, repo := specSetup(t)

	base := specCommit(t, cfg, repoPath, nil, map[string]string{"base": "base\n"})
	upstream := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "u": "u\n"})
	branch := specCommit(t, cfg, repoPath, []git.ObjectID{upstream}, map[string]string{"base": "base\n", "u": "u\n", "b": "b\n"})

	engine := New(repo)
	t.Cleanup(func() { _ = engine.Close() })

	got, err := landRebase(ctx, repo, engine, upstream, branch, nil)
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if got != branch {
		t.Fatalf("fast-forward: got %s, want branch %s", got, branch)
	}
}

// TestSpecRebaseReplaysBranchOntoUpstream: branch commits are replayed in
// order on top of upstream, keeping their full commit messages.
func TestSpecRebaseReplaysBranchOntoUpstream(t *testing.T) {
	ctx, cfg, repoPath, repo := specSetup(t)

	base := specCommit(t, cfg, repoPath, nil, map[string]string{"base": "base\n"})
	upstream := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "u": "u\n"})
	c1 := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "b": "b\n"})
	c2 := specCommit(t, cfg, repoPath, []git.ObjectID{c1}, map[string]string{"base": "base\n", "b": "b\n", "c": "c\n"})

	engine := New(repo)
	t.Cleanup(func() { _ = engine.Close() })

	got, err := landRebase(ctx, repo, engine, upstream, c2, nil)
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if got == c2 || got == upstream {
		t.Fatalf("expected a new rebased commit, got %s", got)
	}

	files := specFiles(t, cfg, repoPath, got)
	for _, want := range []string{"base", "u", "b", "c"} {
		if !specContains(files, want) {
			t.Fatalf("rebased tree %v is missing %q", files, want)
		}
	}

	// The rebased tip must sit on top of a rebased copy of c1, which sits on upstream.
	parent := specParents(t, cfg, repoPath, got)
	if len(parent) != 1 {
		t.Fatalf("rebased tip has parents %v, want exactly one", parent)
	}
	grand := specParents(t, cfg, repoPath, git.ObjectID(parent[0]))
	if len(grand) != 1 || grand[0] != upstream.String() {
		t.Fatalf("first replayed commit has parents %v, want [%s]", grand, upstream)
	}

	if msg, want := specGit(t, cfg, repoPath, "log", "-1", "--format=%B", got.String()), specGit(t, cfg, repoPath, "log", "-1", "--format=%B", c2.String()); msg != want {
		t.Fatalf("message changed: got %q, want %q", msg, want)
	}
}

// TestSpecRebaseConflictReturnsRebaseConflictError: a conflicting replay
// returns *localrepo.RebaseConflictError.
func TestSpecRebaseConflictReturnsRebaseConflictError(t *testing.T) {
	ctx, cfg, repoPath, repo := specSetup(t)

	base := specCommit(t, cfg, repoPath, nil, map[string]string{"a": "1\n"})
	upstream := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"a": "upstream\n"})
	branch := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"a": "branch\n"})

	engine := New(repo)
	t.Cleanup(func() { _ = engine.Close() })

	_, err := landRebase(ctx, repo, engine, upstream, branch, nil)
	var conflict *localrepo.RebaseConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("got error %v (%T), want *localrepo.RebaseConflictError", err, err)
	}
}

// TestSpecRebaseSkipsCherryPickedCommits: a branch commit whose change is
// already in upstream becomes empty when it is replayed and is dropped, so the
// result is upstream itself. (Landing reapplies cherry-picks, see computeTip in
// internal/gitaly/service/operations/land_attempt.go; the commit is dropped as empty, not by
// patch ID.)
func TestSpecRebaseSkipsCherryPickedCommits(t *testing.T) {
	ctx, cfg, repoPath, repo := specSetup(t)

	base := specCommit(t, cfg, repoPath, nil, map[string]string{"base": "base\n"})
	upstream := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "b": "b\n"})
	branch := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "b": "b\n"})

	engine := New(repo)
	t.Cleanup(func() { _ = engine.Close() })

	got, err := landRebase(ctx, repo, engine, upstream, branch, nil)
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if got != upstream {
		t.Fatalf("cherry-picked commit was replayed: got %s, want upstream %s", got, upstream)
	}
}

// TestSpecPlanRefusesMergesInRange: a range with a merge commit cannot be
// rebased. Landing refuses it (InvalidArgument, use STRATEGY_MERGE), so the plan
// returns ErrMergesInRange instead of a todo list.
func TestSpecPlanRefusesMergesInRange(t *testing.T) {
	ctx, cfg, repoPath, repo := specSetup(t)

	base := specCommit(t, cfg, repoPath, nil, map[string]string{"base": "base\n"})
	upstream := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "u": "u\n"})
	left := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "b": "b\n"})
	right := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "s": "s\n"})
	merge := specCommit(t, cfg, repoPath, []git.ObjectID{left, right}, map[string]string{"base": "base\n", "b": "b\n", "s": "s\n"})

	engine := New(repo)
	t.Cleanup(func() { _ = engine.Close() })

	if _, err := landRebase(ctx, repo, engine, upstream, merge, nil); !errors.Is(err, ErrMergesInRange) {
		t.Fatalf("got error %v, want ErrMergesInRange", err)
	}
}

// TestSpecEngineMatchesLocalrepoRebase: for the same inputs, landing (plan and
// replay) and (*localrepo.Repo).Rebase with reapplied cherry-picks produce the same tree, parents and messages on a
// clean replay, and the same error type on a conflict.
func TestSpecEngineMatchesLocalrepoRebase(t *testing.T) {
	ctx, cfg, repoPath, repo := specSetup(t)

	base := specCommit(t, cfg, repoPath, nil, map[string]string{"base": "base\n", "a": "1\n"})
	upstream := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "a": "1\n", "u": "u\n"})
	c1 := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "a": "1\n", "b": "b\n"})
	c2 := specCommit(t, cfg, repoPath, []git.ObjectID{c1}, map[string]string{"base": "base\n", "a": "1\n", "b": "b\n", "c": "c\n"})
	conflictUpstream := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"a": "upstream\n"})
	conflictBranch := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"a": "branch\n"})

	engine := New(repo)
	t.Cleanup(func() { _ = engine.Close() })

	engineOID, engineErr := landRebase(ctx, repo, engine, upstream, c2, nil)
	localOID, localErr := repo.Rebase(ctx, upstream.String(), c2.String(), rebaseOptions(nil)...)
	if engineErr != nil || localErr != nil {
		t.Fatalf("clean replay errors: engine=%v localrepo=%v", engineErr, localErr)
	}
	if got, want := specFiles(t, cfg, repoPath, engineOID), specFiles(t, cfg, repoPath, localOID); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tree mismatch: engine %v, localrepo %v", got, want)
	}
	if got, want := specParents(t, cfg, repoPath, engineOID), specParents(t, cfg, repoPath, localOID); len(got) != len(want) {
		t.Fatalf("parent count mismatch: engine %v, localrepo %v", got, want)
	}
	if got, want := specGit(t, cfg, repoPath, "log", "-1", "--format=%B", engineOID.String()), specGit(t, cfg, repoPath, "log", "-1", "--format=%B", localOID.String()); got != want {
		t.Fatalf("message mismatch: engine %q, localrepo %q", got, want)
	}

	_, engineErr = landRebase(ctx, repo, engine, conflictUpstream, conflictBranch, nil)
	_, localErr = repo.Rebase(ctx, conflictUpstream.String(), conflictBranch.String(), rebaseOptions(nil)...)
	var engineConflict, localConflict *localrepo.RebaseConflictError
	if !errors.As(engineErr, &engineConflict) || !errors.As(localErr, &localConflict) {
		t.Fatalf("conflict errors: engine %v (%T), localrepo %v (%T)", engineErr, engineErr, localErr, localErr)
	}
}

// TestSpecEngineReuseAndClose: one engine serves several rebases in a batch
// attempt, and Close succeeds both after use and on an engine that never ran.
func TestSpecEngineReuseAndClose(t *testing.T) {
	ctx, cfg, repoPath, repo := specSetup(t)

	base := specCommit(t, cfg, repoPath, nil, map[string]string{"base": "base\n"})
	upstream := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "u": "u\n"})
	c1 := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "x": "x\n"})
	c2 := specCommit(t, cfg, repoPath, []git.ObjectID{base}, map[string]string{"base": "base\n", "y": "y\n"})

	engine := New(repo)
	for _, branch := range []git.ObjectID{c1, c2} {
		if _, err := landRebase(ctx, repo, engine, upstream, branch, nil); err != nil {
			t.Fatalf("Rebase %s: %v", branch, err)
		}
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Close after use: %v", err)
	}

	idle := New(repo)
	if err := idle.Close(); err != nil {
		t.Fatalf("Close on unused engine: %v", err)
	}
}

func specContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
