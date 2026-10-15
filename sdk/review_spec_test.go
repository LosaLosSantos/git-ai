package sdk

// Black-box spec tests for WP46 (review in SDK). Written from PLAN5.md only.
//
// ASSUMPTIONS (the spec names the SDK methods but not their exact Go
// signatures, nor the Repo constructor; the implementation must match these
// or the orchestrator must adapt the calls):
//   - newSpecRepo(t) returns an *sdk Repo connected to a testserver, plus the
//     on-disk repository path. Replace its body with the existing sdk test
//     helper used by other sdk tests.
//   - CreateReview(ctx, branch, base, head, title, description string) (*gitalypb.Review, error)
//     (base == "" means merge-base of head and branch tip)
//   - GetReview(ctx, id string) (*gitalypb.Review, error)
//   - ListReviews(ctx, includeClosed bool) ([]*gitalypb.Review, error)
//   - UpdateReview(ctx, id, expectedHead, newHead string) (*gitalypb.Review, error)
//   - CommentReview(ctx, id, body, path string, line int32) (*gitalypb.Review, error)
//   - ApproveReview(ctx, id, body string) (*gitalypb.Review, error)
//   - RequestChanges(ctx, id, body string) (*gitalypb.Review, error)
//   - CloseReview(ctx, id string, landed bool) (*gitalypb.Review, error)
//   - withSpecActor(ctx, name) attaches an identity actor (the server reads it
//     via identity.Actor(ctx, "anonymous")); replace with the real helper.
//   - Errors carry gRPC status codes (status.Code(err)).

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service/setup"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

const specAuthor = "spec-author"
const specReviewer = "spec-reviewer"

// specSetup creates a repository with main (base) and a feature branch that
// changes a Go code file. It returns the repo, the repo path and the feature
// commit ID.
func specSetup(t *testing.T) (context.Context, *Repo, string, string) {
	t.Helper()
	ctx := testhelper.Context(t)
	repo, repoPath := newSpecRepo(t)
	cfg := specCfg(repoPath)

	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch("main"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "main.go", Content: "package main\n\n// v1\nfunc a() {}\n"}),
	)
	head := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithBranch("feature"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "main.go", Content: "package main\n\nfunc a() { println(1) }\n"}),
	)
	return withSpecActor(ctx, specAuthor), repo, repoPath, head.String()
}

func specCode(err error) codes.Code { return status.Code(err) }

// newSpecRepo is a PLACEHOLDER. The orchestrator must replace it with the
// sdk package's existing test helper that starts a testserver and returns a
// connected *Repo and the repository's on-disk path.
//
// Harness wiring (gate): the server runs with one per-agent token for each
// spec actor, and the review RPCs are allowed for agents. withSpecActor marks
// the context, and a client interceptor sends that actor's token.
func newSpecRepo(t *testing.T) (*Repo, string) {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Auth.Token = "spec-shared-secret" // repository setup authenticates with it
	cfg.Graft.Agents = []config.GraftAgent{
		{Name: specAuthor, Token: specActorToken(specAuthor)},
		{Name: specReviewer, Token: specActorToken(specReviewer)},
	}
	cfg.Graft.AgentRPCs = []string{
		gitalypb.ReviewService_CreateReview_FullMethodName,
		gitalypb.ReviewService_GetReview_FullMethodName,
		gitalypb.ReviewService_ListReviews_FullMethodName,
		gitalypb.ReviewService_UpdateReview_FullMethodName,
		gitalypb.ReviewService_AddReviewComment_FullMethodName,
		gitalypb.ReviewService_SubmitReviewVerdict_FullMethodName,
		gitalypb.ReviewService_CloseReview_FullMethodName,
	}
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll)
	cfg.SocketPath = addr

	setupCtx := testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))
	repoProto, repoPath := gittest.CreateRepository(t, setupCtx, cfg)

	client, err := Dial(ctx, addr, WithStorage(cfg.Storages[0].Name), WithGRPCOptions(
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(specActorOutgoing(ctx), method, req, reply, cc, opts...)
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(specActorOutgoing(ctx), desc, cc, method, opts...)
		}),
	))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	specCfgs.Store(repoPath, cfg)
	t.Cleanup(func() { specCfgs.Delete(repoPath) })
	return client.Repo(repoProto.GetRelativePath()), repoPath
}

