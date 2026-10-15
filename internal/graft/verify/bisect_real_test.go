package verify

import (
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
	"github.com/stretchr/testify/require"
)

// landWith commits path with the message on main, dated when unless it is zero,
// and returns the new tip. The branch must exist.
func landWith(t *testing.T, env *realEnv, path, content, message string, when time.Time) string {
	t.Helper()
	res, err := env.repo.CommitFiles(env.ctx, sdk.CommitOptions{
		Branch:    "main",
		StartRev:  "main",
		Message:   message,
		Author:    sdk.Signature{Name: "dev", Email: "dev@example.com"},
		User:      sdk.User{ID: "dev", Username: "dev", Name: "dev", Email: "dev@example.com"},
		Actions:   []sdk.Action{sdk.Put{Path: path, Content: []byte(content)}},
		Timestamp: when,
	})
	require.NoError(t, err)
	return res.CommitID
}

// realReverter returns the lister and the reverter of the verifier over env, and
// the auto-reverter that uses them with the given cap.
func realReverter(t *testing.T, env *realEnv, maxPerHour int) (*CommitLister, *AutoReverter) {
	t.Helper()
	greens, err := dialRefUpdater(env.ctx, env.cfg.Address, env.cfg.Token, env.repo, env.cfg.Storage)
	require.NoError(t, err)
	t.Cleanup(func() { _ = greens.Close() })

	lister := &CommitLister{Commits: gitalypb.NewCommitServiceClient(greens.conn), Repository: greens.repository}
	reverter := &OperationReverter{
		Operations: gitalypb.NewOperationServiceClient(greens.conn),
		Repository: greens.repository,
		TipOf:      BranchTip(&sdkBackend{repo: env.repo}),
	}
	committer := Signature{Name: DefaultRevertCommitterName, Email: DefaultRevertCommitterEmail}
	return lister, &AutoReverter{Reverter: reverter, History: lister, Policy: RevertPolicy{MaxPerHour: maxPerHour, Committer: committer}}
}

// TestRealServerRevertCapCountsOldDatedTip checks that a tip with an old committer
// date does not hide the reverts below it from the cap. The walk of the server
// used to stop at the old tip, so a second revert in the hour was allowed.
func TestRealServerRevertCapCountsOldDatedTip(t *testing.T) {
	env := startRealServer(t)
	lister, auto := realReverter(t, env, 1)

	env.land("a.txt", "one")
	b := env.land("b.txt", "two")
	out, err := auto.Apply(env.ctx, "main", &BisectResult{Tip: b, Culprit: &CommitInfo{ID: b, Subject: "add b.txt", Message: "add b.txt"}})
	require.NoError(t, err)
	require.Equal(t, StatusReverted, out.Status)

	// c is dated three hours back, as a fast-forward of an old commit is.
	c := landWith(t, env, "c.txt", "three", "add c.txt", time.Now().Add(-3*time.Hour))

	history, err := lister.CommitsSince(env.ctx, "main", time.Now().Add(-time.Hour))
	require.NoError(t, err)
	reverts := 0
	for _, h := range history {
		if hasAutoRevertTrailer(h.Message) {
			reverts++
		}
	}
	require.Equal(t, 1, reverts, "the revert below the old tip is counted")

	out, err = auto.Apply(env.ctx, "main", &BisectResult{Tip: c, Culprit: &CommitInfo{ID: c, Subject: "add c.txt", Message: "add c.txt"}})
	require.NoError(t, err)
	require.Equal(t, StatusRefused, out.Status)
	require.Contains(t, out.Reason, "cap reached")
}

