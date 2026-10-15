package derived

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/graft/derived"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// blameHistory creates a file that is changed twice, a directory and an empty
// file, and returns the commits by name.
func blameHistory(tb testing.TB, cfg config.Cfg, repoPath string) map[string]git.ObjectID {
	tb.Helper()

	return writeHistory(tb, cfg, repoPath, []historyCommit{
		{name: "c0", changes: []fileChange{
			{path: "f", content: "a\nb\nc\n"},
			{path: "dir/g", content: "only\n"},
		}},
		{name: "c1", parents: []string{"c0"}, changes: []fileChange{{path: "f", content: "a\nB\nc\nd\n"}}},
		{name: "c2", parents: []string{"c1"}, changes: []fileChange{{path: "f", content: "a\nB\nc2\nd\n"}}},
		{name: "empty", parents: []string{"c2"}, changes: []fileChange{{path: "e", content: ""}}},
	})
}

// drainBlame reads the Blame stream and returns the messages, or the error.
func drainBlame(ctx context.Context, client gitalypb.DerivedDataServiceClient, request *gitalypb.BlameRequest) ([]*gitalypb.BlameResponse, error) {
	stream, err := client.Blame(ctx, request)
	if err != nil {
		return nil, err
	}

	var responses []*gitalypb.BlameResponse
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return responses, nil
		}
		if err != nil {
			return responses, err
		}
		responses = append(responses, response)
	}
}

func TestBlame_MatchesRawBlame(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := blameHistory(t, cfg, repoPath)

	for _, revision := range []string{"c2", "c1", "c0"} {
		t.Run(revision, func(t *testing.T) {
			raw, err := svc.commit.RawBlame(ctx, &gitalypb.RawBlameRequest{
				Repository: repoProto,
				Revision:   []byte(revision),
				Path:       []byte("f"),
			})
			require.NoError(t, err)

			var porcelain bytes.Buffer
			for {
				response, err := raw.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				porcelain.Write(response.GetData())
			}

			want, err := derived.ParsePorcelain(porcelain.Bytes())
			require.NoError(t, err)

			responses := readBlame(t, ctx, svc.derived, &gitalypb.BlameRequest{
				Repository: repoProto,
				Revision:   []byte(revision),
				Path:       []byte("f"),
			})
			require.Equal(t, ids[revision].String(), responses[0].GetCommitId())
			require.Equal(t, want, blameLines(responses))
		})
	}
}

func TestBlame_ResultIsSharedByRevisionsWithSameFile(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := blameHistory(t, cfg, repoPath)

	// The file did not change in "empty", so its blame is the one of c2 and the
	// revision is reported as the resolved commit.
	responses := readBlame(t, ctx, svc.derived, &gitalypb.BlameRequest{
		Repository: repoProto,
		Revision:   []byte("empty"),
		Path:       []byte("f"),
	})
	require.Equal(t, ids["empty"].String(), responses[0].GetCommitId())
	require.Len(t, blameLines(responses), 4)
}

func TestBlame_UsesStoredBlame(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := blameHistory(t, cfg, repoPath)

	planted := []derived.BlameLine{
		{LineNumber: 1, CommitID: "planted", OriginalLineNumber: 7, Content: []byte("from the store")},
	}
	lastMod := git.ObjectID(gitLastMod(t, cfg, repoPath, ids["c2"], "f"))
	require.Equal(t, ids["c2"], lastMod)
	plantBlame(t, repoPath, lastMod, "f", planted)

	responses := readBlame(t, ctx, svc.derived, &gitalypb.BlameRequest{
		Repository: repoProto,
		Revision:   []byte("c2"),
		Path:       []byte("f"),
	})
	require.Equal(t, planted, blameLines(responses), "a stored blame must be returned without running git blame")
}

func TestBlame_OriginIsReadFromStore(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := blameHistory(t, cfg, repoPath)

	// Pretend that the blame of f at c2 originates in c0. The blame must then be looked up at c0.
	plantBlameOrigin(t, repoPath, ids["c2"], "f", ids["c0"])
	planted := []derived.BlameLine{{LineNumber: 1, CommitID: "from-c0", Content: []byte("x")}}
	plantBlame(t, repoPath, ids["c0"], "f", planted)

	responses := readBlame(t, ctx, svc.derived, &gitalypb.BlameRequest{
		Repository: repoProto,
		Revision:   []byte("c2"),
		Path:       []byte("f"),
	})
	require.Equal(t, planted, blameLines(responses))
}

