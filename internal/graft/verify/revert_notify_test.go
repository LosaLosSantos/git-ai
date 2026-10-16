package verify

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStdoutNotifier(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	n := &StdoutNotifier{Writer: &buf}

	err := n.Notify(testhelper.Context(t), RevertOutcome{
		Status:       StatusDryRun,
		Branch:       "main",
		Tip:          "tip",
		Culprit:      "culprit",
		RevertCommit: "",
		DryRun:       true,
		Reason:       "dry run",
	})
	require.NoError(t, err)
	require.Equal(t, "graft-verify: dry-run branch=main tip=tip culprit=culprit revert= dry_run=true reason=\"dry run\"\n", buf.String())
}

func TestWebhookNotifier(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	t.Run("posts the outcome as JSON", func(t *testing.T) {
		var got RevertOutcome
		var contentType string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			contentType = r.Header.Get("Content-Type")
			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			assert.NoError(t, json.Unmarshal(body, &got))
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		out := RevertOutcome{Status: StatusReverted, Branch: "main", Culprit: "c1", RevertCommit: "r1", Reason: "x"}
		require.NoError(t, (&WebhookNotifier{URL: srv.URL}).Notify(ctx, out))
		require.Equal(t, "application/json", contentType)
		require.Equal(t, StatusReverted, got.Status)
		require.Equal(t, "c1", got.Culprit)
		require.Equal(t, "r1", got.RevertCommit)
	})

	t.Run("non-2xx answer is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusInternalServerError)
		}))
		defer srv.Close()

		err := (&WebhookNotifier{URL: srv.URL}).Notify(ctx, RevertOutcome{})
		require.ErrorContains(t, err, "500")
	})

	t.Run("slow webhook times out", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		}))
		defer srv.Close()

		start := time.Now()
		err := (&WebhookNotifier{URL: srv.URL, Timeout: 50 * time.Millisecond}).Notify(ctx, RevertOutcome{})
		require.Error(t, err)
		require.Less(t, time.Since(start), 5*time.Second)
	})

	t.Run("missing URL is an error", func(t *testing.T) {
		require.Error(t, (&WebhookNotifier{}).Notify(ctx, RevertOutcome{}))
	})

	t.Run("unreachable webhook is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()

		require.Error(t, (&WebhookNotifier{URL: url, Timeout: time.Second}).Notify(ctx, RevertOutcome{}))
	})
}
