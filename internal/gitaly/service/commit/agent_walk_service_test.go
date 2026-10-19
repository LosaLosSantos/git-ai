package commit

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/blob"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// testAgentHeader is the metadata that names the agent a test request is made as.
const testAgentHeader = "x-test-agent"

// agentStream is a server stream whose context carries the identity of an agent.
type agentStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s agentStream) Context() context.Context { return s.ctx }

// agentInterceptors authenticates each streaming request as the agent named in its test header, the
// way the per-agent token authentication does.
func agentInterceptors(log.Logger, localrepo.Factory) ([]grpc.UnaryServerInterceptor, []grpc.StreamServerInterceptor) {
	return nil, []grpc.StreamServerInterceptor{
		func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if md, ok := metadata.FromIncomingContext(ss.Context()); ok {
				if names := md.Get(testAgentHeader); len(names) > 0 {
					ctx := identity.NewContext(ss.Context(), identity.Identity{Name: names[0], Source: identity.SourceAgent})
					return handler(srv, agentStream{ServerStream: ss, ctx: ctx})
				}
			}
			return handler(srv, ss)
		},
	}
}

// TestAgentWalk_rpcsReachNoProposal: an agent that asks for every reference, by --all or --glob in
// the revisions of ListCommits and ListBlobs, or by FindCommits' all, gets neither the commits nor
// the blobs that only a reference under refs/graft/ reaches (a proposal, whose message names other
// agents); another caller still gets them.
func TestAgentWalk_rpcsReachNoProposal(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterCommitServiceServer(srv, NewServer(deps))
		gitalypb.RegisterBlobServiceServer(srv, blob.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	}, testserver.WithInterceptors(agentInterceptors))
	conn := dial(t, cfg.SocketPath)
	commits := gitalypb.NewCommitServiceClient(conn)
	blobs := gitalypb.NewBlobServiceClient(conn)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	main := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a\n"}))
	proposal := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(main),
		gittest.WithReference("refs/graft/proposals/0123456789abcdef/1"),
		gittest.WithMessage("graft: proposal\n\nRequested-By: agent-b\n"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "proposed by a model\n"}))
	proposedBlob := gittest.ResolveRevision(t, cfg, repoPath, proposal.String()+":a")

	agent := metadata.AppendToOutgoingContext(ctx, testAgentHeader, "agent-a")

	listCommits := func(ctx context.Context, revisions ...string) []string {
		stream, err := commits.ListCommits(ctx, &gitalypb.ListCommitsRequest{Repository: repo, Revisions: revisions})
		require.NoError(t, err)
		var ids []string
		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return ids
			}
			require.NoError(t, err)
			for _, c := range resp.GetCommits() {
				ids = append(ids, c.GetId())
			}
		}
	}
	listBlobs := func(ctx context.Context, revisions ...string) []string {
		stream, err := blobs.ListBlobs(ctx, &gitalypb.ListBlobsRequest{Repository: repo, Revisions: revisions, Limit: 100})
		require.NoError(t, err)
		var ids []string
		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return ids
			}
			require.NoError(t, err)
			for _, b := range resp.GetBlobs() {
				ids = append(ids, b.GetOid())
			}
		}
	}
	findAll := func(ctx context.Context) []string {
		stream, err := commits.FindCommits(ctx, &gitalypb.FindCommitsRequest{Repository: repo, All: true, Limit: 100})
		require.NoError(t, err)
		var ids []string
		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return ids
			}
			require.NoError(t, err)
			for _, c := range resp.GetCommits() {
				ids = append(ids, c.GetId())
			}
		}
	}

	for _, revisions := range [][]string{{"--all"}, {"--glob=refs/*"}, {"main", "--glob=refs/*"}} {
		require.Contains(t, listCommits(agent, revisions...), main.String(), revisions)
		require.NotContains(t, listCommits(agent, revisions...), proposal.String(), revisions)
		require.Contains(t, listCommits(ctx, revisions...), proposal.String(), revisions)

		require.NotContains(t, listBlobs(agent, revisions...), proposedBlob.String(), revisions)
		require.Contains(t, listBlobs(ctx, revisions...), proposedBlob.String(), revisions)
	}

	require.Equal(t, []string{main.String()}, findAll(agent))
	require.Contains(t, findAll(ctx), proposal.String())
}