func TestBlame_Validation(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	blameHistory(t, cfg, repoPath)

	for _, tc := range []struct {
		desc     string
		request  *gitalypb.BlameRequest
		expected codes.Code
	}{
		{
			desc:     "invalid storage",
			request:  &gitalypb.BlameRequest{Repository: &gitalypb.Repository{StorageName: "fake", RelativePath: "x"}, Revision: []byte("c2"), Path: []byte("f")},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "unset repository",
			request:  &gitalypb.BlameRequest{Revision: []byte("c2"), Path: []byte("f")},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "empty revision",
			request:  &gitalypb.BlameRequest{Repository: repoProto, Path: []byte("f")},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "revision that looks like an option",
			request:  &gitalypb.BlameRequest{Repository: repoProto, Revision: []byte("--output=/tmp/x"), Path: []byte("f")},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "empty path",
			request:  &gitalypb.BlameRequest{Repository: repoProto, Revision: []byte("c2")},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "absolute path",
			request:  &gitalypb.BlameRequest{Repository: repoProto, Revision: []byte("c2"), Path: []byte("/f")},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "path escapes repository",
			request:  &gitalypb.BlameRequest{Repository: repoProto, Revision: []byte("c2"), Path: []byte("../f")},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "unknown revision",
			request:  &gitalypb.BlameRequest{Repository: repoProto, Revision: []byte("does-not-exist"), Path: []byte("f")},
			expected: codes.NotFound,
		},
		{
			desc:     "path missing at revision",
			request:  &gitalypb.BlameRequest{Repository: repoProto, Revision: []byte("c2"), Path: []byte("missing")},
			expected: codes.NotFound,
		},
		{
			desc:     "directory is not a file",
			request:  &gitalypb.BlameRequest{Repository: repoProto, Revision: []byte("c2"), Path: []byte("dir")},
			expected: codes.InvalidArgument,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := drainBlame(ctx, svc.derived, tc.request)
			testhelper.RequireGrpcCode(t, err, tc.expected)
		})
	}
}

func TestBlame_EmptyFileSendsOneMessage(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := blameHistory(t, cfg, repoPath)

	responses := readBlame(t, ctx, svc.derived, &gitalypb.BlameRequest{
		Repository: repoProto,
		Revision:   []byte("empty"),
		Path:       []byte("e"),
	})
	require.Len(t, responses, 1)
	require.Equal(t, ids["empty"].String(), responses[0].GetCommitId())
	require.Empty(t, responses[0].GetLines())
}

func TestBlame_BatchesLines(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	const lineCount = 2500
	var content strings.Builder
	for i := 1; i <= lineCount; i++ {
		fmt.Fprintf(&content, "line %d\n", i)
	}
	ids := writeHistory(t, cfg, repoPath, []historyCommit{
		{name: "big", changes: []fileChange{{path: "big", content: content.String()}}},
	})

	responses := readBlame(t, ctx, svc.derived, &gitalypb.BlameRequest{
		Repository: repoProto,
		Revision:   []byte("big"),
		Path:       []byte("big"),
	})

	require.Len(t, responses, 3)
	require.Equal(t, ids["big"].String(), responses[0].GetCommitId())
	require.Empty(t, responses[1].GetCommitId(), "the commit ID is only set on the first message")
	require.Empty(t, responses[2].GetCommitId())
	require.Len(t, responses[0].GetLines(), 1000)
	require.Len(t, responses[1].GetLines(), 1000)
	require.Len(t, responses[2].GetLines(), 500)

	lines := blameLines(responses)
	require.Len(t, lines, lineCount)
	for i, line := range lines {
		require.Equal(t, int32(i+1), line.LineNumber)
		require.Equal(t, ids["big"].String(), line.CommitID)
		require.Equal(t, fmt.Sprintf("line %d", i+1), string(line.Content))
	}
}

