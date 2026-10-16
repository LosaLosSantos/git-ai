package review

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// createAs creates a review of head against main as actor, calling the server
// directly so that the context carries the actor's identity.
func (h *harness) createAs(actor string, head git.ObjectID) *gitalypb.Review {
	h.t.Helper()

	resp, err := h.srv.CreateReview(h.as(actor), &gitalypb.CreateReviewRequest{
		Repository:   h.repo,
		TargetBranch: []byte("main"),
		HeadCommitId: head.String(),
		Title:        []byte("change a"),
	})
	require.NoError(h.t, err)
	return resp.GetReview()
}

// Regression: an identity other than the author cannot move the head of a
// review. Otherwise it could put its own code under the author's name and
// approve it, and the approval would count as a non-author approval.
func TestReviewOnlyAuthorMovesHead(t *testing.T) {
	h := setupHarness(t)
	base, aliceHead := h.fixture(codeV2)
	review := h.createAs("alice", aliceHead)
	require.Equal(t, "alice", review.GetAuthor())

	malloryHead := h.write([]git.ObjectID{base}, "", map[string]string{"a.go": "package a\nfunc A() { panic(1) }\n"})
	_, err := h.srv.UpdateReview(h.as("mallory"), &gitalypb.UpdateReviewRequest{
		Repository: h.repo, Id: review.GetId(), HeadCommitId: malloryHead.String(),
	})
	testhelperRequireCode(t, err, codes.PermissionDenied)

	// The anonymous caller is not the author either.
	_, err = h.client.UpdateReview(h.ctx, &gitalypb.UpdateReviewRequest{
		Repository: h.repo, Id: review.GetId(), HeadCommitId: malloryHead.String(),
	})
	testhelperRequireCode(t, err, codes.PermissionDenied)

	got := h.getReview(review.GetId())
	require.Equal(t, aliceHead.String(), got.GetHeadCommitId(), "the head is unchanged")

	// mallory's approval can only be given on alice's code.
	_, err = h.srv.SubmitReviewVerdict(h.as("mallory"), &gitalypb.SubmitReviewVerdictRequest{
		Repository: h.repo, Id: review.GetId(), Kind: gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED,
		HeadCommitId: malloryHead.String(),
	})
	testhelperRequireCode(t, err, codes.FailedPrecondition)

	// The author still moves the head.
	aliceHead2 := h.write([]git.ObjectID{aliceHead}, "", map[string]string{"a.go": codeV3})
	resp, err := h.srv.UpdateReview(h.as("alice"), &gitalypb.UpdateReviewRequest{
		Repository: h.repo, Id: review.GetId(), HeadCommitId: aliceHead2.String(),
	})
	require.NoError(t, err)
	require.Equal(t, aliceHead2.String(), resp.GetReview().GetHeadCommitId())
}

// Regression: an identity other than the author can neither close a review nor
// mark it landed.
func TestReviewOnlyAuthorCloses(t *testing.T) {
	h := setupHarness(t)
	_, head := h.fixture(codeV2)
	review := h.createAs("alice", head)

	for _, landed := range []bool{true, false} {
		_, err := h.srv.CloseReview(h.as("mallory"), &gitalypb.CloseReviewRequest{
			Repository: h.repo, Id: review.GetId(), Landed: landed,
		})
		testhelperRequireCode(t, err, codes.PermissionDenied)
	}
	require.Equal(t, gitalypb.Review_STATE_OPEN_UNSPECIFIED, h.getReview(review.GetId()).GetState())

	resp, err := h.srv.CloseReview(h.as("alice"), &gitalypb.CloseReviewRequest{
		Repository: h.repo, Id: review.GetId(), Landed: true,
	})
	require.NoError(t, err)
	require.Equal(t, gitalypb.Review_STATE_LANDED, resp.GetReview().GetState())
}

// Regression: an explicit base must be a merge base of the head and the branch
// tip. Any other base misstates the change, down to an empty file list that
// needs no approval.
func TestCreateReviewExplicitBaseMustBeMergeBase(t *testing.T) {
	h := setupHarness(t)

	// root has a.go at codeV1; main moved a.go to codeV2. The head forks from
	// main and reverts a.go to codeV1, so the change it lands is a code change.
	root := h.write(nil, "", map[string]string{"a.go": codeV1})
	tip := h.write([]git.ObjectID{root}, "main", map[string]string{"a.go": codeV2})
	head := h.write([]git.ObjectID{tip}, "", map[string]string{"a.go": codeV1})
	sameTreeOrphan := gittest.WriteCommit(t, h.cfg, h.repoPath,
		gittest.WithMessage("orphan"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV1}),
	)
	require.NotEqual(t, root, sameTreeOrphan)

	for _, tc := range []struct {
		name string
		base git.ObjectID
	}{
		{"base equals head", head},
		{"orphan with the head's tree", sameTreeOrphan},
		{"older common ancestor hiding a revert", root},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.client.CreateReview(h.ctx, &gitalypb.CreateReviewRequest{
				Repository: h.repo, TargetBranch: []byte("main"),
				HeadCommitId: head.String(), BaseCommitId: tc.base.String(),
			})
			testhelperRequireCode(t, err, codes.FailedPrecondition)
		})
	}

	resp, err := h.client.CreateReview(h.ctx, &gitalypb.CreateReviewRequest{
		Repository: h.repo, TargetBranch: []byte("main"),
		HeadCommitId: head.String(), BaseCommitId: tip.String(),
	})
	require.NoError(t, err)
	require.Equal(t, tip.String(), resp.GetReview().GetBaseCommitId())
	require.Equal(t, gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, classOf(t, resp.GetReview(), "a.go"))
	require.False(t, resp.GetReview().GetApproved())
}

