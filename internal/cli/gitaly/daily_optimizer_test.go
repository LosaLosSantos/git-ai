package gitaly

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestDailyOptimizer checks the job of the daily maintenance for one repository: the backstop of
// the review roots runs first, on the repository's local repo, and a failed backstop skips the
// housekeeping that could prune the commits it protects.
func TestDailyOptimizer(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"}
	local := &localrepo.Repo{}

	for _, tc := range []struct {
		desc          string
		backstopErr   error
		expectedCalls []string
		expectedErr   error
	}{
		{
			desc:          "the backstop runs before the housekeeping",
			expectedCalls: []string{"backstop", "optimize"},
		},
		{
			desc:          "a failed backstop skips the housekeeping",
			backstopErr:   errors.New("backfill failed"),
			expectedCalls: []string{"backstop"},
			expectedErr:   errors.New("backfill failed"),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			var calls []string
			optimizer := dailyOptimizer(
				func(_ log.Logger, r storage.Repository) *localrepo.Repo {
					require.Equal(t, repo, r)
					return local
				},
				func(_ context.Context, _ log.Logger, l *localrepo.Repo, r storage.Repository) error {
					require.Same(t, local, l)
					require.Equal(t, repo, r)
					calls = append(calls, "backstop")
					return tc.backstopErr
				},
				func(_ context.Context, l *localrepo.Repo) error {
					require.Same(t, local, l)
					calls = append(calls, "optimize")
					return nil
				},
			)

			err := optimizer.OptimizeRepository(ctx, testhelper.SharedLogger(t), repo)
			require.Equal(t, tc.expectedErr, err)
			require.Equal(t, tc.expectedCalls, calls)
		})
	}
}
