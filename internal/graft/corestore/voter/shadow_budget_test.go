package voter_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
)

// TestShadowBudgetOverrunInPrepared checks the SHADOW BUDGET requirement of
// doc/graft/PLAN7.md for a Prepared phase that runs past the shadow budget
// (TestPreparingShadowTimeoutCoversBothPhases covers a Preparing phase that
// uses it up): the write is not failed, nothing is recorded, and the failure
// is counted as a shadow error.
func TestShadowBudgetOverrunInPrepared(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect})
		f.core.ShadowTimeout = 200 * time.Millisecond
		before := f.headSeq(t)

		// The work between the upload and the commit takes the whole budget.
		voter.FaultHook = func(point string) error {
			if point == "after_upload" {
				<-time.After(f.core.ShadowTimeout)
			}
			return nil
		}
		t.Cleanup(func() { voter.FaultHook = nil })

		require.NoError(t, f.prepared(line(f.zero, f.c.String(), ref)), "shadow mode never fails the write")
		require.Equal(t, before, f.headSeq(t), "nothing is recorded")
		require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
	})
}
