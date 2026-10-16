package derived

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// drainPathHistory reads the PathHistory stream and returns the commit IDs, or
// the error.
func drainPathHistory(ctx context.Context, client gitalypb.DerivedDataServiceClient, request *gitalypb.PathHistoryRequest) ([]string, error) {
	stream, err := client.PathHistory(ctx, request)
	if err != nil {
		return nil, err
	}

	var ids []string
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return ids, nil
		}
		if err != nil {
			return ids, err
		}
		for _, commit := range response.GetCommits() {
			ids = append(ids, commit.GetId())
		}
	}
}

func TestPathHistory_MatchesGitFirstParentLog(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commits := mergeHistory()
	ids := writeHistory(t, cfg, repoPath, commits)

	for _, commit := range commits {
		for _, path := range []string{"a", "b", "renamed", "."} {
			want := gitFirstParentLog(t, cfg, repoPath, ids[commit.name], path)

			got := flatten(readPathHistory(t, ctx, svc.derived, &gitalypb.PathHistoryRequest{
				Repository: repoProto,
				Revision:   []byte(commit.name),
				Path:       []byte(path),
				Limit:      1000,
			}))
			require.Equal(t, want, got, "history of %q at %s", path, commit.name)
		}
	}
}

func TestPathHistory_BatchesAndLimit(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := writeHistory(t, cfg, repoPath, linearHistory(250, func(int) string { return "a" }))

	batches := readPathHistory(t, ctx, svc.derived, &gitalypb.PathHistoryRequest{
		Repository: repoProto,
		Revision:   []byte("c249"),
		Path:       []byte("a"),
		Limit:      250,
	})
	require.Len(t, batches, 3)
	require.Len(t, batches[0], 100)
	require.Len(t, batches[1], 100)
	require.Len(t, batches[2], 50)

	all := flatten(batches)
	require.Equal(t, ids["c249"].String(), all[0])
	require.Equal(t, ids["c0"].String(), all[249])
	require.Equal(t, gitFirstParentLog(t, cfg, repoPath, ids["c249"], "a"), all)

	// A zero limit means 100 commits.
	defaulted := flatten(readPathHistory(t, ctx, svc.derived, &gitalypb.PathHistoryRequest{
		Repository: repoProto,
		Revision:   []byte("c249"),
		Path:       []byte("a"),
	}))
	require.Equal(t, all[:100], defaulted)

	limited := flatten(readPathHistory(t, ctx, svc.derived, &gitalypb.PathHistoryRequest{
		Repository: repoProto,
		Revision:   []byte("c249"),
		Path:       []byte("a"),
		Limit:      7,
	}))
	require.Equal(t, all[:7], limited)
}

func TestPathHistory_RootSpellings(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := writeHistory(t, cfg, repoPath, mergeHistory())

	want := gitFirstParentLog(t, cfg, repoPath, ids["delete"], ".")
	require.NotEmpty(t, want)

	for _, path := range []string{".", "", "/"} {
		got := flatten(readPathHistory(t, ctx, svc.derived, &gitalypb.PathHistoryRequest{
			Repository: repoProto,
			Revision:   []byte("delete"),
			Path:       []byte(path),
			Limit:      1000,
		}))
		require.Equal(t, want, got, "path %q names the repository root", path)
	}
}

func TestPathHistory_MissingPathIsEmpty(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	writeHistory(t, cfg, repoPath, mergeHistory())

	batches := readPathHistory(t, ctx, svc.derived, &gitalypb.PathHistoryRequest{
		Repository: repoProto,
		Revision:   []byte("delete"),
		Path:       []byte("never-existed"),
	})
	require.Empty(t, batches)
}

func TestPathHistory_Validation(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	writeHistory(t, cfg, repoPath, mergeHistory())

	for _, tc := range []struct {
		desc     string
		request  *gitalypb.PathHistoryRequest
		expected codes.Code
	}{
		{
			desc: "invalid storage",
			request: &gitalypb.PathHistoryRequest{
				Repository: &gitalypb.Repository{StorageName: "fake", RelativePath: "x"},
				Revision:   []byte("delete"),
				Path:       []byte("a"),
			},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "unset repository",
			request:  &gitalypb.PathHistoryRequest{Revision: []byte("delete"), Path: []byte("a")},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "empty revision",
			request:  &gitalypb.PathHistoryRequest{Repository: repoProto, Path: []byte("a")},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "revision that looks like an option",
			request:  &gitalypb.PathHistoryRequest{Repository: repoProto, Revision: []byte("-n1"), Path: []byte("a")},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "absolute path",
			request:  &gitalypb.PathHistoryRequest{Repository: repoProto, Revision: []byte("delete"), Path: []byte("/abs/a")},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "path escapes repository",
			request:  &gitalypb.PathHistoryRequest{Repository: repoProto, Revision: []byte("delete"), Path: []byte("../a")},
			expected: codes.InvalidArgument,
		},
		{
			desc:     "unknown revision",
			request:  &gitalypb.PathHistoryRequest{Repository: repoProto, Revision: []byte("does-not-exist"), Path: []byte("a")},
			expected: codes.NotFound,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := drainPathHistory(ctx, svc.derived, tc.request)
			testhelper.RequireGrpcCode(t, err, tc.expected)
		})
	}
}

func TestPathHistory_LongLinearHistory(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	const length = 2000
	ids := writeHistory(t, cfg, repoPath, linearHistory(length, func(int) string { return "a" }))

	got := flatten(readPathHistory(t, ctx, svc.derived, &gitalypb.PathHistoryRequest{
		Repository: repoProto,
		Revision:   []byte(fmt.Sprintf("c%d", length-1)),
		Path:       []byte("a"),
		Limit:      length,
	}))
	require.Len(t, got, length)
	require.Equal(t, ids[fmt.Sprintf("c%d", length-1)].String(), got[0])
	require.Equal(t, ids["c0"].String(), got[length-1])
}

// TestPathHistory_DoesNotComputeChangedPaths requires the RPC to answer with the
// Deriver's walk, reading changed-paths summaries only as a pre-filter: a read never
// computes or stores a summary, which would diff the whole tree of every commit.
func TestPathHistory_DoesNotComputeChangedPaths(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	require.False(t, cfg.Graft.Derived.Disabled)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := writeHistory(t, cfg, repoPath, linearHistory(50, func(int) string { return "a" }))

	request := &gitalypb.PathHistoryRequest{Repository: repoProto, Revision: []byte("c49"), Path: []byte("a"), Limit: 1000}
	want := gitFirstParentLog(t, cfg, repoPath, ids["c49"], "a")
	require.Equal(t, want, flatten(readPathHistory(t, ctx, svc.derived, request)))
	// The repeated query is answered by the stored chain values.
	require.Equal(t, want, flatten(readPathHistory(t, ctx, svc.derived, request)))

	require.DirExists(t, filepath.Join(repoPath, "graft", "derived", "lastmod-firstparent"))
	require.NoDirExists(t, filepath.Join(repoPath, "graft", "derived", "changed-paths"))
}

func TestPathHistory_ConcurrentRequests(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	writeHistory(t, cfg, repoPath, mergeHistory())

	request := &gitalypb.PathHistoryRequest{Repository: repoProto, Revision: []byte("delete"), Path: []byte("a")}
	want := flatten(readPathHistory(t, ctx, svc.derived, request))
	require.NotEmpty(t, want)

	const workers = 10
	var wg sync.WaitGroup
	results := make([][]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = drainPathHistory(ctx, svc.derived, request)
		}(i)
	}
	wg.Wait()

	for i := 0; i < workers; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, want, results[i])
	}
}
