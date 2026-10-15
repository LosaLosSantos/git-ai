package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// tree writes a commit with the given tree entries, which may carry any file
// mode. A nil parents list makes a root commit.
func (h *harness) tree(parents []git.ObjectID, branch string, entries ...gittest.TreeEntry) git.ObjectID {
	h.t.Helper()

	opts := []gittest.WriteCommitOption{
		gittest.WithParents(parents...),
		gittest.WithTreeEntries(entries...),
	}
	if branch != "" {
		opts = append(opts, gittest.WithBranch(branch))
	}
	return gittest.WriteCommit(h.t, h.cfg, h.repoPath, opts...)
}

// verdictCurrent returns whether the first verdict of the review is current.
func verdictCurrent(t testing.TB, r *gitalypb.Review) bool {
	t.Helper()
	require.NotEmpty(t, r.GetVerdicts())
	return r.GetVerdicts()[0].GetCurrent()
}

// Regression: a mode change with an identical blob is not a comment-only
// change, so an approval on the old head does not survive it.
func TestSpecRegressChmodDropsApproval(t *testing.T) {
	h := setupHarness(t)
	base := h.tree(nil, "main", gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV1})
	head1 := h.tree([]git.ObjectID{base}, "",
		gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV1},
		gittest.TreeEntry{Mode: "100644", Path: "run.py", Content: "print(1)\n"},
	)

	review := h.create(head1)
	require.Equal(t, gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, classOf(t, review, "run.py"))
	h.approve(review.GetId(), head1, "reviewer")
	require.True(t, h.getReview(review.GetId()).GetApproved())

	head2 := h.tree([]git.ObjectID{head1}, "",
		gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV1},
		gittest.TreeEntry{Mode: "100755", Path: "run.py", Content: "print(1)\n"},
	)
	review = h.updateReview(review.GetId(), head2, "")

	require.Equal(t, gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, classOf(t, review, "run.py"),
		"a mode change is never classified by content")
	require.False(t, verdictCurrent(t, review), "the approval on head1 must not survive a chmod")
	require.False(t, review.GetApproved())
}

// Regression: a regular file replaced by a symlink with the same blob resolves
// to different code, so the approval on the old head does not survive.
func TestSpecRegressRegularToSymlinkDropsApproval(t *testing.T) {
	h := setupHarness(t)
	base := h.tree(nil, "main", gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV1})
	head3 := h.tree([]git.ObjectID{base}, "",
		gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV1},
		gittest.TreeEntry{Mode: "100644", Path: "x.py", Content: "evil"},
		gittest.TreeEntry{Mode: "100644", Path: "evil", Content: "import os\nos.system('true')\n"},
	)

	review := h.create(head3)
	h.approve(review.GetId(), head3, "reviewer")
	require.True(t, h.getReview(review.GetId()).GetApproved())

	head4 := h.tree([]git.ObjectID{head3}, "",
		gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV1},
		gittest.TreeEntry{Mode: "120000", Path: "x.py", Content: "evil"},
		gittest.TreeEntry{Mode: "100644", Path: "evil", Content: "import os\nos.system('true')\n"},
	)
	review = h.updateReview(review.GetId(), head4, "")

	require.Equal(t, gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, classOf(t, review, "x.py"))
	require.False(t, verdictCurrent(t, review), "a symlink replacing a file is never comment-only")
	require.False(t, review.GetApproved())
}

// Regression: a submodule bump under a docs directory is not a docs change.
func TestSpecRegressSubmoduleUnderDocsNeedsApproval(t *testing.T) {
	h := setupHarness(t)
	commitA, commitB := submoduleCommits(h)
	docsAt := func(commit git.ObjectID) git.ObjectID {
		return gittest.WriteTree(h.t, h.cfg, h.repoPath, []gittest.TreeEntry{
			{Mode: "160000", Path: "theme", OID: commit},
		})
	}

	base := h.tree(nil, "main",
		gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV1},
		gittest.TreeEntry{Mode: "040000", Path: "docs", OID: docsAt(commitA)},
	)
	head1 := h.tree([]git.ObjectID{base}, "",
		gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV2},
		gittest.TreeEntry{Mode: "040000", Path: "docs", OID: docsAt(commitA)},
	)
	review := h.create(head1)
	h.approve(review.GetId(), head1, "reviewer")

	head2 := h.tree([]git.ObjectID{head1}, "",
		gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV2},
		gittest.TreeEntry{Mode: "040000", Path: "docs", OID: docsAt(commitB)},
	)
	review = h.updateReview(review.GetId(), head2, "")

	require.Equal(t, gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, classOf(t, review, "docs/theme"))
	require.False(t, verdictCurrent(t, review), "a submodule bump under docs must not keep an approval")
	require.False(t, review.GetApproved())
}

// Regression: a submodule bump under docs alone needs approval at creation.
func TestSpecRegressSubmoduleUnderDocsCreateNeedsApproval(t *testing.T) {
	h := setupHarness(t)
	commitA, commitB := submoduleCommits(h)
	docsAt := func(commit git.ObjectID) git.ObjectID {
		return gittest.WriteTree(h.t, h.cfg, h.repoPath, []gittest.TreeEntry{
			{Mode: "160000", Path: "theme", OID: commit},
		})
	}

	base := h.tree(nil, "main", gittest.TreeEntry{Mode: "040000", Path: "docs", OID: docsAt(commitA)})
	head := h.tree([]git.ObjectID{base}, "", gittest.TreeEntry{Mode: "040000", Path: "docs", OID: docsAt(commitB)})

	review := h.create(head)
	require.Equal(t, gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, classOf(t, review, "docs/theme"))
	require.NotEmpty(t, review.GetRequirements(), "a submodule change needs an approval")
	require.False(t, review.GetApproved())
}

