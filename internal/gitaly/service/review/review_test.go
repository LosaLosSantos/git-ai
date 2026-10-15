package review

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/gitaly/service/repository"
	"github.com/feder-cr/open_scion/internal/graft/identity"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// harness runs a ReviewService and a repository to review.
type harness struct {
	t        testing.TB
	ctx      context.Context
	cfg      config.Cfg
	repo     *gitalypb.Repository
	repoPath string
	client   gitalypb.ReviewServiceClient
	// srv is the server behind the client, for calls that need an identity in
	// the context, which only the authentication layer sets over gRPC.
	srv *server
}

func setupHarness(t *testing.T) *harness {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	h := &harness{t: t, ctx: ctx}

	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		h.srv = newServer(deps)
		gitalypb.RegisterReviewServiceServer(srv, h.srv)
		// gittest.CreateRepository goes through the RepositoryService.
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	})
	h.cfg = cfg

	conn, err := client.New(ctx, cfg.SocketPath)
	require.NoError(t, err)
	t.Cleanup(func() { testhelper.MustClose(t, conn) })
	h.client = gitalypb.NewReviewServiceClient(conn)

	h.repo, h.repoPath = gittest.CreateRepository(t, ctx, cfg)
	return h
}

// as returns a context whose identity is the named actor.
func (h *harness) as(name string) context.Context {
	return identity.NewContext(h.ctx, identity.Identity{Name: name, Source: identity.SourceUser})
}

func (h *harness) write(parents []git.ObjectID, branch string, contents map[string]string) git.ObjectID {
	h.t.Helper()

	var entries []gittest.TreeEntry
	for path, content := range contents {
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: path, Content: content})
	}

	opts := []gittest.WriteCommitOption{
		gittest.WithParents(parents...),
		gittest.WithTreeEntries(entries...),
	}
	if branch != "" {
		opts = append(opts, gittest.WithBranch(branch))
	}

	return gittest.WriteCommit(h.t, h.cfg, h.repoPath, opts...)
}

const (
	codeV1 = "package a\nfunc A() {}\n"
	codeV2 = "package a\nfunc A() { _ = 1 }\n"
	codeV3 = "package a\nfunc A() { _ = 2 }\n"
	// commentV2 only adds a comment to codeV1.
	commentV2 = "package a\nfunc A() {}\n// comment\n"
)

// fixture writes a main branch with a.go, and a feature commit on top of it
// that changes a.go as given by feature.
func (h *harness) fixture(feature string) (base, head git.ObjectID) {
	h.t.Helper()

	base = h.write(nil, "main", map[string]string{"a.go": codeV1})
	head = h.write([]git.ObjectID{base}, "", map[string]string{"a.go": feature})
	return base, head
}

func (h *harness) create(head git.ObjectID) *gitalypb.Review {
	h.t.Helper()

	resp, err := h.client.CreateReview(h.ctx, &gitalypb.CreateReviewRequest{
		Repository:   h.repo,
		TargetBranch: []byte("main"),
		HeadCommitId: head.String(),
		Title:        []byte("change a"),
	})
	require.NoError(h.t, err)
	return resp.GetReview()
}

func requireCode(t testing.TB, err error, code string) {
	t.Helper()
	require.Error(t, err)
	require.Contains(t, err.Error(), code)
}

func TestSpecCreateReviewClassifiesAndEvaluates(t *testing.T) {
	h := setupHarness(t)
	base, head := h.fixture(codeV2)

	review := h.create(head)

	require.Equal(t, "1", review.GetId())
	require.Equal(t, "main", string(review.GetTargetBranch()))
	require.Equal(t, base.String(), review.GetBaseCommitId(), "base is the merge base with the branch tip")
	require.Equal(t, head.String(), review.GetHeadCommitId())
	require.Equal(t, "anonymous", review.GetAuthor())
	require.Equal(t, gitalypb.Review_STATE_OPEN_UNSPECIFIED, review.GetState())

	require.Len(t, review.GetFiles(), 1)
	require.Equal(t, []byte("a.go"), review.GetFiles()[0].GetPath())
	require.Equal(t, gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, review.GetFiles()[0].GetClass())
	require.NotEmpty(t, review.GetFiles()[0].GetReason())

	// The default rule needs one approval for code.
	require.Len(t, review.GetRequirements(), 1)
	require.Equal(t, "code", review.GetRequirements()[0].GetRule())
	require.Equal(t, int32(1), review.GetRequirements()[0].GetRequiredApprovals())
	require.False(t, review.GetApproved())
}

