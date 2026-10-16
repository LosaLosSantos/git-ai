package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

// recordingBisector records the failures it is given and tests the good
// commit with the Tester it receives, which exercises materializing an older
// commit.
type recordingBisector struct {
	mu       sync.Mutex
	failures []Failure
	goodPass bool
	err      error
}

func (b *recordingBisector) Bisect(ctx context.Context, f Failure) error {
	b.mu.Lock()
	b.failures = append(b.failures, f)
	b.mu.Unlock()

	if f.Good != "" {
		res, err := f.Tester.Test(ctx, f.Good)
		if err != nil {
			return err
		}
		b.mu.Lock()
		b.goodPass = res.Passed
		b.mu.Unlock()
	}
	return b.err
}

func (b *recordingBisector) recorded() []Failure {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Failure(nil), b.failures...)
}

func TestRunOncePassingTipMovesGreen(t *testing.T) {
	fx := newFixture(t, nil)
	c1 := fx.commit("a.txt", "one")

	v := fx.verifier()
	out, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, c1, out.Tip)
	require.True(t, out.Passed)
	require.True(t, out.GreenMoved)
	require.Equal(t, c1, out.Green)
	require.Equal(t, uint64(1), out.Cursor)

	require.Equal(t, c1, fx.green())
	require.Equal(t, []string{c1}, fx.runsLog())
	require.Equal(t, uint64(1), v.Cursor())

	st, err := loadState(fx.cfg.StateFile)
	require.NoError(t, err)
	require.Equal(t, uint64(1), st.Cursor)
}

func TestRunOnceWithoutBranchChangeTestsNothing(t *testing.T) {
	fx := newFixture(t, nil)

	v := fx.verifier()
	out, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.False(t, ran)
	require.Equal(t, Outcome{}, out)
	require.Empty(t, fx.runsLog())
	require.Empty(t, fx.green())
	require.Equal(t, uint64(0), v.Cursor())
}

func TestRunOnceFailingTipKeepsGreen(t *testing.T) {
	fx := newFixture(t, nil)
	c1 := fx.commit("a.txt", "one")
	v := fx.verifier()
	_, _, err := v.RunOnce(context.Background())
	require.NoError(t, err)

	c2 := fx.commit("broken", "x")
	out, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, c2, out.Tip)
	require.False(t, out.Passed)
	require.False(t, out.GreenMoved)
	require.Equal(t, c1, out.Green)
	require.Contains(t, out.Output, "broken file present")
	require.Equal(t, uint64(2), out.Cursor)

	require.Equal(t, c1, fx.green(), "a failing tip must not move the green ref")
	require.Equal(t, uint64(2), v.Cursor())
}

func TestRunOnceTimedOutTipIsFailure(t *testing.T) {
	fx := newFixture(t, func(c *Config) {
		c.Test.Command = []string{"sh", "-c", "exec sleep 30"}
		c.Test.Timeout = Duration(100 * time.Millisecond)
	})
	fx.commit("a.txt", "one")

	out, ran, err := fx.verifier().RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.False(t, out.Passed)
	require.True(t, out.TimedOut)
	require.Empty(t, fx.green())
}

func TestRunOnceBisectorGetsFailureAndCanTestGoodCommit(t *testing.T) {
	fx := newFixture(t, nil)
	c1 := fx.commit("a.txt", "one")
	v := fx.verifier()
	_, _, err := v.RunOnce(context.Background())
	require.NoError(t, err)

	c2 := fx.commit("broken", "x")
	bisector := &recordingBisector{}
	v.SetBisector(bisector)

	out, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.False(t, out.Passed)
	require.Empty(t, out.BisectError)

	failures := bisector.recorded()
	require.Len(t, failures, 1)
	require.Equal(t, "main", failures[0].Branch)
	require.Equal(t, c1, failures[0].Good)
	require.Equal(t, c2, failures[0].Bad)
	require.Contains(t, failures[0].Output, "broken file present")
	require.NotNil(t, failures[0].Tester)
	require.NotNil(t, failures[0].Backend)

	// The bisector tested the good commit, which is materialized even though
	// the tip is ahead of it.
	require.True(t, bisector.goodPass)
	require.Equal(t, []string{c1, c2, c1}, fx.runsLog())
	require.Equal(t, c1, fx.green())
}