func TestBlame_DisabledCacheWritesNothing(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Derived.Disabled = true
	cfg, svc := setupServices(t, cfg)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := blameHistory(t, cfg, repoPath)

	responses := readBlame(t, ctx, svc.derived, &gitalypb.BlameRequest{
		Repository: repoProto,
		Revision:   []byte("c2"),
		Path:       []byte("f"),
	})
	require.Equal(t, ids["c2"].String(), responses[0].GetCommitId())
	require.Len(t, blameLines(responses), 4)

	require.NoDirExists(t, filepath.Join(repoPath, "graft"))
}

func TestBlame_ConcurrentRequests(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	blameHistory(t, cfg, repoPath)

	request := &gitalypb.BlameRequest{Repository: repoProto, Revision: []byte("c2"), Path: []byte("f")}
	want := blameLines(readBlame(t, ctx, svc.derived, request))

	const workers = 10
	var wg sync.WaitGroup
	results := make([][]derived.BlameLine, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			responses, err := drainBlame(ctx, svc.derived, request)
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = blameLines(responses)
		}(i)
	}
	wg.Wait()

	for i := 0; i < workers; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, want, results[i])
	}
}

// rawBlameLines returns the parsed RawBlame of path at revision.
func rawBlameLines(tb testing.TB, ctx context.Context, client gitalypb.CommitServiceClient, repo *gitalypb.Repository, revision, path string) []derived.BlameLine {
	tb.Helper()

	raw, err := client.RawBlame(ctx, &gitalypb.RawBlameRequest{
		Repository: repo,
		Revision:   []byte(revision),
		Path:       []byte(path),
	})
	require.NoError(tb, err)

	var porcelain bytes.Buffer
	for {
		response, err := raw.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(tb, err)
		porcelain.Write(response.GetData())
	}

	lines, err := derived.ParsePorcelain(porcelain.Bytes())
	require.NoError(tb, err)
	return lines
}

func TestBlame_MatchesRawBlameAcrossTypeChanges(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	// git blame passes the whole blame to a parent with the same blob only when the
	// file type is the same too: a symlink that becomes a regular file (or the reverse)
	// starts a new blame, while a permission change does not.
	ids := writeHistory(t, cfg, repoPath, []historyCommit{
		{name: "c0", changes: []fileChange{
			{path: "link", mode: "120000", content: "target"},
			{path: "exe", content: "x\n"},
		}},
		{name: "c1", parents: []string{"c0"}, changes: []fileChange{
			{path: "link", content: "target"},
			{path: "exe", mode: "100755", content: "x\n"},
		}},
		{name: "c2", parents: []string{"c1"}, changes: []fileChange{{path: "link", mode: "120000", content: "target"}}},
		{name: "base", changes: []fileChange{{path: "other", content: "o\n"}}},
		{name: "reg", parents: []string{"base"}, changes: []fileChange{{path: "g", content: "v1\n"}}},
		{name: "sym", parents: []string{"base"}, changes: []fileChange{{path: "g", mode: "120000", content: "v1\n"}}},
		{name: "merge", parents: []string{"sym", "reg"}, changes: []fileChange{{path: "g", content: "v1\n"}}},
	})

	for _, tc := range []struct {
		revision, path, attributed string
	}{
		{revision: "c1", path: "link", attributed: "c1"},
		{revision: "c2", path: "link", attributed: "c2"},
		{revision: "c0", path: "link", attributed: "c0"},
		{revision: "c2", path: "exe", attributed: "c0"},
		{revision: "merge", path: "g", attributed: "reg"},
		{revision: "sym", path: "g", attributed: "sym"},
	} {
		t.Run(tc.revision+"/"+tc.path, func(t *testing.T) {
			want := rawBlameLines(t, ctx, svc.commit, repoProto, tc.revision, tc.path)
			require.Len(t, want, 1)
			require.Equal(t, ids[tc.attributed].String(), want[0].CommitID)

			// The second request is answered from the store and must not differ.
			for i := 0; i < 2; i++ {
				responses := readBlame(t, ctx, svc.derived, &gitalypb.BlameRequest{
					Repository: repoProto,
					Revision:   []byte(tc.revision),
					Path:       []byte(tc.path),
				})
				require.Equal(t, ids[tc.revision].String(), responses[0].GetCommitId())
				require.Equal(t, want, blameLines(responses))
			}
		})
	}
}