func TestSpecCreateReviewCommentOnlyNeedsNoApproval(t *testing.T) {
	h := setupHarness(t)
	_, head := h.fixture(commentV2)

	review := h.create(head)

	require.Equal(t, gitalypb.ReviewFile_CHANGE_CLASS_COMMENT_ONLY, review.GetFiles()[0].GetClass())
	require.Empty(t, review.GetRequirements(), "the default rules only require code and config")
	require.True(t, review.GetApproved())
}

func TestSpecCreateReviewExplicitBase(t *testing.T) {
	h := setupHarness(t)
	base, head := h.fixture(codeV2)

	resp, err := h.client.CreateReview(h.ctx, &gitalypb.CreateReviewRequest{
		Repository:   h.repo,
		TargetBranch: []byte("main"),
		HeadCommitId: head.String(),
		BaseCommitId: base.String(),
	})
	require.NoError(t, err)
	require.Equal(t, base.String(), resp.GetReview().GetBaseCommitId())
}

func TestSpecCreateReviewErrors(t *testing.T) {
	h := setupHarness(t)
	_, head := h.fixture(codeV2)
	blob := gittest.WriteBlob(t, h.cfg, h.repoPath, []byte("not a commit"))
	unrelated := h.write(nil, "", map[string]string{"b.go": "package b\n"})

	tests := []struct {
		name string
		req  *gitalypb.CreateReviewRequest
		code string
	}{
		{
			name: "missing repository",
			req: &gitalypb.CreateReviewRequest{
				Repository:   &gitalypb.Repository{StorageName: h.repo.GetStorageName(), RelativePath: "missing.git"},
				TargetBranch: []byte("main"), HeadCommitId: head.String(),
			},
			code: "NotFound",
		},
		{
			name: "invalid branch name",
			req:  &gitalypb.CreateReviewRequest{Repository: h.repo, TargetBranch: []byte("-main"), HeadCommitId: head.String()},
			code: "InvalidArgument",
		},
		{
			name: "branch with revision syntax",
			req:  &gitalypb.CreateReviewRequest{Repository: h.repo, TargetBranch: []byte("main:a.go"), HeadCommitId: head.String()},
			code: "InvalidArgument",
		},
		{
			name: "unknown branch",
			req:  &gitalypb.CreateReviewRequest{Repository: h.repo, TargetBranch: []byte("nope"), HeadCommitId: head.String()},
			code: "NotFound",
		},
		{
			name: "missing head",
			req:  &gitalypb.CreateReviewRequest{Repository: h.repo, TargetBranch: []byte("main")},
			code: "InvalidArgument",
		},
		{
			name: "head is not a commit",
			req:  &gitalypb.CreateReviewRequest{Repository: h.repo, TargetBranch: []byte("main"), HeadCommitId: blob.String()},
			code: "InvalidArgument",
		},
		{
			name: "unknown head",
			req:  &gitalypb.CreateReviewRequest{Repository: h.repo, TargetBranch: []byte("main"), HeadCommitId: strings.Repeat("a", 40)},
			code: "NotFound",
		},
		{
			name: "unknown base",
			req: &gitalypb.CreateReviewRequest{
				Repository: h.repo, TargetBranch: []byte("main"),
				HeadCommitId: head.String(), BaseCommitId: strings.Repeat("b", 40),
			},
			code: "NotFound",
		},
		{
			name: "no common ancestor",
			req:  &gitalypb.CreateReviewRequest{Repository: h.repo, TargetBranch: []byte("main"), HeadCommitId: unrelated.String()},
			code: "FailedPrecondition",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.client.CreateReview(h.ctx, tt.req)
			requireCode(t, err, tt.code)
		})
	}
}