// Regression: a new head must descend from the review base, otherwise the
// files computed against the base are not the change of the head.
func TestUpdateReviewHeadMustDescendFromBase(t *testing.T) {
	h := setupHarness(t)
	_, head1 := h.fixture(codeV2)
	review := h.create(head1)

	// An orphan with the base's tree would show no change at all. The message
	// makes it a commit other than the base.
	orphan := gittest.WriteCommit(t, h.cfg, h.repoPath,
		gittest.WithMessage("orphan"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV1}),
	)
	_, err := h.client.UpdateReview(h.ctx, &gitalypb.UpdateReviewRequest{
		Repository: h.repo, Id: review.GetId(), HeadCommitId: orphan.String(),
	})
	testhelperRequireCode(t, err, codes.FailedPrecondition)
	require.Equal(t, head1.String(), h.getReview(review.GetId()).GetHeadCommitId())
}

// Regression: a file under a doc directory that is not documentation by name
// is classified DOCS, but changing it after an approval drops the approval: it
// can be compiled code.
func TestUpdateReviewSourceUnderDocDirDropsApproval(t *testing.T) {
	h := setupHarness(t)
	// internalDir returns the tree of internal/ with doc/handler.go as given.
	internalDir := func(handler string) gittest.TreeEntry {
		doc := gittest.WriteTree(t, h.cfg, h.repoPath, []gittest.TreeEntry{
			{Mode: "100644", Path: "handler.go", Content: handler},
		})
		internal := gittest.WriteTree(t, h.cfg, h.repoPath, []gittest.TreeEntry{
			{Mode: "040000", Path: "doc", OID: doc},
		})
		return gittest.TreeEntry{Mode: "040000", Path: "internal", OID: internal}
	}
	handler := "package doc\nfunc Handle() {}\n"

	base := h.tree(nil, "main", gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV1}, internalDir(handler))
	head1 := h.tree([]git.ObjectID{base}, "", gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV2}, internalDir(handler))

	review := h.create(head1)
	h.approve(review.GetId(), head1, "reviewer")
	require.True(t, h.getReview(review.GetId()).GetApproved())

	head2 := h.tree([]git.ObjectID{head1}, "",
		gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: codeV2},
		internalDir("package doc\nfunc Handle() { panic(1) }\n"),
	)
	review = h.updateReview(review.GetId(), head2, "")

	require.Equal(t, gitalypb.ReviewFile_CHANGE_CLASS_DOCS, classOf(t, review, "internal/doc/handler.go"))
	require.False(t, verdictCurrent(t, review), "a source file under doc/ is not documentation by name")
	require.False(t, review.GetApproved())
}

// Regression: an actor's latest verdict counts only when it is current; an
// earlier approval that stayed current does not count again when the later
// request for changes became non-current.
func TestUpdateReviewSupersededApprovalDoesNotReturn(t *testing.T) {
	h := setupHarness(t)
	base := h.write(nil, "main", map[string]string{"a.go": codeV1, "notes.md": "x"})
	head1 := h.write([]git.ObjectID{base}, "", map[string]string{"a.go": codeV2, "notes.md": "x"})

	review := h.create(head1)
	h.approve(review.GetId(), head1, "bob")
	require.True(t, h.getReview(review.GetId()).GetApproved())

	// A docs change keeps bob's approval current.
	head2 := h.write([]git.ObjectID{head1}, "", map[string]string{"a.go": codeV2, "notes.md": "@generated\nx"})
	review = h.updateReview(review.GetId(), head2, "")
	require.True(t, review.GetApproved())

	_, err := h.srv.SubmitReviewVerdict(h.as("bob"), &gitalypb.SubmitReviewVerdictRequest{
		Repository: h.repo, Id: review.GetId(), Kind: gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES,
		HeadCommitId: head2.String(),
	})
	require.NoError(t, err)
	require.False(t, h.getReview(review.GetId()).GetApproved())

	// Back to head1: the change from head2 is GENERATED (the marker is in the
	// old version), so the request for changes becomes non-current, while the
	// approval on head1 itself stays current.
	review = h.updateReview(review.GetId(), head1, "")
	require.Len(t, review.GetVerdicts(), 2)
	require.True(t, review.GetVerdicts()[0].GetCurrent())
	require.False(t, review.GetVerdicts()[1].GetCurrent())
	require.False(t, review.GetApproved(), "bob's latest verdict is a request for changes")
}

func testhelperRequireCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, code, structerr.GRPCCode(err), "error: %v", err)
}