// TestRealServerRevertCapWithHistoryBeyondBound checks against a real server
// that a branch with more first-parent commits than the lister's bound keeps
// the cap working: the newest commits are listed, the revert among them is
// counted, and a revert below the cap is still allowed.
func TestRealServerRevertCapWithHistoryBeyondBound(t *testing.T) {
	env := startRealServer(t)
	lister, auto := realReverter(t, env, 5)

	old := time.Now().Add(-3 * time.Hour)
	env.land("a.txt", "one")
	x := landWith(t, env, "x.txt", "two", "add x.txt", old)
	out, err := auto.Apply(env.ctx, "main", &BisectResult{Tip: x, Culprit: &CommitInfo{ID: x, Subject: "add x.txt", Message: "add x.txt"}})
	require.NoError(t, err)
	require.Equal(t, StatusReverted, out.Status)
	c := landWith(t, env, "c.txt", "three", "add c.txt", old)

	// The newest three commits are c, the revert and x; a and the commits below
	// it are beyond the bound.
	lister.MaxCommits = 3
	history, err := lister.CommitsSince(env.ctx, "main", time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, out.RevertCommit, history[0].ID)

	culprit := &CommitInfo{ID: c, Subject: "add c.txt", Message: "add c.txt"}
	auto.Policy.MaxPerHour = 1
	out, err = auto.Apply(env.ctx, "main", &BisectResult{Tip: c, Culprit: culprit})
	require.NoError(t, err)
	require.Equal(t, StatusRefused, out.Status)
	require.Contains(t, out.Reason, "cap reached")

	auto.Policy.MaxPerHour = 2
	out, err = auto.Apply(env.ctx, "main", &BisectResult{Tip: c, Culprit: culprit})
	require.NoError(t, err)
	require.Equal(t, StatusReverted, out.Status, out.Reason)
}

// TestRealServerRevertOfLongSubjectKeepsTrailer checks that the revert of a
// culprit with a subject longer than the server's message bound still carries
// the trailer, so that the revert is recognized and cannot be reverted again.
func TestRealServerRevertOfLongSubjectKeepsTrailer(t *testing.T) {
	env := startRealServer(t)
	lister, auto := realReverter(t, env, 10)

	env.land("a.txt", "one")
	long := strings.Repeat("s", 12*1024)
	culprit := landWith(t, env, "long.txt", "x", long+"\n\nbody", time.Time{})

	out, err := auto.Apply(env.ctx, "main", &BisectResult{Tip: culprit, Culprit: &CommitInfo{ID: culprit, Subject: long, Message: long + "\n\nbody"}})
	require.NoError(t, err)
	require.Equal(t, StatusReverted, out.Status)

	history, err := lister.CommitsSince(env.ctx, "main", time.Now().Add(-time.Hour))
	require.NoError(t, err)
	revert := history[len(history)-1]
	require.Equal(t, out.RevertCommit, revert.ID)
	require.False(t, revert.Truncated)
	require.True(t, hasAutoRevertTrailer(revert.Message), "the trailer survives the server's message bound")
	require.Equal(t, DefaultRevertCommitterEmail, revert.CommitterEmail)

	out, err = auto.Apply(env.ctx, "main", &BisectResult{Tip: revert.ID, Culprit: &revert})
	require.NoError(t, err)
	require.Equal(t, StatusRefused, out.Status)
	require.Contains(t, out.Reason, "auto-revert")
}