func TestSpecGetReview(t *testing.T) {
	h := setupHarness(t)
	_, head := h.fixture(codeV2)
	created := h.create(head)

	resp, err := h.client.GetReview(h.ctx, &gitalypb.GetReviewRequest{Repository: h.repo, Id: created.GetId()})
	require.NoError(t, err)
	require.Equal(t, created.GetHeadCommitId(), resp.GetReview().GetHeadCommitId())
	require.Len(t, resp.GetReview().GetRequirements(), 1, "requirements are recomputed on read")

	_, err = h.client.GetReview(h.ctx, &gitalypb.GetReviewRequest{Repository: h.repo, Id: "99"})
	requireCode(t, err, "NotFound")

	_, err = h.client.GetReview(h.ctx, &gitalypb.GetReviewRequest{Repository: h.repo})
	requireCode(t, err, "InvalidArgument")
}

func TestSpecListReviews(t *testing.T) {
	h := setupHarness(t)
	_, head := h.fixture(codeV2)
	first := h.create(head)
	second := h.create(head)

	_, err := h.client.AddReviewComment(h.ctx, &gitalypb.AddReviewCommentRequest{
		Repository: h.repo, Id: first.GetId(), Body: []byte("hello"),
	})
	require.NoError(t, err)

	reviews := listReviews(t, h, &gitalypb.ListReviewsRequest{Repository: h.repo})
	require.Len(t, reviews, 2)
	require.Equal(t, second.GetId(), reviews[0].GetId(), "newest first")
	require.Equal(t, first.GetId(), reviews[1].GetId())
	require.Empty(t, reviews[1].GetComments(), "listings carry no comments")

	limited := listReviews(t, h, &gitalypb.ListReviewsRequest{Repository: h.repo, Limit: 1})
	require.Len(t, limited, 1)
}

func TestSpecListReviewsTargetBranchAndClosed(t *testing.T) {
	h := setupHarness(t)
	_, head := h.fixture(codeV2)
	closed := h.create(head)
	_, err := h.client.CloseReview(h.ctx, &gitalypb.CloseReviewRequest{Repository: h.repo, Id: closed.GetId()})
	require.NoError(t, err)
	open := h.create(head)

	reviews := listReviews(t, h, &gitalypb.ListReviewsRequest{Repository: h.repo, TargetBranch: []byte("main")})
	require.Len(t, reviews, 1)
	require.Equal(t, open.GetId(), reviews[0].GetId())

	none := listReviews(t, h, &gitalypb.ListReviewsRequest{Repository: h.repo, TargetBranch: []byte("other")})
	require.Empty(t, none)

	all := listReviews(t, h, &gitalypb.ListReviewsRequest{Repository: h.repo, IncludeClosed: true})
	require.Len(t, all, 2)
}

func listReviews(t *testing.T, h *harness, req *gitalypb.ListReviewsRequest) []*gitalypb.Review {
	t.Helper()

	stream, err := h.client.ListReviews(h.ctx, req)
	require.NoError(t, err)

	var reviews []*gitalypb.Review
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return reviews
		}
		require.NoError(t, err)
		reviews = append(reviews, resp.GetReviews()...)
	}
}

func TestSpecUpdateReviewCommentOnlyKeepsApproval(t *testing.T) {
	h := setupHarness(t)
	_, head1 := h.fixture(codeV1)
	review := h.create(head1)
	require.Empty(t, review.GetRequirements(), "unchanged code needs no approval here")

	// A code change needs approval; the reviewer approves it.
	head2 := h.write([]git.ObjectID{head1}, "", map[string]string{"a.go": codeV2})
	review = h.updateReview(review.GetId(), head2, "")
	require.False(t, review.GetApproved())
	h.approve(review.GetId(), head2, "reviewer")
	review = h.getReview(review.GetId())
	require.True(t, review.GetApproved())

	// A comment-only change on top keeps the approval.
	head3 := h.write([]git.ObjectID{head2}, "", map[string]string{"a.go": codeV2 + "// comment\n"})
	review = h.updateReview(review.GetId(), head3, "")
	require.True(t, review.GetApproved(), "a comment fix does not need re-approval")
	require.Equal(t, head3.String(), review.GetHeadCommitId())
	require.Len(t, review.GetVerdicts(), 1)
	require.True(t, review.GetVerdicts()[0].GetCurrent())
}