func TestRunOnceBisectorErrorIsReportedAndCursorAdvances(t *testing.T) {
	fx := newFixture(t, nil)
	fx.commit("broken", "x")

	v := fx.verifier()
	v.SetBisector(&recordingBisector{err: errors.New("no culprit found")})

	out, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, "no culprit found", out.BisectError)
	require.Equal(t, uint64(1), v.Cursor())
	require.Empty(t, fx.green())
}

func TestRestartResumesFromPersistedCursor(t *testing.T) {
	fx := newFixture(t, nil)
	c1 := fx.commit("a.txt", "one")
	_, _, err := fx.verifier().RunOnce(context.Background())
	require.NoError(t, err)

	restarted := fx.verifier()
	require.Equal(t, uint64(1), restarted.Cursor())
	require.Equal(t, c1, restarted.Green())

	_, ran, err := restarted.RunOnce(context.Background())
	require.NoError(t, err)
	require.False(t, ran, "nothing changed since the cursor")
	require.Equal(t, []string{c1}, fx.runsLog())

	c2 := fx.commit("b.txt", "two")
	out, ran, err := restarted.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, c2, out.Tip)
	require.Equal(t, c2, fx.green())
}

func TestRunOnceInfrastructureErrorKeepsCursor(t *testing.T) {
	fx := newFixture(t, func(c *Config) {
		c.Test.Command = []string{filepath.Join(c.WorkDir, "no-such-test-command")}
	})
	fx.commit("a.txt", "one")

	v := fx.verifier()
	_, ran, err := v.RunOnce(context.Background())
	require.Error(t, err)
	require.True(t, ran)
	require.Equal(t, uint64(0), v.Cursor())
	require.Empty(t, fx.green())

	st, err := loadState(fx.cfg.StateFile)
	require.NoError(t, err)
	require.Equal(t, uint64(0), st.Cursor)
}

func TestRunOnceGreenMoveFailureKeepsCursor(t *testing.T) {
	fx := newFixture(t, nil)
	c1 := fx.commit("a.txt", "one")

	v := fx.verifier()
	fx.greens.failMove = errors.New("green ref is locked")
	_, _, err := v.RunOnce(context.Background())
	require.ErrorContains(t, err, "green ref is locked")
	require.Equal(t, uint64(0), v.Cursor(), "a passed tip that did not become green is verified again")

	fx.greens.failMove = nil
	out, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.True(t, out.GreenMoved)
	require.Equal(t, c1, fx.green())
	require.Equal(t, uint64(1), v.Cursor())
}

func TestRunOnceSkipsDeletedBranch(t *testing.T) {
	fx := newFixture(t, nil)
	fx.commit("a.txt", "one")
	require.NoError(t, fx.backend.DeleteBranch(context.Background(), "main"))

	out, ran, err := fx.verifier().RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, "branch does not exist", out.Skipped)
	require.Empty(t, fx.runsLog())
}

func TestRunOnceReclonesDamagedWorkTree(t *testing.T) {
	fx := newFixture(t, nil)
	fx.commit("app.txt", "v1")
	v := fx.verifier()
	_, _, err := v.RunOnce(context.Background())
	require.NoError(t, err)

	// A local edit to a file that the next commit writes makes the update
	// refuse, so the tree is cloned again.
	require.NoError(t, os.WriteFile(fx.treeFile("app.txt"), []byte("local edit"), 0o600))
	c2 := fx.commit("app.txt", "v2")

	out, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.True(t, out.Passed)
	require.Equal(t, c2, out.Tip)
	require.Equal(t, c2, fx.green())

	content, err := os.ReadFile(fx.treeFile("app.txt"))
	require.NoError(t, err)
	require.Equal(t, "v2", string(content))
}

