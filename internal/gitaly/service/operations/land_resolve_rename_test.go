package operations

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// renameFileContent returns a 12-line file whose line 6 is line6. A rename and an edit of the same
// file are the conflicts these tests build, so the sides are told apart by line 6 alone.
func renameFileContent(line6 string) string {
	var b strings.Builder
	for i := 1; i <= 12; i++ {
		if i == 6 {
			b.WriteString(line6 + "\n")
		} else {
			fmt.Fprintf(&b, "line%d\n", i)
		}
	}
	return b.String()
}

// TestResolveLandConflicts_renameOnCommitSide covers a change that renames a and edits it, while
// the tip edits a in place. The conflict is named after the renamed path b, and the base of the
// file lives under a. The resolver must get the base of a, the tip's edit and the commit's edit.
func TestResolveLandConflicts_renameOnCommitSide(t *testing.T) {
	t.Parallel()

	base := renameFileContent("base")
	tipEdit := renameFileContent("OURS")
	commitEdit := renameFileContent("THEIRS")

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, resolver.Response{
		Version:  resolver.ProtocolVersion,
		Resolver: "graft-resolve/fake",
		Files: []resolver.ResponseFile{
			{Path: "b", Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged\n")},
		},
	})

	// The tip renames a to b and edits it. The landed commit edits a.
	fx := newResolveFixtureWith(t, config.GraftResolver{},
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: base}},
		[]gittest.TreeEntry{{Mode: "100644", Path: "b", Content: tipEdit}},
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: commitEdit}},
	)
	s := fx.server(t, enabledResolver(argv))

	res, err := s.resolveLandConflicts(fx.ctx, fx.request("Land rename"), fx.repo, fx.tip, fx.commit, fx.committer())
	require.NoError(t, err)

	require.Equal(t, 1, resolverCalls(t, dir))
	req := resolverRequest(t, dir, 1)
	require.Len(t, req.Files, 1)
	f := req.Files[0]
	require.Equal(t, "b", f.Path)
	require.Equal(t, resolver.KindContent, f.Kind)
	require.NotNil(t, f.Base, "the base of the renamed file must be sent")
	require.Equal(t, base, string(f.Base.Content))
	require.NotNil(t, f.Ours)
	require.Equal(t, tipEdit, string(f.Ours.Content))
	require.NotNil(t, f.Theirs, "the commit's edit of a must reach the resolver as theirs")
	require.Equal(t, commitEdit, string(f.Theirs.Content))

	require.Equal(t, [][]byte{[]byte("b")}, res.resolvedPaths)
	require.Equal(t, "merged\n", fx.blobAt(t, res.commit, "b"))
}

// TestResolveLandConflicts_renameOnTipSide is the mirror case: the tip renames a to b and edits
// it, while the landed commit edits a in place. The commit's edit must reach the resolver, and the
// tip's edit must not be lost.
func TestResolveLandConflicts_renameOnTipSide(t *testing.T) {
	t.Parallel()

	base := renameFileContent("base")
	tipEdit := renameFileContent("OURS")
	commitEdit := renameFileContent("THEIRS")

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, resolver.Response{
		Version:  resolver.ProtocolVersion,
		Resolver: "graft-resolve/fake",
		Files: []resolver.ResponseFile{
			{Path: "b", Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged\n")},
		},
	})

	// The tip edits a in place. The landed commit renames a to b and edits it.
	fx := newResolveFixtureWith(t, config.GraftResolver{},
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: base}},
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: tipEdit}},
		[]gittest.TreeEntry{{Mode: "100644", Path: "b", Content: commitEdit}},
	)
	s := fx.server(t, enabledResolver(argv))

	res, err := s.resolveLandConflicts(fx.ctx, fx.request("Land rename"), fx.repo, fx.tip, fx.commit, fx.committer())
	require.NoError(t, err)

	require.Equal(t, 1, resolverCalls(t, dir))
	req := resolverRequest(t, dir, 1)
	require.Len(t, req.Files, 1)
	f := req.Files[0]
	require.Equal(t, "b", f.Path)
	require.Equal(t, resolver.KindContent, f.Kind)
	require.NotNil(t, f.Base)
	require.Equal(t, base, string(f.Base.Content))
	require.NotNil(t, f.Ours)
	require.Equal(t, tipEdit, string(f.Ours.Content))
	require.NotNil(t, f.Theirs)
	require.Equal(t, commitEdit, string(f.Theirs.Content))

	require.Equal(t, [][]byte{[]byte("b")}, res.resolvedPaths)
	require.Equal(t, "merged\n", fx.blobAt(t, res.commit, "b"))
}