func TestSpecUpdateReviewCodeChangeDropsApproval(t *testing.T) {
	h := setupHarness(t)
	_, head1 := h.fixture(codeV2)
	review := h.create(head1)
	h.approve(review.GetId(), head1, "reviewer")
	require.True(t, h.getReview(review.GetId()).GetApproved())

	head2 := h.write([]git.ObjectID{head1}, "", map[string]string{"a.go": codeV3})
	review = h.updateReview(review.GetId(), head2, "")

	require.False(t, review.GetApproved(), "a code change needs re-approval")
	require.Len(t, review.GetVerdicts(), 1, "verdicts are kept")
	require.False(t, review.GetVerdicts()[0].GetCurrent())
}

func TestSpecUpdateReviewCommentThenCodeNeedsApproval(t *testing.T) {
	h := setupHarness(t)
	_, head1 := h.fixture(codeV1)
	review := h.create(head1)

	head2 := h.write([]git.ObjectID{head1}, "", map[string]string{"a.go": codeV1 + "// comment\n"})
	review = h.updateReview(review.GetId(), head2, "")
	h.approve(review.GetId(), head2, "reviewer")
	require.True(t, h.getReview(review.GetId()).GetApproved())

	// The approval given on head2 is judged against the change since head2.
	head3 := h.write([]git.ObjectID{head2}, "", map[string]string{"a.go": codeV2})
	review = h.updateReview(review.GetId(), head3, "")
	require.False(t, review.GetApproved())
}

func TestSpecUpdateReviewErrors(t *testing.T) {
	h := setupHarness(t)
	_, head1 := h.fixture(codeV1)
	review := h.create(head1)
	head2 := h.write([]git.ObjectID{head1}, "", map[string]string{"a.go": codeV2})

	_, err := h.client.UpdateReview(h.ctx, &gitalypb.UpdateReviewRequest{
		Repository: h.repo, Id: review.GetId(), HeadCommitId: head2.String(),
		ExpectedHeadCommitId: strings.Repeat("c", 40),
	})
	requireCode(t, err, "FailedPrecondition")

	_, err = h.client.UpdateReview(h.ctx, &gitalypb.UpdateReviewRequest{
		Repository: h.repo, Id: review.GetId(),
	})
	requireCode(t, err, "InvalidArgument")

	_, err = h.client.UpdateReview(h.ctx, &gitalypb.UpdateReviewRequest{
		Repository: h.repo, Id: "77", HeadCommitId: head2.String(),
	})
	requireCode(t, err, "NotFound")

	// The compare-and-swap with the current head succeeds.
	h.updateReview(review.GetId(), head2, head1.String())

	h.closeReview(review.GetId(), false)
	_, err = h.client.UpdateReview(h.ctx, &gitalypb.UpdateReviewRequest{
		Repository: h.repo, Id: review.GetId(), HeadCommitId: head1.String(),
	})
	requireCode(t, err, "FailedPrecondition")
}

func TestSpecAddReviewComment(t *testing.T) {
	h := setupHarness(t)
	_, head := h.fixture(codeV2)
	review := h.create(head)

	resp, err := h.client.AddReviewComment(h.ctx, &gitalypb.AddReviewCommentRequest{
		Repository: h.repo, Id: review.GetId(), Body: []byte("nit"), Path: []byte("a.go"), Line: 2,
	})
	require.NoError(t, err)
	require.Equal(t, "1", resp.GetComment().GetId())
	require.Equal(t, "anonymous", resp.GetComment().GetActor())
	require.Equal(t, head.String(), resp.GetComment().GetCommitId())
	require.Equal(t, int32(2), resp.GetComment().GetLine())

	resp, err = h.client.AddReviewComment(h.ctx, &gitalypb.AddReviewCommentRequest{
		Repository: h.repo, Id: review.GetId(), Body: []byte("general"),
	})
	require.NoError(t, err)
	require.Equal(t, "2", resp.GetComment().GetId())

	got := h.getReview(review.GetId())
	require.Len(t, got.GetComments(), 2)
}

