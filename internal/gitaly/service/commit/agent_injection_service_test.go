package commit

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/blob"
	"github.com/feder-cr/git-ai/internal/gitaly/service/ref"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestRevisionInjection_rpcsRefuseLineBreaks: revisions travel to git one per line on its stdin, so
// a revision with a line feed, a carriage return or a NUL is more than one revision to git, or one
// that the filter that leaves refs/graft/ out of an agent's walks reads differently from git. The
// pseudo-revision patterns after "=" took any bytes, and the request check read the whole as one
// name: "--glob=x\nrefs/graft/green/main" named a hidden reference without the check seeing it.
// Every RPC that sends revisions to git over stdin refuses them now, for every caller, with
// InvalidArgument, and no proposal is returned.
func TestRevisionInjection_rpcsRefuseLineBreaks(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterCommitServiceServer(srv, NewServer(deps))
		gitalypb.RegisterBlobServiceServer(srv, blob.NewServer(deps))
		gitalypb.RegisterRefServiceServer(srv, ref.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	}, testserver.WithInterceptors(agentInterceptors))
	conn := dial(t, cfg.SocketPath)
	commits := gitalypb.NewCommitServiceClient(conn)
	blobs := gitalypb.NewBlobServiceClient(conn)
	refs := gitalypb.NewRefServiceClient(conn)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	main := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a\n"}))
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(main),
		gittest.WithReference("refs/graft/proposals/0123456789abcdef/1"), gittest.WithMessage("graft: proposal\n"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "proposed by a model\n"}))
	agent := metadata.AppendToOutgoingContext(ctx, testAgentHeader, "agent-a")

	drain := func(recv func() error) error {
		for {
			if err := recv(); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
	}
	rpcs := map[string]func(context.Context, ...string) error{
		"ListCommits": func(ctx context.Context, revs ...string) error {
			stream, err := commits.ListCommits(ctx, &gitalypb.ListCommitsRequest{Repository: repo, Revisions: revs})
			require.NoError(t, err)
			return drain(func() error { _, err := stream.Recv(); return err })
		},
		"ListBlobs": func(ctx context.Context, revs ...string) error {
			stream, err := blobs.ListBlobs(ctx, &gitalypb.ListBlobsRequest{Repository: repo, Revisions: revs, Limit: 100})
			require.NoError(t, err)
			return drain(func() error { _, err := stream.Recv(); return err })
		},
		"ListLFSPointers": func(ctx context.Context, revs ...string) error {
			stream, err := blobs.ListLFSPointers(ctx, &gitalypb.ListLFSPointersRequest{Repository: repo, Revisions: revs, Limit: 100})
			require.NoError(t, err)
			return drain(func() error { _, err := stream.Recv(); return err })
		},
		"GetTagSignatures": func(ctx context.Context, revs ...string) error {
			stream, err := refs.GetTagSignatures(ctx, &gitalypb.GetTagSignaturesRequest{Repository: repo, TagRevisions: revs})
			require.NoError(t, err)
			return drain(func() error { _, err := stream.Recv(); return err })
		},
	}
	injections := map[string][]string{
		"CR after --all":            {"--glob=x\n--all\r", "main"},
		"NUL after --all":           {"--glob=x\n--all\x00", "main"},
		"name after a line feed":    {"--glob=x\nrefs/graft/proposals/0123456789abcdef/1"},
		"short name after a line":   {"--branches=x\ngraft/proposals/0123456789abcdef/1"},
		"tags pattern, then a name": {"--tags=x\nrefs/graft/green/main"},
		"CR alone in the pattern":   {"--glob=refs/*\r--all"},
		"NUL in the pattern":        {"--glob=refs/*\x00"},
	}

	for name, call := range rpcs {
		for desc, revs := range injections {
			for who, callCtx := range map[string]context.Context{"agent": agent, "other caller": ctx} {
				err := call(callCtx, revs...)
				require.Error(t, err, "%s %s as %s", name, desc, who)
				require.Equal(t, codes.InvalidArgument, status.Code(err), "%s %s as %s: %v", name, desc, who, err)
			}
		}
		// Control: an ordinary pattern is not refused.
		require.NoError(t, call(agent, "--glob=refs/heads/*"), name)
	}
}