// TestRealServerAutoRevert lands a, b, broken and d on a real server, verifies
// the tips and checks the whole pipeline: the bisection finds broken, the revert
// lands on d with the trailer and the verifier's committer, the next run is
// green, and the guards hold against the real server (stale tips, reverts of
// reverts, a second revert of the same culprit, and the cap across instances).
func TestRealServerAutoRevert(t *testing.T) {
	env := startRealServer(t)
	env.cfg.Test.Command = []string{"sh", "-c", "test ! -e broken"}
	env.cfg.Revert = RevertConfig{Enabled: true, MaxPerHour: 3}

	v, closeVerifier := env.verifier()
	defer closeVerifier()

	greens, err := dialRefUpdater(env.ctx, env.cfg.Address, verifyToken, env.repo, env.cfg.Storage)
	require.NoError(t, err)
	defer func() { _ = greens.Close() }()

	backend := &sdkBackend{repo: env.repo}
	bisector, err := NewBisector(env.cfg, greens.conn, greens.repository, backend)
	require.NoError(t, err)
	notes := &recordingNotifier{}
	bisector.Reverts.Notifier = notes
	v.SetBisector(bisector)

	committer := Signature{Name: DefaultRevertCommitterName, Email: DefaultRevertCommitterEmail}
	lister := &CommitLister{Commits: gitalypb.NewCommitServiceClient(greens.conn), Repository: greens.repository}
	reverter := &OperationReverter{
		Operations: gitalypb.NewOperationServiceClient(greens.conn),
		Repository: greens.repository,
		TipOf:      BranchTip(backend),
	}

	// The green baseline is b.
	env.land("a.txt", "one")
	_, ran, err := v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.True(t, ran)
	b := env.land("b.txt", "two")
	_, _, err = v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.Equal(t, b, env.green())

	// broken is the culprit, and d is the failing tip.
	broken := env.land("broken", "x")
	d := env.land("d.txt", "four")
	out, ran, err := v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.False(t, out.Passed)
	require.Equal(t, d, out.Tip)

	require.Len(t, notes.outcomes, 1)
	require.Equal(t, StatusReverted, notes.outcomes[0].Status)
	require.Equal(t, broken, notes.outcomes[0].Culprit)

	tip, err := env.repo.ResolveRevision(env.ctx, "refs/heads/main")
	require.NoError(t, err)
	require.NotEqual(t, d, tip)

	// The revert is the first-parent child of d, with the message and the
	// committer of the verifier.
	onD, err := lister.FirstParentRange(env.ctx, d, tip)
	require.NoError(t, err)
	require.Len(t, onD, 1, "the revert's parent is d")
	require.Equal(t, tip, onD[0].ID)

	history, err := lister.CommitsSince(env.ctx, "main", time.Now().Add(-time.Hour))
	require.NoError(t, err)
	revert := history[len(history)-1]
	require.Equal(t, tip, revert.ID)
	require.Equal(t, `Revert "add broken"`, revert.Subject)
	require.True(t, hasAutoRevertTrailer(revert.Message))
	require.Contains(t, revert.Message, AutoRevertTrailer+": "+broken)
	require.Equal(t, committer.Email, revert.CommitterEmail)

	// The revert fixes the tip, so the next run goes green on it.
	out, ran, err = v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.True(t, out.Passed, out.Output)
	require.Equal(t, tip, env.green())

	t.Run("a stale expected tip is ErrBranchMoved", func(t *testing.T) {
		// d is not undone on the branch, so the server gets to the compare-and-swap.
		_, err := reverter.Revert(env.ctx, RevertRequest{
			Branch: "main", Commit: d, ExpectedOld: b, Message: "x", Committer: committer,
		})
		require.ErrorIs(t, err, ErrBranchMoved)
	})

	t.Run("a revert of an auto-revert is refused", func(t *testing.T) {
		auto := &AutoReverter{Reverter: reverter, History: lister, Policy: RevertPolicy{MaxPerHour: 10, Committer: committer}}
		out, err := auto.Apply(env.ctx, "main", &BisectResult{Tip: tip, Culprit: &revert})
		require.NoError(t, err)
		require.Equal(t, StatusRefused, out.Status)
		require.Contains(t, out.Reason, "auto-revert")
	})

	t.Run("the same culprit again is already reverted", func(t *testing.T) {
		auto := &AutoReverter{Reverter: reverter, History: lister, Policy: RevertPolicy{MaxPerHour: 10, Committer: committer}}
		culprit := CommitInfo{ID: broken, Subject: "add broken", Message: "add broken"}
		out, err := auto.Apply(env.ctx, "main", &BisectResult{Tip: tip, Culprit: &culprit})
		require.NoError(t, err)
		require.Equal(t, StatusAlreadyReverted, out.Status)
	})

	t.Run("the cap counts the reverts of the server in a fresh instance", func(t *testing.T) {
		// A new process has no memory of the revert, but the branch has it.
		capped := &AutoReverter{Reverter: reverter, History: lister, Policy: RevertPolicy{MaxPerHour: 1, Committer: committer}}
		culprit := CommitInfo{ID: d, Subject: "add d.txt", Message: "add d.txt"}
		out, err := capped.Apply(env.ctx, "main", &BisectResult{Tip: tip, Culprit: &culprit})
		require.NoError(t, err)
		require.Equal(t, StatusRefused, out.Status)
		require.Contains(t, out.Reason, "cap reached")
	})
}