func TestSpecAddReviewCommentErrors(t *testing.T) {
	h := setupHarness(t)
	_, head := h.fixture(codeV2)
	review := h.create(head)

	tests := []struct {
		name string
		req  *gitalypb.AddReviewCommentRequest
		code string
	}{
		{
			name: "empty body",
			req:  &gitalypb.AddReviewCommentRequest{Id: review.GetId()},
			code: "InvalidArgument",
		},
		{
			name: "body too large",
			req:  &gitalypb.AddReviewCommentRequest{Id: review.GetId(), Body: bytes.Repeat([]byte("x"), maxBodyBytes+1)},
			code: "InvalidArgument",
		},
		{
			name: "negative line",
			req:  &gitalypb.AddReviewCommentRequest{Id: review.GetId(), Body: []byte("x"), Path: []byte("a.go"), Line: -1},
			code: "InvalidArgument",
		},
		{
			name: "path is not a file of the review",
			req:  &gitalypb.AddReviewCommentRequest{Id: review.GetId(), Body: []byte("x"), Path: []byte("b.go")},
			code: "InvalidArgument",
		},
		{
			name: "unknown review",
			req:  &gitalypb.AddReviewCommentRequest{Id: "55", Body: []byte("x")},
			code: "NotFound",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.Repository = h.repo
			_, err := h.client.AddReviewComment(h.ctx, tt.req)
			requireCode(t, err, tt.code)
		})
	}

	h.closeReview(review.GetId(), false)
	_, err := h.client.AddReviewComment(h.ctx, &gitalypb.AddReviewCommentRequest{
		Repository: h.repo, Id: review.GetId(), Body: []byte("late"),
	})
	requireCode(t, err, "FailedPrecondition")
}

func TestSpecConcurrentAddReviewCommentsAllStored(t *testing.T) {
	h := setupHarness(t)
	_, head := h.fixture(codeV2)
	review := h.create(head)

	const n = 20
	idsCh := make(chan string, n)
	errCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := h.client.AddReviewComment(h.ctx, &gitalypb.AddReviewCommentRequest{
				Repository: h.repo, Id: review.GetId(), Body: []byte("concurrent"),
			})
			if err != nil {
				errCh <- err
				return
			}
			idsCh <- resp.GetComment().GetId()
		}()
	}
	wg.Wait()
	close(idsCh)
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}

	seen := map[string]bool{}
	for id := range idsCh {
		require.False(t, seen[id], "duplicate comment id %s", id)
		seen[id] = true
	}
	require.Len(t, seen, n)
	require.Len(t, h.getReview(review.GetId()).GetComments(), n)
}

func TestSpecSubmitReviewVerdict(t *testing.T) {
	h := setupHarness(t)
	_, head := h.fixture(codeV2)
	review := h.create(head)

	// The author's own approval does not count for the default code rule.
	resp, err := h.client.SubmitReviewVerdict(h.ctx, &gitalypb.SubmitReviewVerdictRequest{
		Repository: h.repo, Id: review.GetId(), Kind: gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED,
		HeadCommitId: head.String(), Body: []byte("lgtm"),
	})
	require.NoError(t, err)
	require.False(t, resp.GetReview().GetApproved())
	require.Len(t, resp.GetReview().GetVerdicts(), 1)
	require.True(t, resp.GetReview().GetVerdicts()[0].GetCurrent())

	h.approve(review.GetId(), head, "reviewer")
	require.True(t, h.getReview(review.GetId()).GetApproved())

	// The latest verdict of an actor counts: a later request for changes blocks.
	_, err = h.client.SubmitReviewVerdict(h.ctx, &gitalypb.SubmitReviewVerdictRequest{
		Repository: h.repo, Id: review.GetId(), Kind: gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES,
		HeadCommitId: head.String(),
	})
	require.NoError(t, err)
	require.False(t, h.getReview(review.GetId()).GetApproved())
}

