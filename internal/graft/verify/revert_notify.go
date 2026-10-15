package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// DefaultWebhookTimeout bounds one webhook notification when the notifier sets
// no timeout.
const DefaultWebhookTimeout = 10 * time.Second

// Notifier reports the outcome of an automatic revert decision.
type Notifier interface {
	Notify(ctx context.Context, out RevertOutcome) error
}

// StdoutNotifier writes one human-readable line per outcome.
type StdoutNotifier struct {
	// Writer defaults to os.Stdout.
	Writer io.Writer
}

// Notify implements Notifier.
func (n *StdoutNotifier) Notify(_ context.Context, out RevertOutcome) error {
	w := n.Writer
	if w == nil {
		w = os.Stdout
	}
	_, err := fmt.Fprintf(w, "graft-verify: %s branch=%s tip=%s culprit=%s revert=%s dry_run=%t reason=%q\n",
		out.Status, out.Branch, out.Tip, out.Culprit, out.RevertCommit, out.DryRun, out.Reason)
	return err
}

// WebhookNotifier POSTs each outcome as a JSON object to URL. A response
// outside the 2xx range is an error.
type WebhookNotifier struct {
	URL string
	// Client defaults to http.DefaultClient.
	Client *http.Client
	// Timeout bounds one notification. Zero selects DefaultWebhookTimeout.
	Timeout time.Duration
}

// Notify implements Notifier.
func (n *WebhookNotifier) Notify(ctx context.Context, out RevertOutcome) error {
	if n.URL == "" {
		return fmt.Errorf("verify: webhook notifier has no URL")
	}

	body, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshal outcome: %w", err)
	}

	timeout := n.Timeout
	if timeout <= 0 {
		timeout = DefaultWebhookTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := n.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook answered %s", resp.Status)
	}
	return nil
}
