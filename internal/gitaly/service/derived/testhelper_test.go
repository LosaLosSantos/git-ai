package derived

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	commitservice "github.com/feder-cr/open_scion/internal/gitaly/service/commit"
	"github.com/feder-cr/open_scion/internal/gitaly/service/repository"
	"github.com/feder-cr/open_scion/internal/graft/derived"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

// services bundles the clients of a running test server.
type services struct {
	derived gitalypb.DerivedDataServiceClient
	commit  gitalypb.CommitServiceClient
}

// setupServices starts a Gitaly server with the DerivedDataService and the
// CommitService (for LastCommitForPath and RawBlame) and returns the config with
// the socket path set, which is needed by the gittest helpers, and the clients.
func setupServices(tb testing.TB, cfg config.Cfg) (config.Cfg, services) {
	tb.Helper()

	cfg.SocketPath = testserver.RunGitalyServer(tb, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterDerivedDataServiceServer(srv, NewServer(deps))
		gitalypb.RegisterCommitServiceServer(srv, commitservice.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	})

	conn, err := client.New(testhelper.Context(tb), cfg.SocketPath)
	require.NoError(tb, err)
	tb.Cleanup(func() { conn.Close() })

	return cfg, services{
		derived: gitalypb.NewDerivedDataServiceClient(conn),
		commit:  gitalypb.NewCommitServiceClient(conn),
	}
}

// fileChange is one change to a path in a commit created by writeHistory.
type fileChange struct {
	path    string
	mode    string
	content string
	delete  bool
}

// historyCommit describes one commit of a history created by writeHistory.
type historyCommit struct {
	name    string
	parents []string
	changes []fileChange
}

// writeHistory creates the commits with git fast-import and returns the object
// ID of every commit by name. Each commit is also reachable as refs/heads/<name>.
func writeHistory(tb testing.TB, cfg config.Cfg, repoPath string, commits []historyCommit) map[string]git.ObjectID {
	tb.Helper()

	marks := make(map[string]int, len(commits))
	var stream bytes.Buffer

	for i, commit := range commits {
		marks[commit.name] = i + 1

		fmt.Fprintf(&stream, "commit refs/heads/%s\n", commit.name)
		fmt.Fprintf(&stream, "mark :%d\n", i+1)
		fmt.Fprintf(&stream, "committer Tester <tester@example.com> %d +0000\n", 1600000000+i)
		fmt.Fprintf(&stream, "data %d\n%s\n", len(commit.name), commit.name)

		for j, parent := range commit.parents {
			mark, ok := marks[parent]
			require.True(tb, ok, "parent %q must be defined before %q", parent, commit.name)
			if j == 0 {
				fmt.Fprintf(&stream, "from :%d\n", mark)
			} else {
				fmt.Fprintf(&stream, "merge :%d\n", mark)
			}
		}

		for _, change := range commit.changes {
			if change.delete {
				fmt.Fprintf(&stream, "D %s\n", change.path)
				continue
			}

			mode := change.mode
			if mode == "" {
				mode = "100644"
			}
			fmt.Fprintf(&stream, "M %s inline %s\n", mode, change.path)
			fmt.Fprintf(&stream, "data %d\n%s\n", len(change.content), change.content)
		}

		stream.WriteString("\n")
	}

	gittest.ExecOpts(tb, cfg, gittest.ExecConfig{Stdin: &stream}, "-C", repoPath, "fast-import", "--quiet")

	ids := make(map[string]git.ObjectID, len(commits))
	for _, commit := range commits {
		ids[commit.name] = gittest.ResolveRevision(tb, cfg, repoPath, "refs/heads/"+commit.name)
	}

	return ids
}

// linearHistory returns n commits in a chain named c0..c(n-1). Commit i writes
// "content i" to pathOf(i) when that is not empty, and otherwise touches an
// unrelated file.
func linearHistory(n int, pathOf func(i int) string) []historyCommit {
	commits := make([]historyCommit, 0, n)
	for i := 0; i < n; i++ {
		commit := historyCommit{name: fmt.Sprintf("c%d", i)}
		if i > 0 {
			commit.parents = []string{fmt.Sprintf("c%d", i-1)}
		}

		path := pathOf(i)
		if path == "" {
			path = "unrelated"
		}
		commit.changes = []fileChange{{path: path, content: fmt.Sprintf("content %d\n", i)}}

		commits = append(commits, commit)
	}
	return commits
}

// mergeHistory has a merge whose path is TREESAME to the second parent, a path
// that is TREESAME to the first parent, a rename and a deletion.
func mergeHistory() []historyCommit {
	return []historyCommit{
		{name: "root", changes: []fileChange{
			{path: "a", content: "1"},
			{path: "b", content: "1"},
		}},
		{name: "side", parents: []string{"root"}, changes: []fileChange{{path: "a", content: "2"}}},
		{name: "main", parents: []string{"root"}, changes: []fileChange{{path: "b", content: "2"}}},
		{name: "merge", parents: []string{"main", "side"}, changes: []fileChange{{path: "a", content: "2"}}},
		{name: "rename", parents: []string{"merge"}, changes: []fileChange{
			{path: "a", delete: true},
			{path: "renamed", content: "2"},
		}},
		{name: "delete", parents: []string{"rename"}, changes: []fileChange{{path: "b", delete: true}}},
	}
}

