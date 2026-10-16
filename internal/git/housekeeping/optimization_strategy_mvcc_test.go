package housekeeping

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/housekeeping/config"
	"github.com/feder-cr/git-ai/internal/git/stats"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// mvccInfo returns a RepositoryInfo whose reference backend is MVCC.
func mvccInfo(info stats.RepositoryInfo) stats.RepositoryInfo {
	info.References.ReferenceBackendName = git.ReferenceBackendMVCC.Name
	return info
}

// TestEagerOptimizationStrategy_ShouldRepackObjects_mvcc: eager must request a plain geometric
// repack on MVCC repositories, object pools included. No bitmap, multi-pack-index or cruft expiry
// may leak into the config, as git-repack(1) cannot see its temporary pack through the manifest and
// would die with exit 128.
func TestEagerOptimizationStrategy_ShouldRepackObjects_mvcc(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	for _, tc := range []struct {
		desc string
		info stats.RepositoryInfo
	}{
		{
			desc: "no packs",
			info: mvccInfo(stats.RepositoryInfo{}),
		},
		{
			desc: "alternates",
			info: mvccInfo(stats.RepositoryInfo{
				Alternates: stats.AlternatesInfo{
					ObjectDirectories: []string{"path/to/alternate"},
				},
			}),
		},
		{
			desc: "object pool",
			info: mvccInfo(stats.RepositoryInfo{
				IsObjectPool: true,
			}),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			shouldRepack, cfg := EagerOptimizationStrategy{info: tc.info}.ShouldRepackObjects(ctx)
			require.True(t, shouldRepack)
			require.Equal(t, config.RepackObjectsConfig{
				Strategy: config.RepackObjectsStrategyGeometric,
			}, cfg)
		})
	}
}

// TestEagerOptimizationStrategy_ShouldPruneObjects_mvcc: eager must never prune on MVCC
// repositories. The MVCC backend keeps no loose objects and git-prune(1) runs read-pinned, so a
// prune would only log a deletion that never happened.
func TestEagerOptimizationStrategy_ShouldPruneObjects_mvcc(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	shouldPrune, _ := EagerOptimizationStrategy{info: mvccInfo(stats.RepositoryInfo{})}.ShouldPruneObjects(ctx)
	require.False(t, shouldPrune)
}
