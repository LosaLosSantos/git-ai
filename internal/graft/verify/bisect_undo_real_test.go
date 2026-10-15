package verify

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// scratchBranches lists the scratch branches left in the repository of env.
func scratchBranches(t *testing.T, env *realEnv, greens *refUpdater) []string {
	t.Helper()
	stream, err := gitalypb.NewRefServiceClient(greens.conn).ListRefs(env.ctx, &gitalypb.ListRefsRequest{
		Repository: greens.repository,
		Patterns:   [][]byte{[]byte("refs/heads/" + strings.TrimSuffix(ScratchBranchPrefix, "/"))},
	})
	require.NoError(t, err)
	var names []string
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return names
		}
		require.NoError(t, err)
		for _, ref := range resp.GetReferences() {
			names = append(names, string(ref.GetName()))
		}
	}
}

// TestRealServerAutoRevertFindsSecondCulpritBeforeRevert lands two breaking
// commits before the verifier runs. The first run reverts the first culprit;
// the second run must find the second culprit, which landed between the first
// culprit and its revert, instead of the first culprit again, and revert it,
// so that the third run is green.
func TestRealServerAutoRevertFindsSecondCulpritBeforeRevert(t *testing.T) {
	env := startRealServer(t)
	env.cfg.Test.Command = []string{"sh", "-c", "test ! -e broken && test ! -e broken2"}
	env.cfg.Revert = RevertConfig{Enabled: true, MaxPerHour: 10}

	v, closeVerifier := env.verifier()
	defer closeVerifier()

	greens, err := dialRefUpdater(env.ctx, env.cfg.Address, verifyToken, env.repo, env.cfg.Storage)
	require.NoError(t, err)
	defer func() { _ = greens.Close() }()

	bisector, err := NewBisector(env.cfg, greens.conn, greens.repository, &sdkBackend{repo: env.repo, refs: greens})
	require.NoError(t, err)
	require.NotNil(t, bisector.Scratch, "reverts that are not a dry run build scratch commits")
	notes := &recordingNotifier{}
	bisector.Reverts.Notifier = notes
	v.SetBisector(bisector)

	env.land("a.txt", "one")
	_, _, err = v.RunOnce(env.ctx)
	require.NoError(t, err)
	b := env.land("b.txt", "two")
	_, _, err = v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.Equal(t, b, env.green())

	broken := env.land("broken", "x")
	broken2 := env.land("broken2", "y")

	out, ran, err := v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.False(t, out.Passed)
	require.Empty(t, out.BisectError)
	require.Len(t, notes.outcomes, 1)
	require.Equal(t, StatusReverted, notes.outcomes[0].Status)
	require.Equal(t, broken, notes.outcomes[0].Culprit)

	// The tip is now the revert of broken, and broken2 still fails it.
	out, ran, err = v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.False(t, out.Passed)
	require.Empty(t, out.BisectError)
	require.Len(t, notes.outcomes, 2)
	require.Equal(t, StatusReverted, notes.outcomes[1].Status, notes.outcomes[1].Reason)
	require.Equal(t, broken2, notes.outcomes[1].Culprit)

	out, ran, err = v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.True(t, out.Passed, out.Output)
	require.True(t, out.GreenMoved)
	require.Equal(t, notes.outcomes[1].RevertCommit, env.green())

	require.Empty(t, scratchBranches(t, env, greens), "every scratch branch is deleted")
}

// TestRealServerRevertWithFailingWebhookIsRecorded checks that a revert is
// logged and reaches the outcome of the cycle when the webhook, the only
// notifier, fails.
func TestRealServerRevertWithFailingWebhookIsRecorded(t *testing.T) {
	var hits atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer hook.Close()

	env := startRealServer(t)
	env.cfg.Test.Command = []string{"sh", "-c", "test ! -e broken"}
	env.cfg.Revert = RevertConfig{Enabled: true, MaxPerHour: 3, WebhookURL: hook.URL}

	var logs bytes.Buffer
	v, closer, err := Open(env.ctx, env.cfg, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	require.NoError(t, err)
	defer func() { _ = closer.Close() }()

	env.land("a.txt", "one")
	_, _, err = v.RunOnce(env.ctx)
	require.NoError(t, err)
	env.land("b.txt", "two")
	_, _, err = v.RunOnce(env.ctx)
	require.NoError(t, err)

	env.land("broken", "x")
	d := env.land("d.txt", "four")
	out, ran, err := v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.False(t, out.Passed)

	tip, err := env.repo.ResolveRevision(env.ctx, "refs/heads/main")
	require.NoError(t, err)
	require.NotEqual(t, d, tip, "the revert landed")
	require.Equal(t, int32(1), hits.Load())

	require.Contains(t, out.BisectError, string(StatusReverted))
	require.Contains(t, out.BisectError, tip, "the outcome names the revert commit")
	require.Contains(t, out.BisectError, "500")

	log := logs.String()
	require.Contains(t, log, "bisect decision")
	require.Contains(t, log, "status=reverted")
	require.Contains(t, log, "revert_commit="+tip)
	require.Contains(t, log, "bisect_error=")
}
