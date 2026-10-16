package search

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// setup starts a Gitaly server with the SearchService and returns its config and a client. The
// configure callback may adjust the search configuration before the server starts.
func setup(tb testing.TB, ctx context.Context, configure func(*config.Cfg)) (config.Cfg, gitalypb.SearchServiceClient) {
	cfg := testcfg.Build(tb)
	if configure != nil {
		configure(&cfg)
	}

	addr := testserver.RunGitalyServer(tb, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterSearchServiceServer(srv, NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	})
	cfg.SocketPath = addr

	conn, err := client.New(ctx, addr)
	require.NoError(tb, err)
	tb.Cleanup(func() { conn.Close() })

	return cfg, gitalypb.NewSearchServiceClient(conn)
}

// searchResult collects everything a SearchCode stream returned.
type searchResult struct {
	matches     []*gitalypb.SearchCodeResponse_Match
	commitID    string
	truncated   bool
	messages    int
	commitIDSet []bool
	// sizes holds the encoded size of each message.
	sizes []int
}

func runSearch(tb testing.TB, ctx context.Context, client gitalypb.SearchServiceClient, req *gitalypb.SearchCodeRequest) (searchResult, error) {
	tb.Helper()

	stream, err := client.SearchCode(ctx, req)
	if err != nil {
		return searchResult{}, err
	}

	var result searchResult
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return result, err
		}

		result.messages++
		result.sizes = append(result.sizes, proto.Size(response))
		result.commitIDSet = append(result.commitIDSet, response.GetCommitId() != "")
		if response.GetCommitId() != "" {
			result.commitID = response.GetCommitId()
		}
		result.matches = append(result.matches, response.GetMatches()...)
		if response.GetTruncated() {
			result.truncated = true
		}
	}
}

// lineOf renders a match as "path:line:text" for compact comparisons.
func lineOf(m *gitalypb.SearchCodeResponse_Match) string {
	return string(m.GetPath()) + ":" + strconv.Itoa(int(m.GetLineNumber())) + ":" + string(m.GetLine())
}

// createSearchRepo creates a repository with the given files committed on branch main and
// returns the repository, its path and the commit ID.
func createSearchRepo(tb testing.TB, ctx context.Context, cfg config.Cfg, files map[string][]byte) (*gitalypb.Repository, string, string) {
	tb.Helper()

	repo, repoPath := gittest.CreateRepository(tb, ctx, cfg)

	root := writeNestedTree(tb, cfg, repoPath, files)
	commitID := gittest.WriteCommit(tb, cfg, repoPath,
		gittest.WithTree(root),
		gittest.WithBranch("main"),
	)

	return repo, repoPath, commitID.String()
}

// writeNestedTree writes a tree for files whose paths may contain slashes.
func writeNestedTree(tb testing.TB, cfg config.Cfg, repoPath string, files map[string][]byte) git.ObjectID {
	tb.Helper()

	var entries []gittest.TreeEntry
	subdirs := map[string]map[string][]byte{}

	for filePath, contents := range files {
		dir, rest, nested := strings.Cut(filePath, "/")
		if !nested {
			oid := gittest.WriteBlob(tb, cfg, repoPath, contents)
			entries = append(entries, gittest.TreeEntry{Path: filePath, Mode: "100644", OID: oid})
			continue
		}

		if subdirs[dir] == nil {
			subdirs[dir] = map[string][]byte{}
		}
		subdirs[dir][rest] = contents
	}

	for dir, subFiles := range subdirs {
		oid := writeNestedTree(tb, cfg, repoPath, subFiles)
		entries = append(entries, gittest.TreeEntry{Path: dir, Mode: "040000", OID: oid})
	}

	return gittest.WriteTree(tb, cfg, repoPath, entries)
}
