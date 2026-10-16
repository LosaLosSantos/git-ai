package review

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// specRegister registers the services the spec tests need. setup.RegisterAll
// cannot be used from inside package review (import cycle).
func specRegister(srv *grpc.Server, deps *service.Dependencies) {
	gitalypb.RegisterReviewServiceServer(srv, NewServer(deps))
	gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
}

// specStart runs a Gitaly server with all services and returns a ReviewService
// client, a context, a fresh repository and its on-disk path.
func specStart(t *testing.T, cfg config.Cfg) (gitalypb.ReviewServiceClient, context.Context, *gitalypb.Repository, string) {
	t.Helper()
	ctx := testhelper.Context(t)
	addr := testserver.RunGitalyServer(t, cfg, specRegister)
	// gittest.CreateRepository dials cfg.SocketPath.
	cfg.SocketPath = addr
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	return gitalypb.NewReviewServiceClient(conn), ctx, repo, repoPath
}

// Spec: CreateReview with an unknown target branch → NotFound; GetReview with
// an unknown id → NotFound.
func TestSpecReviewUnknownBranchAndIDAreNotFound(t *testing.T) {
	cfg := testcfg.Build(t)
	client, ctx, repo, repoPath := specStart(t, cfg)
	head := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("feature"))

	_, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository:   repo,
		TargetBranch: []byte("does-not-exist"),
		HeadCommitId: head.String(),
		Title:        []byte("t"),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("CreateReview unknown branch: want NotFound, got %v", err)
	}

	_, err = client.GetReview(ctx, &gitalypb.GetReviewRequest{Repository: repo, Id: "999999"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetReview unknown id: want NotFound, got %v", err)
	}
}

// Spec: files are the paths changed between base and head, each classified;
// a doc file is DOCS and a Go source change is CODE.
func TestSpecCreateReviewClassifiesChangedFiles(t *testing.T) {
	cfg := testcfg.Build(t)
	client, ctx, repo, repoPath := specStart(t, cfg)

	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch("main"),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: "package a\n"},
		),
	)
	// WithTreeEntries does not accept nested paths, so docs/ is its own tree.
	docsTree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
		{Mode: "100644", Path: "x.md", Content: "# doc\n"},
	})
	head := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithBranch("feature"),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a.go", Content: "package a\n\nvar X = 1\n"},
			gittest.TreeEntry{Mode: "040000", Path: "docs", OID: docsTree},
		),
	)

	created, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository:   repo,
		TargetBranch: []byte("main"),
		HeadCommitId: head.String(),
		Title:        []byte("classify"),
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	classes := map[string]gitalypb.ReviewFile_ChangeClass{}
	for _, f := range created.GetReview().GetFiles() {
		classes[string(f.GetPath())] = f.GetClass()
	}
	if len(classes) != 2 {
		t.Fatalf("want 2 changed files, got %v", classes)
	}
	if classes["a.go"] == gitalypb.ReviewFile_CHANGE_CLASS_COMMENT_ONLY {
		t.Fatalf("a.go gained a declaration and must not be COMMENT_ONLY")
	}
	if classes["docs/x.md"] != gitalypb.ReviewFile_CHANGE_CLASS_DOCS {
		t.Fatalf("docs/x.md: want DOCS, got %v", classes["docs/x.md"])
	}
}

// Spec: AddReviewComment body ≤ 64 KiB and line ≥ 0; violations → InvalidArgument.
func TestSpecAddCommentValidation(t *testing.T) {
	cfg := testcfg.Build(t)
	client, ctx, repo, repoPath := specStart(t, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	created, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository: repo, TargetBranch: []byte("main"), HeadCommitId: base.String(), Title: []byte("c"),
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	tooBig := strings.Repeat("x", 64*1024+1)
	_, err = client.AddReviewComment(ctx, &gitalypb.AddReviewCommentRequest{
		Repository: repo, Id: created.GetReview().GetId(), Body: []byte(tooBig),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("body over 64 KiB: want InvalidArgument, got %v", err)
	}

	_, err = client.AddReviewComment(ctx, &gitalypb.AddReviewCommentRequest{
		Repository: repo, Id: created.GetReview().GetId(), Body: []byte("ok"), Line: -1,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("negative line: want InvalidArgument, got %v", err)
	}
}

// Spec: SubmitReviewVerdict requires head_commit_id to equal the current head;
// otherwise FailedPrecondition.
func TestSpecSubmitVerdictRequiresCurrentHead(t *testing.T) {
	cfg := testcfg.Build(t)
	client, ctx, repo, repoPath := specStart(t, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	created, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository: repo, TargetBranch: []byte("main"), HeadCommitId: base.String(), Title: []byte("v"),
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	stale := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))
	_, err = client.SubmitReviewVerdict(ctx, &gitalypb.SubmitReviewVerdictRequest{
		Repository:   repo,
		Id:           created.GetReview().GetId(),
		HeadCommitId: stale.String(),
		Kind:         gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("verdict on non-current head: want FailedPrecondition, got %v", err)
	}
}

// Spec: CloseReview moves the review to a closed state; closed reviews reject
// mutations with FailedPrecondition.
func TestSpecClosedReviewRejectsMutations(t *testing.T) {
	cfg := testcfg.Build(t)
	client, ctx, repo, repoPath := specStart(t, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	created, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository: repo, TargetBranch: []byte("main"), HeadCommitId: base.String(), Title: []byte("closed"),
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	if _, err := client.CloseReview(ctx, &gitalypb.CloseReviewRequest{
		Repository: repo, Id: created.GetReview().GetId(),
	}); err != nil {
		t.Fatalf("CloseReview: %v", err)
	}

	_, err = client.AddReviewComment(ctx, &gitalypb.AddReviewCommentRequest{
		Repository: repo, Id: created.GetReview().GetId(), Body: []byte("late"),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("comment on closed review: want FailedPrecondition, got %v", err)
	}
}

// Spec: 20 goroutines commenting concurrently → all comments stored.
func TestSpecConcurrentCommentsAllStored(t *testing.T) {
	cfg := testcfg.Build(t)
	client, ctx, repo, repoPath := specStart(t, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	created, err := client.CreateReview(ctx, &gitalypb.CreateReviewRequest{
		Repository: repo, TargetBranch: []byte("main"), HeadCommitId: base.String(), Title: []byte("concurrent"),
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.AddReviewComment(ctx, &gitalypb.AddReviewCommentRequest{
				Repository: repo, Id: created.GetReview().GetId(), Body: []byte("c"),
			})
			if err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent AddReviewComment: %v", err)
	}

	got, err := client.GetReview(ctx, &gitalypb.GetReviewRequest{Repository: repo, Id: created.GetReview().GetId()})
	if err != nil {
		t.Fatalf("GetReview: %v", err)
	}
	if len(got.GetReview().GetComments()) != n {
		t.Fatalf("want %d stored comments, got %d", n, len(got.GetReview().GetComments()))
	}
}