// specCfgs maps the repository path of newSpecRepo to its server config, so
// commits are written with the config of the server that owns the repository.
var specCfgs sync.Map

func specCfg(repoPath string) config.Cfg {
	cfg, _ := specCfgs.Load(repoPath)
	return cfg.(config.Cfg)
}

type specActorKey struct{}

func specActorToken(name string) string { return "spec-token-" + name }

func specActorOutgoing(ctx context.Context) context.Context {
	if name, ok := ctx.Value(specActorKey{}).(string); ok {
		return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+specActorToken(name))
	}
	return ctx
}

// withSpecActor attaches an identity actor to ctx (see newSpecRepo).
func withSpecActor(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, specActorKey{}, name)
}

func TestSpecCreateReviewUnknownBranchNotFound(t *testing.T) {
	ctx, repo, _, head := specSetup(t)

	_, err := repo.CreateReview(ctx, CreateReviewOptions{TargetBranch: "does-not-exist", Head: head, Title: "title"})
	if specCode(err) != codes.NotFound {
		t.Fatalf("expected NotFound for missing branch, got %v", err)
	}
}

func TestSpecCreateReviewCodeChangeRequiresApproval(t *testing.T) {
	ctx, repo, _, head := specSetup(t)

	review, err := repo.CreateReview(ctx, CreateReviewOptions{TargetBranch: "main", Head: head, Title: "title", Description: "desc"})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if len(review.GetRequirements()) == 0 {
		t.Fatal("default rules must produce at least one requirement for a code change")
	}
	if review.GetApproved() {
		t.Fatal("a code change without any approval must not be approved")
	}
}

func TestSpecCommentBodyLimit64KiB(t *testing.T) {
	ctx, repo, _, head := specSetup(t)
	review, err := repo.CreateReview(ctx, CreateReviewOptions{TargetBranch: "main", Head: head, Title: "title"})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	exact := strings.Repeat("a", 64*1024)
	if _, err := repo.CommentReview(ctx, CommentReviewOptions{ID: review.GetId(), Body: exact}); err != nil {
		t.Fatalf("a 64 KiB body must be accepted, got %v", err)
	}

	tooBig := strings.Repeat("a", 64*1024+1)
	_, err = repo.CommentReview(ctx, CommentReviewOptions{ID: review.GetId(), Body: tooBig})
	if specCode(err) != codes.InvalidArgument {
		t.Fatalf("a body above 64 KiB must be InvalidArgument, got %v", err)
	}
}

func TestSpecCommentPathAndLineValidation(t *testing.T) {
	ctx, repo, _, head := specSetup(t)
	review, err := repo.CreateReview(ctx, CreateReviewOptions{TargetBranch: "main", Head: head, Title: "title"})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	// A path that is not one of the review files is rejected.
	_, err = repo.CommentReview(ctx, CommentReviewOptions{ID: review.GetId(), Body: "nit", Path: "not/in/review.go", Line: 1})
	if specCode(err) != codes.InvalidArgument {
		t.Fatalf("comment on a path outside the review must be InvalidArgument, got %v", err)
	}

	// A negative line is rejected.
	_, err = repo.CommentReview(ctx, CommentReviewOptions{ID: review.GetId(), Body: "nit", Path: "main.go", Line: -1})
	if specCode(err) != codes.InvalidArgument {
		t.Fatalf("negative line must be InvalidArgument, got %v", err)
	}
}