// TestUserLand_renameFromDeniedPathIsNotResolved covers a conflict whose name is allowed while a
// side of it comes from a denied path through a rename. merge-tree names the conflict after the
// new path, but the base and one side are the versions of the old path. None of them may reach the
// resolver: the landing keeps the merge conflict and the resolver is never called.
func TestUserLand_renameFromDeniedPathIsNotResolved(t *testing.T) {
	t.Parallel()

	base := renameFileContent("BASE-SECRET")
	inPlace := renameFileContent("EDIT-SECRET")
	renamed := renameFileContent("RENAMED")

	for _, tc := range []struct {
		desc        string
		tip, commit []gittest.TreeEntry
		policy      func(*config.GraftResolver)
	}{
		{
			desc:   "commit renames the denied file, tip edits it in place",
			tip:    []gittest.TreeEntry{{Mode: "100644", Path: "secrets/k", Content: inPlace}},
			commit: []gittest.TreeEntry{{Mode: "100644", Path: "pub/k", Content: renamed}},
			policy: func(r *config.GraftResolver) { r.PathsDeny = []string{"secrets/**"} },
		},
		{
			desc:   "tip renames the denied file, commit edits it in place",
			tip:    []gittest.TreeEntry{{Mode: "100644", Path: "pub/k", Content: renamed}},
			commit: []gittest.TreeEntry{{Mode: "100644", Path: "secrets/k", Content: inPlace}},
			policy: func(r *config.GraftResolver) { r.PathsDeny = []string{"secrets/**"} },
		},
		{
			desc:   "commit renames a file from outside the allow list",
			tip:    []gittest.TreeEntry{{Mode: "100644", Path: "secrets/k", Content: inPlace}},
			commit: []gittest.TreeEntry{{Mode: "100644", Path: "pub/k", Content: renamed}},
			policy: func(r *config.GraftResolver) { r.PathsAllow = []string{"pub/**"} },
		},
		{
			desc:   "tip renames a file from outside the allow list",
			tip:    []gittest.TreeEntry{{Mode: "100644", Path: "pub/k", Content: renamed}},
			commit: []gittest.TreeEntry{{Mode: "100644", Path: "secrets/k", Content: inPlace}},
			policy: func(r *config.GraftResolver) { r.PathsAllow = []string{"pub/**"} },
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			dir, argv := resolverScript(t, "")
			writeAnswer(t, dir, resolver.Response{
				Version:  resolver.ProtocolVersion,
				Resolver: "graft-resolve/fake",
				Files: []resolver.ResponseFile{
					{Path: "pub/k", Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged\n")},
				},
			})
			rcfg := enabledResolver(argv)
			tc.policy(&rcfg)

			fx := newResolveFixtureWith(t, rcfg,
				[]gittest.TreeEntry{{Mode: "100644", Path: "secrets/k", Content: base}},
				tc.tip, tc.commit,
			)

			// Directly, the file is not sent, and the package names the source path of the side.
			_, err := fx.server(t, rcfg).resolveLandConflicts(fx.ctx, fx.request("Land rename"), fx.repo, fx.tip, fx.commit, fx.committer())
			var unresolved *landUnresolvedError
			require.ErrorAs(t, err, &unresolved)
			k := packageFile(t, builtPackage(t, fx.ctx, unresolved), "pub/k")
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_SENT, k.GetOutcome())
			require.Equal(t, `a side comes from path "secrets/k", which may not be sent to the resolver`, k.GetReason())
			require.Equal(t, 0, resolverCalls(t, dir))

			// Through the RPC, the landing keeps the merge conflict.
			_, err = fx.client.UserLand(fx.ctx, fx.request("Land rename"))
			requireMergeConflict(t, err)
			require.Equal(t, 0, resolverCalls(t, dir))
			requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
		})
	}
}

