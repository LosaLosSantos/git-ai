package e2e

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/open_scion/sdk"
)

// The load scenario of phase 3 runs many agents against one branch at once.
// The helpers in this file are pure: they classify land outcomes and tally
// them, so the scenario's bookkeeping is tested with fakes and needs neither
// a server nor Git.

// loadTally counts the outcomes of concurrent land requests.
type loadTally struct {
	Landed    int
	Conflicts int
	Denied    int
	Failed    int
}

// record adds one land outcome to the tally. A nil error is a landed change,
// a merge conflict and a permission denial are counted separately, and any
// other error counts as a failure.
func (t *loadTally) record(err error) {
	switch {
	case err == nil:
		t.Landed++
	case sdk.IsMergeConflict(err):
		t.Conflicts++
	case sdk.Code(err) == codes.PermissionDenied:
		t.Denied++
	default:
		t.Failed++
	}
}

// total returns the number of recorded outcomes.
func (t loadTally) total() int {
	return t.Landed + t.Conflicts + t.Denied + t.Failed
}

// check returns an error unless every one of the total requests has exactly
// one recorded outcome.
func (t loadTally) check(total int) error {
	if got := t.total(); got != total {
		return fmt.Errorf("tally holds %d outcomes, want %d: %+v", got, total, t)
	}
	return nil
}

// runLoad calls land once per agent, all at the same time, and tallies the
// results. Each call receives its agent index, so callers can keep the
// per-agent responses in slices indexed by agent without locking.
func runLoad(agents int, land func(agent int) error) loadTally {
	var (
		mu    sync.Mutex
		tally loadTally
		wg    sync.WaitGroup
	)

	wg.Add(agents)
	for i := 0; i < agents; i++ {
		go func(agent int) {
			defer wg.Done()

			err := land(agent)

			mu.Lock()
			defer mu.Unlock()
			tally.record(err)
		}(i)
	}
	wg.Wait()

	return tally
}

// expectedMixedOutcome is the tally of the mixed load scenario: private
// changes never conflict and all land, and of the shared changes,
// made from one base and all touching the same file, only the first to land
// succeeds while the others fail with a merge conflict. The tally is the
// same whether or not the lands are batched, because each rejected request
// is judged against the running tip of its batch.
func expectedMixedOutcome(private, shared int) loadTally {
	if shared == 0 {
		return loadTally{Landed: private}
	}
	return loadTally{Landed: private + 1, Conflicts: shared - 1}
}

func TestRecordClassifiesLandOutcomes(t *testing.T) {
	t.Parallel()

	var tally loadTally
	tally.record(nil)
	tally.record(status.Error(codes.PermissionDenied, "denied"))
	tally.record(status.Error(codes.Internal, "boom"))

	require.Equal(t, loadTally{Landed: 1, Denied: 1, Failed: 1}, tally)
	require.NoError(t, tally.check(3))
	require.Error(t, tally.check(4))
}

func TestRunLoadTalliesEveryAgent(t *testing.T) {
	t.Parallel()

	const agents = 100
	tally := runLoad(agents, func(agent int) error {
		switch {
		case agent%10 == 0:
			return status.Error(codes.PermissionDenied, "denied")
		case agent == agents-1:
			return status.Error(codes.Internal, "boom")
		default:
			return nil
		}
	})

	// Agents 0, 10, ..., 90 are denied and agent 99 fails.
	require.Equal(t, loadTally{Landed: 89, Denied: 10, Failed: 1}, tally)
	require.NoError(t, tally.check(agents))
}

func TestRunLoadRunsAgentsConcurrently(t *testing.T) {
	t.Parallel()

	const agents = 100

	// Every agent signals its arrival and then waits for all the others. If
	// runLoad ran the agents one after another, the first agent would wait
	// until its timeout and fail.
	var arrived sync.WaitGroup
	arrived.Add(agents)
	release := make(chan struct{})
	go func() {
		arrived.Wait()
		close(release)
	}()

	tally := runLoad(agents, func(int) error {
		arrived.Done()
		select {
		case <-release:
			return nil
		case <-time.After(10 * time.Second):
			return status.Error(codes.DeadlineExceeded, "agents did not run concurrently")
		}
	})

	require.Equal(t, agents, tally.Landed, "tally: %+v", tally)
}

func TestExpectedMixedOutcome(t *testing.T) {
	t.Parallel()

	require.Equal(t, loadTally{Landed: 100}, expectedMixedOutcome(100, 0))
	require.Equal(t, loadTally{Landed: 91, Conflicts: 9}, expectedMixedOutcome(90, 10))
	require.Equal(t, loadTally{Landed: 1}, expectedMixedOutcome(0, 1))
	require.NoError(t, expectedMixedOutcome(90, 10).check(100))
}