// readBlame reads the whole Blame stream and returns the messages.
func readBlame(tb testing.TB, ctx context.Context, client gitalypb.DerivedDataServiceClient, request *gitalypb.BlameRequest) []*gitalypb.BlameResponse {
	tb.Helper()

	stream, err := client.Blame(ctx, request)
	require.NoError(tb, err)

	var responses []*gitalypb.BlameResponse
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			return responses
		}
		require.NoError(tb, err)
		responses = append(responses, response)
	}
}

// blameLines flattens the lines of blame responses.
func blameLines(responses []*gitalypb.BlameResponse) []derived.BlameLine {
	var lines []derived.BlameLine
	for _, response := range responses {
		for _, line := range response.GetLines() {
			lines = append(lines, derived.BlameLine{
				LineNumber:         line.GetLineNumber(),
				CommitID:           line.GetCommitId(),
				OriginalLineNumber: line.GetOriginalLineNumber(),
				Content:            line.GetContent(),
			})
		}
	}
	return lines
}

// readPathHistory reads the whole PathHistory stream and returns the commit IDs
// of each message.
func readPathHistory(tb testing.TB, ctx context.Context, client gitalypb.DerivedDataServiceClient, request *gitalypb.PathHistoryRequest) [][]string {
	tb.Helper()

	stream, err := client.PathHistory(ctx, request)
	require.NoError(tb, err)

	var batches [][]string
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			return batches
		}
		require.NoError(tb, err)

		var batch []string
		for _, commit := range response.GetCommits() {
			batch = append(batch, commit.GetId())
		}
		batches = append(batches, batch)
	}
}

func flatten(batches [][]string) []string {
	var ids []string
	for _, batch := range batches {
		ids = append(ids, batch...)
	}
	return ids
}

// plantBlame stores blame lines for the last-modified commit of path so that a
// later query must return them instead of running git blame.
func plantBlame(tb testing.TB, repoPath string, lastMod git.ObjectID, path string, lines []derived.BlameLine) {
	tb.Helper()

	encoded, err := json.Marshal(lines)
	require.NoError(tb, err)
	require.NoError(tb, derived.NewStore(repoPath).Put("blame", derived.Key("blame", lastMod, path), encoded))
}

// plantLastMod stores the last-modified commit of path as of commit.
func plantLastMod(tb testing.TB, repoPath string, commit git.ObjectID, path string, lastMod git.ObjectID) {
	tb.Helper()

	require.NoError(tb, derived.NewStore(repoPath).Put("lastmod", derived.Key("lastmod", commit, path), []byte(lastMod)))
}

// directoryHistory has directories and a file-to-directory relation that is
// missing from mergeHistory. "dir" only exists as a directory, and "a" is a file
// that paths such as "a/." and "a/x/.." name through Git's normalization.
func directoryHistory() []historyCommit {
	return []historyCommit{
		{name: "root", changes: []fileChange{
			{path: "a", content: "1"},
			{path: "dir/x", content: "1"},
		}},
		{name: "side", parents: []string{"root"}, changes: []fileChange{{path: "dir/y", content: "2"}}},
		{name: "main", parents: []string{"root"}, changes: []fileChange{{path: "dir/x", content: "2"}}},
		{name: "merge", parents: []string{"main", "side"}, changes: []fileChange{{path: "dir/y", content: "3"}}},
		{name: "remove", parents: []string{"merge"}, changes: []fileChange{{path: "dir/x", delete: true}}},
	}
}

// plantBlameOrigin stores the blame origin of path as of commit.
func plantBlameOrigin(tb testing.TB, repoPath string, commit git.ObjectID, path string, origin git.ObjectID) {
	tb.Helper()

	require.NoError(tb, derived.NewStore(repoPath).Put("blame-origin-typed", derived.Key("blame-origin-typed", commit, path), []byte(origin)))
}

// gitLastMod returns the commit `git log -1 -- path` prints as of commit.
func gitLastMod(tb testing.TB, cfg config.Cfg, repoPath string, commit git.ObjectID, path string) string {
	tb.Helper()

	out := gittest.Exec(tb, cfg, "-C", repoPath, "log", "-1", "--format=%H", commit.String(), "--", path)
	return string(bytes.TrimSpace(out))
}

// gitFirstParentLog returns what `git log --first-parent -- path` prints, newest first.
func gitFirstParentLog(tb testing.TB, cfg config.Cfg, repoPath string, commit git.ObjectID, path string) []string {
	tb.Helper()

	out := gittest.Exec(tb, cfg, "-C", repoPath, "log", "--first-parent", "--format=%H", commit.String(), "--", path)
	var ids []string
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) > 0 {
			ids = append(ids, string(line))
		}
	}
	return ids
}