func TestSpecCommentOnlyUpdateKeepsApproval(t *testing.T) {
	ctx, repo, repoPath, head := specSetup(t)
	review, err := repo.CreateReview(ctx, CreateReviewOptions{TargetBranch: "main", Head: head, Title: "title"})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	reviewerCtx := withSpecActor(ctx, specReviewer)
	if _, err := repo.ApproveReview(reviewerCtx, review.GetId(), "lgtm"); err != nil {
		t.Fatalf("ApproveReview: %v", err)
	}

	// New head changes only a comment: an approval must survive.
	cfg := specCfg(repoPath)
	newHead := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(gittest.ResolveRevision(t, cfg, repoPath, head)),
		gittest.WithBranch("feature"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "main.go", Content: "package main\n\n// comment fixed\nfunc a() { println(1) }\n"}),
	)

	updated, err := repo.UpdateReview(ctx, UpdateReviewOptions{ID: review.GetId(), ExpectedHead: head, Head: newHead.String()})
	if err != nil {
		t.Fatalf("UpdateReview: %v", err)
	}
	if !updated.GetApproved() {
		t.Fatal("a comment-only update must keep the existing approval")
	}
}

func TestSpecCodeUpdateDropsApproval(t *testing.T) {
	ctx, repo, repoPath, head := specSetup(t)
	review, err := repo.CreateReview(ctx, CreateReviewOptions{TargetBranch: "main", Head: head, Title: "title"})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if _, err := repo.ApproveReview(withSpecActor(ctx, specReviewer), review.GetId(), "lgtm"); err != nil {
		t.Fatalf("ApproveReview: %v", err)
	}

	cfg := specCfg(repoPath)
	newHead := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(gittest.ResolveRevision(t, cfg, repoPath, head)),
		gittest.WithBranch("feature"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "main.go", Content: "package main\n\nfunc a() { println(2) }\n"}),
	)

	updated, err := repo.UpdateReview(ctx, UpdateReviewOptions{ID: review.GetId(), ExpectedHead: head, Head: newHead.String()})
	if err != nil {
		t.Fatalf("UpdateReview: %v", err)
	}
	if updated.GetApproved() {
		t.Fatal("a code change after approval must not leave the review approved")
	}
}

func TestSpecUpdateStaleExpectedHeadFailedPrecondition(t *testing.T) {
	ctx, repo, _, head := specSetup(t)
	review, err := repo.CreateReview(ctx, CreateReviewOptions{TargetBranch: "main", Head: head, Title: "title"})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	stale := strings.Repeat("0", len(head))
	_, err = repo.UpdateReview(ctx, UpdateReviewOptions{ID: review.GetId(), ExpectedHead: stale, Head: head})
	if specCode(err) != codes.FailedPrecondition {
		t.Fatalf("update with a stale expected head must be FailedPrecondition, got %v", err)
	}
}

func TestSpecLatestVerdictSupersedesEarlier(t *testing.T) {
	ctx, repo, _, head := specSetup(t)
	review, err := repo.CreateReview(ctx, CreateReviewOptions{TargetBranch: "main", Head: head, Title: "title"})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	reviewerCtx := withSpecActor(ctx, specReviewer)
	if _, err := repo.ApproveReview(reviewerCtx, review.GetId(), ""); err != nil {
		t.Fatalf("ApproveReview: %v", err)
	}
	// The same actor's later REQUEST_CHANGES supersedes the approval.
	changed, err := repo.RequestChanges(reviewerCtx, review.GetId(), "wait")
	if err != nil {
		t.Fatalf("RequestChanges: %v", err)
	}
	if changed.GetApproved() {
		t.Fatal("a later request-changes by the same actor must block approval")
	}
}

func TestSpecClosedReviewRejectsMutations(t *testing.T) {
	ctx, repo, _, head := specSetup(t)
	review, err := repo.CreateReview(ctx, CreateReviewOptions{TargetBranch: "main", Head: head, Title: "title"})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if _, err := repo.CloseReview(ctx, review.GetId(), false); err != nil {
		t.Fatalf("CloseReview: %v", err)
	}

	_, err = repo.CommentReview(ctx, CommentReviewOptions{ID: review.GetId(), Body: "late comment"})
	if specCode(err) != codes.FailedPrecondition {
		t.Fatalf("mutating a closed review must be FailedPrecondition, got %v", err)
	}

	reviews, err := repo.ListReviews(ctx, ListReviewsOptions{IncludeClosed: false})
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	for _, r := range reviews {
		if r.GetId() == review.GetId() {
			t.Fatal("a closed review must not be listed without --all (includeClosed=false)")
		}
	}
}
