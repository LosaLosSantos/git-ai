package operations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// nestTreeEntries turns the entries whose paths contain slashes into the subtrees they belong to,
// because gittest.WriteCommit does not create the directories of a nested path itself.
func nestTreeEntries(t *testing.T, cfg config.Cfg, repoPath string, entries []gittest.TreeEntry) []gittest.TreeEntry {
	t.Helper()

	var nested []gittest.TreeEntry
	children := map[string][]gittest.TreeEntry{}
	var dirs []string
	for _, e := range entries {
		dir, rest, ok := strings.Cut(e.Path, "/")
		if !ok {
			nested = append(nested, e)
			continue
		}
		if _, seen := children[dir]; !seen {
			dirs = append(dirs, dir)
		}
		e.Path = rest
		children[dir] = append(children[dir], e)
	}

	for _, dir := range dirs {
		tree := gittest.WriteTree(t, cfg, repoPath, nestTreeEntries(t, cfg, repoPath, children[dir]))
		nested = append(nested, gittest.TreeEntry{Mode: "040000", Path: dir, OID: tree})
	}
	return nested
}

// TestUserLand_fileDirectoryConflictIsNotResolved covers a file of the tip that the landed commit
// turns into a directory. merge-tree reports the file under the synthetic name "x~<oid>", which is
// on neither side. Such a conflict must keep the merge conflict and never reach the resolver,
// because an answer to it could drop the file from the branch.
func TestUserLand_fileDirectoryConflictIsNotResolved(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc      string
		tipEntry  gittest.TreeEntry
		commitEnt gittest.TreeEntry
	}{
		{
			desc:      "tip adds a file, commit adds a directory",
			tipEntry:  gittest.TreeEntry{Mode: "100644", Path: "x", Content: "tip file\n"},
			commitEnt: gittest.TreeEntry{Mode: "100644", Path: "x/y", Content: "dir\n"},
		},
		{
			desc:      "tip adds a directory, commit adds a file",
			tipEntry:  gittest.TreeEntry{Mode: "100644", Path: "x/y", Content: "dir\n"},
			commitEnt: gittest.TreeEntry{Mode: "100644", Path: "x", Content: "commit file\n"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			dir, argv := resolverScript(t, "")
			writeAnswer(t, dir, validAnswer())
			fx := newResolveFixtureWith(t, enabledResolver(argv),
				[]gittest.TreeEntry{{Mode: "100644", Path: "q", Content: "q\n"}},
				[]gittest.TreeEntry{{Mode: "100644", Path: "q", Content: "q\n"}, tc.tipEntry},
				[]gittest.TreeEntry{{Mode: "100644", Path: "q", Content: "q\n"}, tc.commitEnt},
			)

			_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
			requireMergeConflict(t, err)
			require.Equal(t, 0, resolverCalls(t, dir))
			requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
		})
	}
}

// TestLandClassify_absentFromBothSides checks the fail-closed rule directly: a conflicting path
// that is on neither side is not sent, so no modify-delete without sides reaches the resolver.
func TestLandClassify_absentFromBothSides(t *testing.T) {
	t.Parallel()

	_, argv := resolverScript(t, "")
	rcfg := enabledResolver(argv)
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)

	f := &landFileState{conflict: landFileConflict{path: "x~" + fx.tip.String()}}
	require.NoError(t, s.landClassify(fx.ctx, fx.repo, []*landFileState{f}, fx.base, fx.tip, fx.commit))
	require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_SENT, f.outcome)
	require.Equal(t, landNotSentNoSide, f.reason)
}

// TestUserLand_pathStartingWithDashIsResolved covers a conflicting path that starts with a dash.
// Such a path must reach the resolver as a path, not as an option of ls-tree.
func TestUserLand_pathStartingWithDashIsResolved(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, resolver.Response{
		Version:  resolver.ProtocolVersion,
		Resolver: "graft-resolve/fake",
		Files: []resolver.ResponseFile{
			{Path: "-a", Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged-a\n")},
		},
	})
	fx := newResolveFixtureWith(t, enabledResolver(argv),
		[]gittest.TreeEntry{
			{Mode: "100644", Path: "-a", Content: "base\n"},
			{Mode: "100644", Path: "c", Content: "c\n"},
		},
		[]gittest.TreeEntry{
			{Mode: "100644", Path: "-a", Content: "ours\n"},
			{Mode: "100644", Path: "c", Content: "c\n"},
		},
		[]gittest.TreeEntry{
			{Mode: "100644", Path: "-a", Content: "theirs\n"},
			{Mode: "100644", Path: "c", Content: "c\n"},
			{Mode: "100644", Path: "d", Content: "d\n"},
		},
	)

	resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	require.NoError(t, err)
	require.Equal(t, [][]byte{[]byte("-a")}, resp.GetResolvedPaths())
	require.Equal(t, 1, resolverCalls(t, dir))

	req := resolverRequest(t, dir, 1)
	require.Len(t, req.Files, 1)
	require.Equal(t, "-a", req.Files[0].Path)
	require.Equal(t, "base\n", string(req.Files[0].Base.Content))

	landed := git.ObjectID(resp.GetLandedCommitId())
	require.Equal(t, "merged-a\n", fx.blobAt(t, landed, "-a"))
	require.Equal(t, "d\n", fx.blobAt(t, landed, "d"))
}

