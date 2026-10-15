package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds one delivery when no timeout is configured.
const DefaultTimeout = 10 * time.Second

// maxErrorBody bounds how much of an error answer is read.
const maxErrorBody = 4096

// WebhookOptions configure Webhook and Slack.
type WebhookOptions struct {
	// Name is the configured name of the destination.
	Name string
	// URL is the HTTP(S) endpoint. It may carry a secret and is never echoed
	// in errors.
	URL string
	// Token, if set, is sent as "Authorization: Bearer <token>". Slack
	// rejects it because the secret is part of the URL.
	Token string
	// Timeout bounds one delivery; zero selects DefaultTimeout.
	Timeout time.Duration
}

// Webhook POSTs a digest as JSON to an HTTP(S) endpoint.
type Webhook struct {
	name   string
	url    string
	token  string
	client *http.Client
}

// NewWebhook validates the options and returns a webhook notifier.
func NewWebhook(o WebhookOptions) (*Webhook, error) {
	if o.Name == "" {
		return nil, errors.New("notify: webhook needs a name")
	}
	if err := ValidateEndpoint(o.URL); err != nil {
		return nil, err
	}
	timeout, err := timeoutOrDefault(o.Timeout)
	if err != nil {
		return nil, err
	}
	return &Webhook{name: o.Name, url: o.URL, token: o.Token, client: newClient(timeout)}, nil
}

// Name returns the configured name.
func (w *Webhook) Name() string { return w.name }

// Notify POSTs msg as JSON. Any answer other than 2xx is an error.
func (w *Webhook) Notify(ctx context.Context, msg Message) error {
	return postJSON(ctx, w.client, w.url, w.token, msg)
}

// Slack posts a digest to a Slack incoming webhook.
type Slack struct {
	name   string
	url    string
	client *http.Client
}

// NewSlack returns a Slack notifier. The incoming webhook URL is the secret.
func NewSlack(o WebhookOptions) (*Slack, error) {
	if o.Name == "" {
		return nil, errors.New("notify: slack needs a name")
	}
	if o.Token != "" {
		return nil, errors.New("notify: slack takes no token; the incoming webhook URL is the secret")
	}
	if err := ValidateEndpoint(o.URL); err != nil {
		return nil, err
	}
	timeout, err := timeoutOrDefault(o.Timeout)
	if err != nil {
		return nil, err
	}
	return &Slack{name: o.Name, url: o.URL, client: newClient(timeout)}, nil
}

// Name returns the configured name.
func (s *Slack) Name() string { return s.name }

// Notify posts the Slack payload for msg.
func (s *Slack) Notify(ctx context.Context, msg Message) error {
	return postJSON(ctx, s.client, s.url, "", slackPayload{Text: SlackText(msg)})
}

func timeoutOrDefault(d time.Duration) (time.Duration, error) {
	if d < 0 {
		return 0, fmt.Errorf("notify: negative timeout %s", d)
	}
	if d == 0 {
		return DefaultTimeout, nil
	}
	return d, nil
}

// ValidateEndpoint requires an absolute http or https URL. The error never
// contains the URL, which may carry a secret.
func ValidateEndpoint(raw string) error {
	if raw == "" {
		return errors.New("notify: endpoint URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("notify: endpoint URL is not valid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("notify: endpoint URL must use http or https")
	}
	if u.Host == "" {
		return errors.New("notify: endpoint URL has no host")
	}
	return nil
}

// newClient returns an HTTP client that does not follow redirects, so that a
// bearer token is never sent to another location.
func newClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// postJSON sends payload as JSON and requires a 2xx answer.
func postJSON(ctx context.Context, client *http.Client, endpoint, token string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("notify: encode payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("notify: build request")
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		// The transport error may repeat the URL; report only the cause class.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errors.New("notify: delivery failed")
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("notify: endpoint answered %d: %s", resp.StatusCode, strings.TrimSpace(string(answer)))
	}
	return nil
}