// TestUserLand_renameBetweenAllowedPathsIsResolved checks that the source check does not refuse a
// rename whose paths are all allowed while a deny list is set.
func TestUserLand_renameBetweenAllowedPathsIsResolved(t *testing.T) {
	t.Parallel()

	base := renameFileContent("base")
	tipEdit := renameFileContent("OURS")
	commitEdit := renameFileContent("THEIRS")

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, resolver.Response{
		Version:  resolver.ProtocolVersion,
		Resolver: "graft-resolve/fake",
		Files: []resolver.ResponseFile{
			{Path: "pub/k", Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged\n")},
		},
	})
	rcfg := enabledResolver(argv)
	rcfg.PathsDeny = []string{"secrets/**"}

	// The commit renames src/k to pub/k and edits it, the tip edits src/k in place. A denied file
	// with other content exists on every side.
	other := gittest.TreeEntry{Mode: "100644", Path: "secrets/other", Content: "secret\n"}
	fx := newResolveFixtureWith(t, rcfg,
		[]gittest.TreeEntry{{Mode: "100644", Path: "src/k", Content: base}, other},
		[]gittest.TreeEntry{{Mode: "100644", Path: "src/k", Content: tipEdit}, other},
		[]gittest.TreeEntry{{Mode: "100644", Path: "pub/k", Content: commitEdit}, other},
	)

	resp, err := fx.client.UserLand(fx.ctx, fx.request("Land rename"))
	require.NoError(t, err)
	require.Equal(t, [][]byte{[]byte("pub/k")}, resp.GetResolvedPaths())
	require.Equal(t, 1, resolverCalls(t, dir))

	f := resolverRequest(t, dir, 1).Files[0]
	require.Equal(t, base, string(f.Base.Content))
	require.Equal(t, tipEdit, string(f.Ours.Content))
	require.Equal(t, commitEdit, string(f.Theirs.Content))
	require.Equal(t, "merged\n", fx.blobAt(t, git.ObjectID(resp.GetLandedCommitId()), "pub/k"))
}

// TestLandSourcePaths covers the lookup of the path a side comes from, including a blob that is
// held by several paths, all of which are returned so that each must be allowed.
func TestLandSourcePaths(t *testing.T) {
	t.Parallel()

	fx := newResolveFixtureWith(t, config.GraftResolver{},
		[]gittest.TreeEntry{
			{Mode: "100644", Path: "a", Content: "same\n"},
			{Mode: "100644", Path: "secrets/a", Content: "same\n"},
			{Mode: "100644", Path: "u", Content: "unique\n"},
		},
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "tip\n"}},
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "commit\n"}},
	)
	same := gittest.WriteBlob(t, fx.cfg, fx.repoPath, []byte("same\n"))
	unique := gittest.WriteBlob(t, fx.cfg, fx.repoPath, []byte("unique\n"))
	absent := gittest.WriteBlob(t, fx.cfg, fx.repoPath, []byte("absent\n"))
	listings := map[git.ObjectID]map[git.ObjectID][]string{}

	// The blob lives at the name of the conflict: no listing is needed.
	got, err := landSourcePaths(fx.ctx, fx.repo, listings, fx.base, same, "a")
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, got)
	require.Empty(t, listings)

	// Renamed: every path holding the blob is returned.
	got, err = landSourcePaths(fx.ctx, fx.repo, listings, fx.base, same, "renamed")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a", "secrets/a"}, got)

	got, err = landSourcePaths(fx.ctx, fx.repo, listings, fx.base, unique, "a")
	require.NoError(t, err)
	require.Equal(t, []string{"u"}, got)
	require.Len(t, listings, 1)

	// A blob in no path of the tree has no source: the classification does not send the file.
	got, err = landSourcePaths(fx.ctx, fx.repo, listings, fx.base, absent, "a")
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestLandCheckSides covers the shapes of the sides that are not resolved.
func TestLandShapeReason(t *testing.T) {
	t.Parallel()

	side := &landBlob{mode: resolver.ModeRegular, oid: "o"}

	for _, tc := range []struct {
		name string
		c    landFileConflict
		want string
	}{
		{name: "content", c: landFileConflict{path: "a", base: side, ours: side, theirs: side}},
		{name: "add-add", c: landFileConflict{path: "a", ours: side, theirs: side}},
		{name: "modify-delete with base", c: landFileConflict{path: "a", base: side, ours: side}},
		{name: "modify-delete with base on theirs", c: landFileConflict{path: "a", base: side, theirs: side}},
		{name: "neither side", c: landFileConflict{path: "a", base: side}, want: landNotSentNoSide},
		{name: "no sides", c: landFileConflict{path: "a"}, want: landNotSentNoSide},
		{name: "no base, ours missing", c: landFileConflict{path: "a", theirs: side}, want: landNotSentNoBaseOneSide},
		{name: "no base, theirs missing", c: landFileConflict{path: "a", ours: side}, want: landNotSentNoBaseOneSide},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, landShapeReason(tc.c))
		})
	}
}