func TestSpecSubmitReviewVerdictErrors(t *testing.T) {
	h := setupHarness(t)
	_, head1 := h.fixture(codeV1)
	review := h.create(head1)
	head2 := h.write([]git.ObjectID{head1}, "", map[string]string{"a.go": codeV2})
	h.updateReview(review.GetId(), head2, "")

	tests := []struct {
		name string
		req  *gitalypb.SubmitReviewVerdictRequest
		code string
	}{
		{
			name: "stale head",
			req: &gitalypb.SubmitReviewVerdictRequest{
				Id: review.GetId(), Kind: gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED, HeadCommitId: head1.String(),
			},
			code: "FailedPrecondition",
		},
		{
			name: "missing head",
			req:  &gitalypb.SubmitReviewVerdictRequest{Id: review.GetId(), Kind: gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED},
			code: "InvalidArgument",
		},
		{
			name: "unknown kind",
			req: &gitalypb.SubmitReviewVerdictRequest{
				Id: review.GetId(), Kind: gitalypb.ReviewVerdict_Kind(9), HeadCommitId: head2.String(),
			},
			code: "InvalidArgument",
		},
		{
			name: "unknown review",
			req: &gitalypb.SubmitReviewVerdictRequest{
				Id: "66", Kind: gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED, HeadCommitId: head2.String(),
			},
			code: "NotFound",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.Repository = h.repo
			_, err := h.client.SubmitReviewVerdict(h.ctx, tt.req)
			requireCode(t, err, tt.code)
		})
	}
}

func TestSpecCloseReview(t *testing.T) {
	h := setupHarness(t)
	_, head := h.fixture(codeV2)
	review := h.create(head)

	closed := h.closeReview(review.GetId(), false)
	require.Equal(t, gitalypb.Review_STATE_CLOSED, closed.GetState())

	_, err := h.client.CloseReview(h.ctx, &gitalypb.CloseReviewRequest{Repository: h.repo, Id: review.GetId()})
	requireCode(t, err, "FailedPrecondition")

	landed := h.create(head)
	require.Equal(t, gitalypb.Review_STATE_LANDED, h.closeReview(landed.GetId(), true).GetState())

	_, err = h.client.CloseReview(h.ctx, &gitalypb.CloseReviewRequest{Repository: h.repo, Id: "88"})
	requireCode(t, err, "NotFound")
}

// approve submits an approval as actor on the given head, calling the server
// directly so that the context carries the actor's identity.
func (h *harness) approve(id string, head git.ObjectID, actor string) {
	h.t.Helper()

	_, err := h.srv.SubmitReviewVerdict(h.as(actor), &gitalypb.SubmitReviewVerdictRequest{
		Repository: h.repo, Id: id, Kind: gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED, HeadCommitId: head.String(),
	})
	require.NoError(h.t, err)
}

func (h *harness) updateReview(id string, head git.ObjectID, expected string) *gitalypb.Review {
	h.t.Helper()

	resp, err := h.client.UpdateReview(h.ctx, &gitalypb.UpdateReviewRequest{
		Repository: h.repo, Id: id, HeadCommitId: head.String(), ExpectedHeadCommitId: expected,
	})
	require.NoError(h.t, err)
	return resp.GetReview()
}

func (h *harness) getReview(id string) *gitalypb.Review {
	h.t.Helper()

	resp, err := h.client.GetReview(h.ctx, &gitalypb.GetReviewRequest{Repository: h.repo, Id: id})
	require.NoError(h.t, err)
	return resp.GetReview()
}

func (h *harness) closeReview(id string, landed bool) *gitalypb.Review {
	h.t.Helper()

	resp, err := h.client.CloseReview(h.ctx, &gitalypb.CloseReviewRequest{Repository: h.repo, Id: id, Landed: landed})
	require.NoError(h.t, err)
	return resp.GetReview()
}