func TestWorkTreeIsSparse(t *testing.T) {
	fx := newFixture(t, func(c *Config) {
		c.Sparse = []string{"cmd"}
	})
	fx.commit("cmd/main.txt", "main")
	fx.commit("docs/readme.txt", "docs")

	_, ran, err := fx.verifier().RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)

	_, err = os.Stat(fx.treeFile("cmd/main.txt"))
	require.NoError(t, err)
	_, err = os.Stat(fx.treeFile("docs/readme.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestNewVerifierRefusesForeignWorkDir(t *testing.T) {
	fx := newFixture(t, nil)
	require.NoError(t, os.MkdirAll(fx.cfg.WorkDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(fx.cfg.WorkDir, "precious.txt"), []byte("keep"), 0o600))

	_, err := newVerifier(context.Background(), fx.cfg, fx.backend, fx.greens, discardLogger())
	require.ErrorContains(t, err, "not empty and not a graft-verify work directory")

	content, err := os.ReadFile(filepath.Join(fx.cfg.WorkDir, "precious.txt"))
	require.NoError(t, err)
	require.Equal(t, "keep", string(content))
}

func TestNewVerifierAcceptsMarkedWorkDir(t *testing.T) {
	fx := newFixture(t, nil)
	fx.commit("a.txt", "one")
	_, _, err := fx.verifier().RunOnce(context.Background())
	require.NoError(t, err)

	// The work directory is now marked, so it is accepted again.
	_, err = newVerifier(context.Background(), fx.cfg, fx.backend, fx.greens, discardLogger())
	require.NoError(t, err)
}

func TestNewVerifierRejectsInvalidConfig(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := fx.cfg
	cfg.GreenRef = "refs/heads/main"

	_, err := newVerifier(context.Background(), cfg, fx.backend, fx.greens, discardLogger())
	require.ErrorContains(t, err, "green_ref")
}

func TestNewVerifierRejectsCorruptState(t *testing.T) {
	fx := newFixture(t, nil)
	require.NoError(t, os.WriteFile(fx.cfg.StateFile, []byte("garbage"), 0o600))

	_, err := newVerifier(context.Background(), fx.cfg, fx.backend, fx.greens, discardLogger())
	require.ErrorContains(t, err, "parse state file")
}

func TestRunBatchesLandsInsideWindow(t *testing.T) {
	fx := newFixture(t, func(c *Config) {
		c.BatchWindow = Duration(800 * time.Millisecond)
	})

	v := fx.verifier()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- v.Run(ctx) }()

	var tip string
	for i := 0; i < 5; i++ {
		tip = fx.commit("file.txt", string(rune('a'+i)))
	}

	require.Eventually(t, func() bool { return fx.green() == tip }, 10*time.Second, 5*time.Millisecond)
	require.Equal(t, []string{tip}, fx.runsLog(), "a burst of lands is tested once")

	cancel()
	require.NoError(t, <-done)
}

func TestRunTestsTipAgainWhenLandArrivesDuringTest(t *testing.T) {
	fx := newFixture(t, nil)

	// Close the gate so that the first test blocks until it is opened.
	require.NoError(t, os.Remove(fx.gate))

	v := fx.verifier()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- v.Run(ctx) }()

	c1 := fx.commit("a.txt", "one")
	require.Eventually(t, func() bool { return len(fx.runsLog()) == 1 }, 10*time.Second, 5*time.Millisecond)

	c2 := fx.commit("b.txt", "two")
	require.NoError(t, os.WriteFile(fx.gate, nil, 0o600))

	require.Eventually(t, func() bool { return fx.green() == c2 }, 10*time.Second, 5*time.Millisecond)
	require.Equal(t, []string{c1, c2}, fx.runsLog())

	cancel()
	require.NoError(t, <-done)
}

func TestRunStopsWhenCanceled(t *testing.T) {
	fx := newFixture(t, nil)
	v := fx.verifier()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- v.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunPersistsCursorAfterCycle(t *testing.T) {
	fx := newFixture(t, nil)
	v := fx.verifier()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- v.Run(ctx) }()

	c1 := fx.commit("a.txt", "one")
	require.Eventually(t, func() bool { return fx.green() == c1 }, 10*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool { return stateCursor(t, fx) == 1 }, 10*time.Second, 5*time.Millisecond)

	cancel()
	require.NoError(t, <-done)
	require.Equal(t, uint64(1), stateCursor(t, fx))
}

// stateCursor reads the cursor from the state file of the fixture. Tests use
// it instead of Verifier.Cursor while Run is active, which would race.
func stateCursor(t *testing.T, fx *fixture) uint64 {
	t.Helper()
	st, err := loadState(fx.cfg.StateFile)
	require.NoError(t, err)
	return st.Cursor
}

