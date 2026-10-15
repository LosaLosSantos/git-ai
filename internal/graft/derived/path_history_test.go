package derived

import (
	"context"
	"fmt"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// collectPathHistory returns the IDs of the commits PathHistory reports.
func collectPathHistory(ctx context.Context, tb testing.TB, deriver *Deriver, start git.ObjectID, path string, limit int) []string {
	tb.Helper()

	var ids []string
	require.NoError(tb, deriver.PathHistory(ctx, start, path, limit, func(commit *catfile.Commit) error {
		ids = append(ids, commit.GetId())
		return nil
	}))
	return ids
}

func requireMatchesGitPathHistory(
	tb testing.TB,
	ctx context.Context,
	cfg config.Cfg,
	repoPath string,
	deriver *Deriver,
	ids map[string]git.ObjectID,
	names []string,
	paths []string,
) {
	tb.Helper()

	for _, name := range names {
		for _, path := range paths {
			want := gitFirstParentPathHistory(tb, cfg, repoPath, ids[name], path)
			got := collectPathHistory(ctx, tb, deriver, ids[name], path, 1000)
			require.Equal(tb, want, got, "path history of %q at %s", path, name)
		}
	}
}

func TestPathHistory_MatchesGitFirstParentLog(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	commits := mergeHistory()
	ids := writeHistory(t, cfg, repoPath, commits)

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	requireMatchesGitPathHistory(t, ctx, cfg, repoPath, deriver, ids, namesOf(commits), mergeHistoryPaths)

	// Histories are the same without the store.
	uncached := newTestDeriver(t, ctx, cfg, repo, false)
	requireMatchesGitPathHistory(t, ctx, cfg, repoPath, uncached, ids, namesOf(commits), mergeHistoryPaths)
}

func TestPathHistory_LimitAndMissingPath(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	commits := linearHistory(10, func(i int) string {
		if i%2 == 0 {
			return "a"
		}
		return ""
	})
	ids := writeHistory(t, cfg, repoPath, commits)
	deriver := newTestDeriver(t, ctx, cfg, repo, true)

	all := collectPathHistory(ctx, t, deriver, ids["c9"], "a", 100)
	require.Equal(t, []string{ids["c8"].String(), ids["c6"].String(), ids["c4"].String(), ids["c2"].String(), ids["c0"].String()}, all)

	require.Equal(t, all[:2], collectPathHistory(ctx, t, deriver, ids["c9"], "a", 2))
	require.Empty(t, collectPathHistory(ctx, t, deriver, ids["c9"], "missing", 100))
}

func TestPathHistory_LongLinearHistory(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	const length = 2000
	ids := writeHistory(t, cfg, repoPath, linearHistory(length, func(i int) string {
		if i%100 == 0 {
			return "a"
		}
		return ""
	}))

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	got := collectPathHistory(ctx, t, deriver, ids[fmt.Sprintf("c%d", length-1)], "a", 1000)
	require.Len(t, got, 20)
	require.Equal(t, ids["c1900"].String(), got[0])
	require.Equal(t, ids["c0"].String(), got[19])
	require.Equal(t, gitFirstParentPathHistory(t, cfg, repoPath, ids[fmt.Sprintf("c%d", length-1)], "a"), got)
}