// submoduleCommits writes two distinct commits to point submodule entries at.
func submoduleCommits(h *harness) (git.ObjectID, git.ObjectID) {
	a := h.tree(nil, "", gittest.TreeEntry{Mode: "100644", Path: "README", Content: "submodule A\n"})
	b := h.tree(nil, "", gittest.TreeEntry{Mode: "100644", Path: "README", Content: "submodule B\n"})
	return a, b
}

// Regression: a verdict whose commit was pruned cannot be shown to be
// comment-only, so it becomes non-current instead of failing UpdateReview.
func TestSpecRegressPrunedVerdictCommitIsNotCurrent(t *testing.T) {
	h := setupHarness(t)
	base, head1 := h.fixture(codeV2)
	review := h.create(head1)
	h.approve(review.GetId(), head1, "reviewer")
	require.True(t, h.getReview(review.GetId()).GetApproved())

	// Remove head1 from the object database, as housekeeping would after a
	// force push.
	oid := head1.String()
	require.NoError(t, os.Remove(filepath.Join(h.repoPath, "objects", oid[:2], oid[2:])))

	// The force-pushed head descends from the base, but not from head1.
	forcePushed := h.write([]git.ObjectID{base}, "", map[string]string{"a.go": codeV3})
	review = h.updateReview(review.GetId(), forcePushed, "")

	require.False(t, verdictCurrent(t, review))
	require.False(t, review.GetApproved())
}

// Regression: a version larger than the classification cap is never read and
// is classified as code, even when a path rule would make it docs.
func TestSpecRegressLargeBlobIsCode(t *testing.T) {
	h := setupHarness(t)
	base := h.tree(nil, "main", gittest.TreeEntry{Mode: "100644", Path: "notes.md", Content: "# base\n"})

	big := strings.Repeat("a", maxClassifiedBlobBytes+1)
	head1 := h.tree([]git.ObjectID{base}, "", gittest.TreeEntry{Mode: "100644", Path: "notes.md", Content: big})

	review := h.create(head1)
	require.Equal(t, gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, classOf(t, review, "notes.md"))
	require.Contains(t, classReason(t, review, "notes.md"), "larger than")
	h.approve(review.GetId(), head1, "reviewer")

	head2 := h.tree([]git.ObjectID{head1}, "", gittest.TreeEntry{Mode: "100644", Path: "notes.md", Content: big + "b"})
	review = h.updateReview(review.GetId(), head2, "")
	require.False(t, verdictCurrent(t, review), "an oversized docs change is not known to be docs")
}

// Regression: a small docs change still classifies as DOCS, so approvals
// survive it. Guards the size cap and mode checks against over-blocking.
func TestSpecRegressSmallDocsChangeKeepsApproval(t *testing.T) {
	h := setupHarness(t)
	base := h.tree(nil, "main",
		gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV1},
		gittest.TreeEntry{Mode: "100644", Path: "notes.md", Content: "# base\n"},
	)
	head1 := h.tree([]git.ObjectID{base}, "",
		gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV2},
		gittest.TreeEntry{Mode: "100644", Path: "notes.md", Content: "# base\n"},
	)
	review := h.create(head1)
	h.approve(review.GetId(), head1, "reviewer")

	head2 := h.tree([]git.ObjectID{head1}, "",
		gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV2},
		gittest.TreeEntry{Mode: "100644", Path: "notes.md", Content: "# base\nmore\n"},
	)
	review = h.updateReview(review.GetId(), head2, "")
	require.Equal(t, gitalypb.ReviewFile_CHANGE_CLASS_DOCS, classOf(t, review, "notes.md"))
	require.True(t, verdictCurrent(t, review))
}

// Verdicts on the same commit are evaluated together and keep one outcome.
func TestSpecRegressSameCommitVerdictsAgree(t *testing.T) {
	h := setupHarness(t)
	_, head1 := h.fixture(codeV1)
	review := h.create(head1)
	for _, actor := range []string{"first", "second"} {
		h.approve(review.GetId(), head1, actor)
	}

	head2 := h.write([]git.ObjectID{head1}, "", map[string]string{"a.go": codeV1 + "// comment\n"})
	review = h.updateReview(review.GetId(), head2, "")

	require.Len(t, review.GetVerdicts(), 2)
	for _, v := range review.GetVerdicts() {
		require.True(t, v.GetCurrent(), "both approvals on head1 survive a comment-only change")
	}
}

func classOf(t testing.TB, r *gitalypb.Review, path string) gitalypb.ReviewFile_ChangeClass {
	t.Helper()
	for _, f := range r.GetFiles() {
		if string(f.GetPath()) == path {
			return f.GetClass()
		}
	}
	t.Fatalf("file %q is not a file of the review", path)
	return gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED
}

func classReason(t testing.TB, r *gitalypb.Review, path string) string {
	t.Helper()
	for _, f := range r.GetFiles() {
		if string(f.GetPath()) == path {
			return f.GetReason()
		}
	}
	t.Fatalf("file %q is not a file of the review", path)
	return ""
}