func TestRunRetriesFailedCycleAndRecovers(t *testing.T) {
	fx := newFixture(t, nil)
	fx.greens.failMove = errors.New("green ref is locked")
	fx.commit("a.txt", "one")

	v := fx.verifier()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- v.Run(ctx) }()

	// The failed cycle does not move the cursor; once the ref can be moved,
	// the retry succeeds.
	require.Eventually(t, func() bool { return len(fx.runsLog()) >= 1 }, 10*time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, uint64(0), stateCursor(t, fx))

	fx.greens.mu.Lock()
	fx.greens.failMove = nil
	fx.greens.mu.Unlock()

	require.Eventually(t, func() bool { return stateCursor(t, fx) == 1 }, 10*time.Second, 5*time.Millisecond)
	require.NotEmpty(t, fx.green())

	cancel()
	require.NoError(t, <-done)
}

func TestRunFailsWhenWatchEnds(t *testing.T) {
	fx := newFixture(t, nil)
	v, err := newVerifier(context.Background(), fx.cfg, &endedWatch{fx.backend}, fx.greens, discardLogger())
	require.NoError(t, err)

	err = v.Run(context.Background())
	require.ErrorContains(t, err, "the watch ended")
}

func TestRunReturnsWatchError(t *testing.T) {
	fx := newFixture(t, nil)
	v, err := newVerifier(context.Background(), fx.cfg, &brokenWatch{fx.backend}, fx.greens, discardLogger())
	require.NoError(t, err)

	err = v.Run(context.Background())
	require.ErrorContains(t, err, "stream broke")
}

func TestRunAndRunOnceAreExclusive(t *testing.T) {
	fx := newFixture(t, nil)
	v := fx.verifier()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- v.Run(ctx) }()

	require.Eventually(t, func() bool {
		if v.running.TryLock() {
			v.running.Unlock()
			return false
		}
		return true
	}, 10*time.Second, time.Millisecond)

	_, _, err := v.RunOnce(context.Background())
	require.ErrorIs(t, err, ErrBusy)

	cancel()
	require.NoError(t, <-done)
}

func TestTesterMaterializesAnyCommit(t *testing.T) {
	fx := newFixture(t, nil)
	c1 := fx.commit("app.txt", "v1")
	c2 := fx.commit("app.txt", "v2")
	v := fx.verifier()

	ctx := context.Background()
	res, err := v.Test(ctx, c2)
	require.NoError(t, err)
	require.True(t, res.Passed)
	content, err := os.ReadFile(fx.treeFile("app.txt"))
	require.NoError(t, err)
	require.Equal(t, "v2", string(content))

	res, err = v.Test(ctx, c1)
	require.NoError(t, err)
	require.Equal(t, c1, res.Commit)
	content, err = os.ReadFile(fx.treeFile("app.txt"))
	require.NoError(t, err)
	require.Equal(t, "v1", string(content))
}

func TestTesterRejectsUnknownCommit(t *testing.T) {
	fx := newFixture(t, nil)
	fx.commit("a.txt", "one")
	v := fx.verifier()

	_, err := v.Test(context.Background(), "0123456789abcdef0123456789abcdef01234567")
	require.ErrorContains(t, err, "resolve")
	require.Empty(t, fx.runsLog(), "a missing commit is never tested")
}

// endedWatch is a backend whose changeset watch ends without an error.
type endedWatch struct {
	*liveBackend
}

func (endedWatch) WatchChangesets(context.Context, uint64) (<-chan workspace.Changeset, <-chan error) {
	out := make(chan workspace.Changeset)
	close(out)
	errs := make(chan error)
	close(errs)
	return out, errs
}

// brokenWatch is a backend whose changeset watch fails.
type brokenWatch struct {
	*liveBackend
}

func (brokenWatch) WatchChangesets(context.Context, uint64) (<-chan workspace.Changeset, <-chan error) {
	out := make(chan workspace.Changeset)
	close(out)
	errs := make(chan error, 1)
	errs <- errors.New("stream broke")
	close(errs)
	return out, errs
}
