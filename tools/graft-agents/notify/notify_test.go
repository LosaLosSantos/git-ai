package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

var testStart = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func testMessage() Message {
	return Message{
		Kind:        KindDigest,
		Component:   "payments",
		WindowStart: testStart,
		WindowEnd:   testStart.Add(time.Hour),
		Events:      3,
		Risk:        "high",
		Summary:     "Two lands touched the ledger.",
		Findings: []Finding{
			{
				Rule:       "protected-path",
				Severity:   "high",
				Summary:    "touches protected paths: ledger/a.go",
				Sequence:   7,
				Reference:  "refs/heads/main",
				Actor:      "agent-7",
				OID:        strings.Repeat("a", 40),
				RecordedAt: testStart.Add(5 * time.Minute),
			},
		},
	}
}

func TestTextRendersDigest(t *testing.T) {
	text := Text(testMessage())
	for _, want := range []string{
		"[digest] component=payments",
		"window=2026-10-01T12:00:00Z..2026-10-01T13:00:00Z",
		"events=3 risk=high",
		"Two lands touched the ledger.",
		"- [high/protected-path] touches protected paths: ledger/a.go (seq 7, refs/heads/main, actor agent-7)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Text() missing %q in:\n%s", want, text)
		}
	}
}

func TestTextOmittedAndEmptyKind(t *testing.T) {
	msg := testMessage()
	msg.Kind = ""
	msg.Omitted = 4
	msg.Findings[0].Actor = ""
	text := Text(msg)
	if !strings.HasPrefix(text, "[digest]") {
		t.Errorf("empty kind should render as digest: %q", text)
	}
	if !strings.Contains(text, "... 4 more findings omitted") {
		t.Errorf("omitted count missing: %q", text)
	}
	if strings.Contains(text, "actor") {
		t.Errorf("empty actor should not be rendered: %q", text)
	}
}

func TestStdoutWritesText(t *testing.T) {
	var buf bytes.Buffer
	n := NewStdout("console", &buf)
	if n.Name() != "console" {
		t.Fatalf("Name() = %q", n.Name())
	}
	if err := n.Notify(context.Background(), testMessage()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if buf.String() != Text(testMessage()) {
		t.Errorf("stdout output differs from Text()")
	}
}

func TestStdoutHonorsCanceledContext(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewStdout("console", &buf).Notify(ctx, testMessage()); err == nil {
		t.Fatal("Notify with a canceled context succeeded")
	}
	if buf.Len() != 0 {
		t.Errorf("canceled notify wrote %q", buf.String())
	}
}

func TestStdoutConcurrentWritesDoNotInterleave(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	w := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, string(p))
		return len(p), nil
	})
	n := NewStdout("console", w)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := n.Notify(context.Background(), testMessage()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(lines) != 20 {
		t.Fatalf("got %d writes, want 20", len(lines))
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestValidateEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		ok   bool
	}{
		{"https", "https://hooks.example.com/x", true},
		{"http", "http://127.0.0.1:9000/", true},
		{"empty", "", false},
		{"ftp", "ftp://example.com/", false},
		{"no host", "https:///path", false},
		{"relative", "/hooks/x", false},
		{"garbage", "http://[::1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEndpoint(tc.url)
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateEndpoint(%q) = %v, want ok=%v", tc.url, err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), tc.url) && tc.url != "" {
				t.Errorf("error echoes the URL: %v", err)
			}
		})
	}
}