// TestUserLand_rebaseConflictWithCleanMergeIsNotResolved covers a rebase whose intermediate commit
// conflicts while the merge of the tip and the landed commit is clean, because a later commit
// restores the tip's version. The landing is not resolved and keeps the merge conflict.
func TestUserLand_rebaseConflictWithCleanMergeIsNotResolved(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())
	fx := newResolveFixtureWith(t, enabledResolver(argv),
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "base\n"}},
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "ours\n"}},
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "theirs\n"}},
	)

	// c1 changes a and c2 restores the tip's version, so the tip and c2 merge cleanly.
	c1 := gittest.WriteCommit(t, fx.cfg, fx.repoPath,
		gittest.WithParents(fx.base),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "theirs\n"}),
	)
	c2 := gittest.WriteCommit(t, fx.cfg, fx.repoPath,
		gittest.WithParents(c1),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "ours\n"}),
	)

	req := fx.request("Land")
	req.CommitId = c2.String()
	_, err := fx.client.UserLand(fx.ctx, req)
	requireMergeConflict(t, err)
	require.Equal(t, 0, resolverCalls(t, dir))
	requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
}

// TestResolveLandConflicts_crissCrossIsNotResolved covers histories with two merge bases. The base
// the resolver is given would then differ from the base of the recursive merge strategy, so such a
// landing is not resolved.
func TestResolveLandConflicts_crissCrossIsNotResolved(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())
	fx := newResolveFixture(t, enabledResolver(argv))

	// x1 and x2 are the two merge bases of tip2 and commit2: both are ancestors of both, and
	// neither is an ancestor of the other.
	x1 := gittest.WriteCommit(t, fx.cfg, fx.repoPath,
		gittest.WithParents(fx.base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base\n"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"},
			gittest.TreeEntry{Mode: "100644", Path: "e", Content: "x1\n"},
		),
	)
	x2 := gittest.WriteCommit(t, fx.cfg, fx.repoPath,
		gittest.WithParents(fx.base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base\n"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"},
			gittest.TreeEntry{Mode: "100644", Path: "f", Content: "x2\n"},
		),
	)
	tip2 := gittest.WriteCommit(t, fx.cfg, fx.repoPath,
		gittest.WithParents(x1, x2),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "ours\n"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "ours\n"},
			gittest.TreeEntry{Mode: "100644", Path: "e", Content: "x1\n"},
			gittest.TreeEntry{Mode: "100644", Path: "f", Content: "x2\n"},
		),
	)
	commit2 := gittest.WriteCommit(t, fx.cfg, fx.repoPath,
		gittest.WithParents(x2, x1),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "theirs\n"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "theirs\n"},
			gittest.TreeEntry{Mode: "100644", Path: "e", Content: "x1\n"},
			gittest.TreeEntry{Mode: "100644", Path: "f", Content: "x2\n"},
		),
	)

	s := fx.server(t, enabledResolver(argv))
	_, err := s.resolveLandConflicts(fx.ctx, fx.request("Land"), fx.repo, tip2, commit2, fx.committer())
	require.ErrorContains(t, err, "criss-cross")
	require.Equal(t, 0, resolverCalls(t, dir))
}

// TestResolveLandConflicts_actorIdentity covers the actor. Only the authenticated identity counts:
// the user name of the request, which any caller may choose, does not satisfy the actor list.
func TestResolveLandConflicts_actorIdentity(t *testing.T) {
	t.Parallel()

	fx := newResolveFixture(t, config.GraftResolver{})

	spoofed := fx.request("Land")
	spoofed.User = &gitalypb.User{Name: []byte("agent-spoof"), Email: []byte("spoof@example.com"), GlId: "user-spoof"}

	t.Run("display name of the request does not count", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		rcfg := enabledResolver(argv)
		rcfg.Actors = []string{"agent-*"}
		s := fx.server(t, rcfg)

		_, err := s.resolveLandConflicts(fx.ctx, spoofed, fx.repo, fx.tip, fx.commit, fx.committer())
		require.ErrorContains(t, err, "may not resolve conflicts")
		require.Equal(t, 0, resolverCalls(t, dir))
	})

	t.Run("authenticated identity is the actor", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		rcfg := enabledResolver(argv)
		rcfg.Actors = []string{"agent-*"}
		s := fx.server(t, rcfg)

		ctx := identity.NewContext(fx.ctx, identity.Identity{Name: "agent-7", Source: identity.SourceAgent})
		_, err := s.resolveLandConflicts(ctx, spoofed, fx.repo, fx.tip, fx.commit, fx.committer())
		require.NoError(t, err)
		require.Equal(t, "agent-7", resolverRequest(t, dir, 1).Actor)
	})
}