func TestNewWebhookValidation(t *testing.T) {
	if _, err := NewWebhook(WebhookOptions{URL: "https://example.com"}); err == nil {
		t.Error("webhook without a name accepted")
	}
	if _, err := NewWebhook(WebhookOptions{Name: "w", URL: "https://example.com", Timeout: -time.Second}); err == nil {
		t.Error("negative timeout accepted")
	}
	w, err := NewWebhook(WebhookOptions{Name: "w", URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Name() != "w" || w.client.Timeout != DefaultTimeout {
		t.Errorf("webhook defaults wrong: name=%q timeout=%s", w.Name(), w.client.Timeout)
	}
}

func TestWebhookPostsJSONWithToken(t *testing.T) {
	var gotMethod, gotType, gotAuth string
	var got Message
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotType = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	w, err := NewWebhook(WebhookOptions{Name: "hook", URL: srv.URL, Token: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Notify(context.Background(), testMessage()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotMethod != http.MethodPost || gotType != "application/json" {
		t.Errorf("method=%s content-type=%s", gotMethod, gotType)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if got.Component != "payments" || got.Events != 3 || len(got.Findings) != 1 || got.Findings[0].Sequence != 7 {
		t.Errorf("payload round trip lost data: %+v", got)
	}
}

func TestWebhookNoTokenNoAuthorization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Authorization header")
		}
		rw.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	w, _ := NewWebhook(WebhookOptions{Name: "hook", URL: srv.URL})
	if err := w.Notify(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookErrorAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(rw, "slow down")
	}))
	defer srv.Close()
	w, _ := NewWebhook(WebhookOptions{Name: "hook", URL: srv.URL})
	err := w.Notify(context.Background(), testMessage())
	if err == nil {
		t.Fatal("429 accepted")
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "slow down") {
		t.Errorf("error lacks status and body: %v", err)
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error echoes the endpoint URL: %v", err)
	}
}

func TestWebhookRefusesRedirects(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hits++
		rw.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	w, _ := NewWebhook(WebhookOptions{Name: "hook", URL: redirect.URL, Token: "s3cret"})
	if err := w.Notify(context.Background(), testMessage()); err == nil {
		t.Fatal("redirect was followed")
	}
	if hits != 0 {
		t.Errorf("token-bearing request reached the redirect target %d times", hits)
	}
}

func TestWebhookUnreachableAndCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	w, _ := NewWebhook(WebhookOptions{Name: "hook", URL: url, Timeout: time.Second})
	err := w.Notify(context.Background(), testMessage())
	if err == nil || strings.Contains(err.Error(), url) {
		t.Fatalf("unreachable endpoint: err=%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Notify(ctx, testMessage()); err == nil {
		t.Fatal("canceled delivery succeeded")
	}
}

func TestWebhookTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	w, _ := NewWebhook(WebhookOptions{Name: "hook", URL: srv.URL, Timeout: 50 * time.Millisecond})
	if err := w.Notify(context.Background(), testMessage()); err == nil {
		t.Fatal("slow endpoint did not time out")
	}
}

func TestNewSlackRejectsToken(t *testing.T) {
	if _, err := NewSlack(WebhookOptions{Name: "s", URL: "https://hooks.slack.com/services/x", Token: "t"}); err == nil {
		t.Error("slack accepted a bearer token")
	}
	if _, err := NewSlack(WebhookOptions{URL: "https://hooks.slack.com/services/x"}); err == nil {
		t.Error("slack without a name accepted")
	}
	if _, err := NewSlack(WebhookOptions{Name: "s", URL: "ftp://x"}); err == nil {
		t.Error("slack accepted an ftp URL")
	}
}

func TestSlackPostsText(t *testing.T) {
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("slack request carries Authorization")
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		io.WriteString(rw, "ok")
	}))
	defer srv.Close()
	s, err := NewSlack(WebhookOptions{Name: "ops", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if s.Name() != "ops" {
		t.Errorf("Name() = %q", s.Name())
	}
	if err := s.Notify(context.Background(), testMessage()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(body["text"], "payments") || !strings.Contains(body["text"], "agent-7") {
		t.Errorf("slack text = %q", body["text"])
	}
}

func TestSlackTextEscapesInjection(t *testing.T) {
	msg := testMessage()
	msg.Component = "<!channel>"
	msg.Summary = "see <https://evil.example|here> & more"
	msg.Findings[0].Summary = "<@U123> & <!here>"
	msg.Findings[0].Actor = "a<b>"
	text := SlackText(msg)
	for _, bad := range []string{"<!channel>", "<https://evil", "<@U123>", "<!here>", "<b>"} {
		if strings.Contains(text, bad) {
			t.Errorf("slack text contains unescaped %q: %s", bad, text)
		}
	}
	for _, good := range []string{"&lt;!channel&gt;", "&amp;"} {
		if !strings.Contains(text, good) {
			t.Errorf("slack text lacks %q: %s", good, text)
		}
	}
}

func TestSlackTextTruncatesOnRuneBoundary(t *testing.T) {
	msg := testMessage()
	msg.Summary = strings.Repeat("é", maxSlackText)
	text := SlackText(msg)
	if !strings.HasSuffix(text, "… truncated") {
		t.Fatalf("long text not truncated: %d bytes", len(text))
	}
	body := strings.TrimSuffix(text, "\n… truncated")
	if len(body) > maxSlackText {
		t.Errorf("body is %d bytes, limit %d", len(body), maxSlackText)
	}
	if !utf8.ValidString(body) {
		t.Fatal("truncation cut inside a multi-byte rune")
	}
}

func TestSlackTextUnassignedComponent(t *testing.T) {
	msg := testMessage()
	msg.Component = ""
	if !strings.Contains(SlackText(msg), "`unassigned`") {
		t.Errorf("empty component not rendered as unassigned")
	}
}

func TestSlackTextOmitted(t *testing.T) {
	msg := testMessage()
	msg.Omitted = 9
	if !strings.Contains(SlackText(msg), "9 more findings omitted") {
		t.Errorf("omitted note missing")
	}
}
