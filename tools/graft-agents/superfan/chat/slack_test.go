package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// slashBody is the form Slack posts for a slash command.
func slashBody(text, responseURL string) []byte {
	return []byte(url.Values{
		"command":      {"/graft"},
		"text":         {text},
		"response_url": {responseURL},
		"user_name":    {"tester"},
	}.Encode())
}

// mentionBody is the Events API envelope of an app_mention.
func mentionBody(t *testing.T, text, channel, ts, threadTS string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type": "event_callback",
		"event": map[string]any{
			"type":      "app_mention",
			"text":      text,
			"channel":   channel,
			"ts":        ts,
			"thread_ts": threadTS,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNewServerValidation(t *testing.T) {
	e := newTestEngine(t, &Config{}, nil, nil)
	if _, err := NewServer(ServerOptions{SigningSecret: testSecret}); err == nil {
		t.Fatal("server without an engine accepted")
	}
	if _, err := NewServer(ServerOptions{Engine: e}); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("server without a secret: err = %v", err)
	}
}

func TestSlackURLVerification(t *testing.T) {
	e := newTestEngine(t, &Config{}, nil, nil)
	_, hs := newTestServer(t, e, "", "", 0)

	body := []byte(`{"type":"url_verification","challenge":"abc123"}`)
	status, out := postSigned(t, hs.URL+"/slack/events", "application/json", body, testSecret, fixedNow)
	if status != http.StatusOK || out != "abc123" {
		t.Fatalf("challenge: status %d, body %q", status, out)
	}
}

func TestSlackRejectsBadRequests(t *testing.T) {
	p := &fakeProvider{reply: "should not be called"}
	e := newTestEngine(t, &Config{}, p, nil)
	_, hs := newTestServer(t, e, "", "", 0)
	body := slashBody("what changed?", "http://127.0.0.1:1/response")

	tests := []struct {
		name   string
		secret string
		ts     time.Time
	}{
		{name: "wrong secret", secret: "other", ts: fixedNow},
		{name: "stale", secret: testSecret, ts: fixedNow.Add(-time.Hour)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := postSigned(t, hs.URL+"/slack/commands", "application/x-www-form-urlencoded", body, tc.secret, tc.ts)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", status)
			}
		})
	}

	resp, err := http.Post(hs.URL+"/slack/events", "application/json", bytes.NewReader(mentionBody(t, "<@U1> hi", "C1", "1.1", "")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned event: status = %d, want 401", resp.StatusCode)
	}
	if n := len(p.requests()); n != 0 {
		t.Fatalf("rejected requests reached the engine %d times", n)
	}
}

func TestSlackCommandAnswersToResponseURL(t *testing.T) {
	p := &fakeProvider{reply: "Two changes landed <!channel> & more"}
	e := newTestEngine(t, &Config{}, p, nil)
	srv, hs := newTestServer(t, e, "", "", 0)
	fake, fakeHS := newFakeSlack(t)

	responseURL := fakeHS.URL + "/response/T0/secret-token"
	status, out := postSigned(t, hs.URL+"/slack/commands", "application/x-www-form-urlencoded", slashBody("what changed?", responseURL), testSecret, fixedNow)
	if status != http.StatusOK || out != "" {
		t.Fatalf("ack: status %d, body %q", status, out)
	}
	srv.Wait()

	_, _, responses := fake.snapshot()
	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}
	if responses[0]["response_type"] != "ephemeral" {
		t.Fatalf("response_type = %q", responses[0]["response_type"])
	}
	want := "Two changes landed &lt;!channel&gt; &amp; more"
	if responses[0]["text"] != want {
		t.Fatalf("text = %q, want %q", responses[0]["text"], want)
	}
}

func TestSlackCommandUsageAndValidation(t *testing.T) {
	e := newTestEngine(t, &Config{}, nil, nil)
	_, hs := newTestServer(t, e, "", "", 0)

	status, out := postSigned(t, hs.URL+"/slack/commands", "application/x-www-form-urlencoded", slashBody("   ", "http://127.0.0.1:1/x"), testSecret, fixedNow)
	if status != http.StatusOK || !strings.Contains(out, "Usage: /graft") || !strings.Contains(out, "ephemeral") {
		t.Fatalf("usage: status %d, body %q", status, out)
	}

	status, _ = postSigned(t, hs.URL+"/slack/commands", "application/x-www-form-urlencoded", slashBody("hi", ""), testSecret, fixedNow)
	if status != http.StatusBadRequest {
		t.Fatalf("missing response_url: status %d, want 400", status)
	}
}

func TestSlackCommandProviderFailureIsGeneric(t *testing.T) {
	p := &fakeProvider{err: errors.New("upstream said: key sk-internal-detail")}
	e := newTestEngine(t, &Config{}, p, nil)
	srv, hs := newTestServer(t, e, "", "", 0)
	fake, fakeHS := newFakeSlack(t)

	status, _ := postSigned(t, hs.URL+"/slack/commands", "application/x-www-form-urlencoded", slashBody("status?", fakeHS.URL+"/r"), testSecret, fixedNow)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	srv.Wait()
	_, _, responses := fake.snapshot()
	if len(responses) != 1 || responses[0]["text"] != failedText {
		t.Fatalf("responses = %+v", responses)
	}
}

func TestSlackEventAnswersInThread(t *testing.T) {
	p := &fakeProvider{reply: "All verification checks pass."}
	e := newTestEngine(t, &Config{}, p, nil)
	fake, fakeHS := newFakeSlack(t)
	srv, hs := newTestServer(t, e, "xoxb-test-token", fakeHS.URL+"/api", 0)

	body := mentionBody(t, "<@UBOT> what is the &lt;status&gt;?", "C42", "1700000000.000100", "")
	status, _ := postSigned(t, hs.URL+"/slack/events", "application/json", body, testSecret, fixedNow)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	srv.Wait()

	reqs := p.requests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].Prompt, "what is the <status>?") {
		t.Fatalf("question not unescaped in the prompt: %+v", reqs)
	}

	posts, auths, _ := fake.snapshot()
	if len(posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(posts))
	}
	if posts[0]["channel"] != "C42" || posts[0]["thread_ts"] != "1700000000.000100" {
		t.Fatalf("post = %+v", posts[0])
	}
	if posts[0]["text"] != "All verification checks pass." {
		t.Fatalf("text = %q", posts[0]["text"])
	}
	if auths[0] != "Bearer xoxb-test-token" {
		t.Fatalf("Authorization = %q", auths[0])
	}
}

func TestSlackEventUsesExistingThread(t *testing.T) {
	p := &fakeProvider{reply: "ok"}
	e := newTestEngine(t, &Config{}, p, nil)
	fake, fakeHS := newFakeSlack(t)
	srv, hs := newTestServer(t, e, "xoxb-t", fakeHS.URL+"/api", 0)

	body := mentionBody(t, "<@UBOT> status?", "C1", "2.2", "1.1")
	postSigned(t, hs.URL+"/slack/events", "application/json", body, testSecret, fixedNow)
	srv.Wait()

	posts, _, _ := fake.snapshot()
	if len(posts) != 1 || posts[0]["thread_ts"] != "1.1" {
		t.Fatalf("posts = %+v", posts)
	}
}

func TestSlackEventIgnoredCases(t *testing.T) {
	cases := map[string]struct {
		body    []byte
		headers map[string]string
	}{
		"bot message": {
			body: []byte(`{"type":"event_callback","event":{"type":"app_mention","text":"<@UBOT> hi","channel":"C1","ts":"1.1","bot_id":"B1"}}`),
		},
		"other event type": {
			body: []byte(`{"type":"event_callback","event":{"type":"message","text":"hi","channel":"C1","ts":"1.1"}}`),
		},
		"empty question": {
			body: []byte(`{"type":"event_callback","event":{"type":"app_mention","text":"<@UBOT>  ","channel":"C1","ts":"1.1"}}`),
		},
		"slack retry": {
			body:    []byte(`{"type":"event_callback","event":{"type":"app_mention","text":"<@UBOT> hi","channel":"C1","ts":"1.1"}}`),
			headers: map[string]string{"X-Slack-Retry-Num": "1"},
		},
		"unknown envelope": {
			body: []byte(`{"type":"app_rate_limited"}`),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := &fakeProvider{reply: "should not be called"}
			e := newTestEngine(t, &Config{}, p, nil)
			fake, fakeHS := newFakeSlack(t)
			srv, hs := newTestServer(t, e, "xoxb-t", fakeHS.URL+"/api", 0)

			stamp := strconv.FormatInt(fixedNow.Unix(), 10)
			req, err := http.NewRequest(http.MethodPost, hs.URL+"/slack/events", bytes.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(SlackTimestampHeader, stamp)
			req.Header.Set(SlackSignatureHeader, SignSlack(testSecret, stamp, tc.body))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			srv.Wait()
			if n := len(p.requests()); n != 0 {
				t.Fatalf("engine called %d times", n)
			}
			if posts, _, _ := fake.snapshot(); len(posts) != 0 {
				t.Fatalf("posted %d messages", len(posts))
			}
		})
	}
}

// postEvent sends a signed Events API request with extra headers.
func postEvent(t *testing.T, target string, body []byte, headers map[string]string) int {
	t.Helper()
	stamp := strconv.FormatInt(fixedNow.Unix(), 10)
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SlackTimestampHeader, stamp)
	req.Header.Set(SlackSignatureHeader, SignSlack(testSecret, stamp, body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// mentionBodyWithID is mentionBody with an event_id in the envelope.
func mentionBodyWithID(t *testing.T, eventID, text, channel, ts string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type":     "event_callback",
		"event_id": eventID,
		"event": map[string]any{
			"type":    "app_mention",
			"text":    text,
			"channel": channel,
			"ts":      ts,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSlackRetryOfUndeliveredEventIsAnswered covers a retry whose first
// delivery never reached the server (connection failure, restart): it must be
// answered, not dropped because it carries a retry header.
func TestSlackRetryOfUndeliveredEventIsAnswered(t *testing.T) {
	p := &fakeProvider{reply: "answer"}
	e := newTestEngine(t, &Config{}, p, nil)
	fake, fakeHS := newFakeSlack(t)
	srv, hs := newTestServer(t, e, "xoxb-t", fakeHS.URL+"/api", 0)

	body := mentionBodyWithID(t, "Ev01RETRY", "<@UBOT> status?", "C1", "1.1")
	status := postEvent(t, hs.URL+"/slack/events", body, map[string]string{
		"X-Slack-Retry-Num":    "1",
		"X-Slack-Retry-Reason": "connection_failed",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	srv.Wait()
	if n := len(p.requests()); n != 1 {
		t.Fatalf("engine called %d times, want 1", n)
	}
	if posts, _, _ := fake.snapshot(); len(posts) != 1 || posts[0]["thread_ts"] != "1.1" {
		t.Fatalf("posts = %+v, want one answer in thread 1.1", posts)
	}
}

// TestSlackDuplicateEventIsAnsweredOnce checks the deduplication by event_id:
// the retries of a delivered event and a replay of the same signed request are
// acknowledged without a second answer, while another event is answered.
func TestSlackDuplicateEventIsAnsweredOnce(t *testing.T) {
	p := &fakeProvider{reply: "answer"}
	e := newTestEngine(t, &Config{}, p, nil)
	fake, fakeHS := newFakeSlack(t)
	srv, hs := newTestServer(t, e, "xoxb-t", fakeHS.URL+"/api", 0)

	body := mentionBodyWithID(t, "Ev01DUP", "<@UBOT> status?", "C1", "1.1")
	for i, headers := range []map[string]string{
		nil,
		{"X-Slack-Retry-Num": "1", "X-Slack-Retry-Reason": "http_timeout"},
		{"X-Slack-Retry-Num": "2", "X-Slack-Retry-Reason": "http_timeout"},
		nil,
	} {
		if status := postEvent(t, hs.URL+"/slack/events", body, headers); status != http.StatusOK {
			t.Fatalf("delivery %d: status = %d, want 200", i, status)
		}
	}
	other := mentionBodyWithID(t, "Ev02OTHER", "<@UBOT> status?", "C1", "2.2")
	if status := postEvent(t, hs.URL+"/slack/events", other, nil); status != http.StatusOK {
		t.Fatalf("other event: status = %d, want 200", status)
	}
	srv.Wait()

	if n := len(p.requests()); n != 2 {
		t.Fatalf("engine called %d times, want 2", n)
	}
	posts, _, _ := fake.snapshot()
	if len(posts) != 2 {
		t.Fatalf("posts = %+v, want 2", posts)
	}
	threads := map[string]bool{posts[0]["thread_ts"]: true, posts[1]["thread_ts"]: true}
	if !threads["1.1"] || !threads["2.2"] {
		t.Fatalf("posts = %+v, want one answer per event", posts)
	}
}

func TestSlackFirstDeliveryExpiresAndIsBounded(t *testing.T) {
	now := fixedNow
	srv, err := NewServer(ServerOptions{
		Engine:        newTestEngine(t, &Config{}, nil, nil),
		SigningSecret: testSecret,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	if !srv.firstDelivery("Ev1") {
		t.Fatal("first delivery reported as a duplicate")
	}
	now = now.Add(eventDedupTTL - time.Second)
	if srv.firstDelivery("Ev1") {
		t.Fatal("duplicate within the TTL reported as a first delivery")
	}
	now = now.Add(2 * time.Second)
	if !srv.firstDelivery("Ev1") {
		t.Fatal("delivery after the TTL reported as a duplicate")
	}

	for i := 0; i < maxSeenEvents+10; i++ {
		now = now.Add(time.Millisecond)
		srv.firstDelivery("bulk" + strconv.Itoa(i))
	}
	srv.seenMu.Lock()
	n := len(srv.seen)
	_, oldestKept := srv.seen["bulk0"]
	_, newestKept := srv.seen["bulk"+strconv.Itoa(maxSeenEvents+9)]
	srv.seenMu.Unlock()
	if n > maxSeenEvents {
		t.Fatalf("remembered %d event ids, want at most %d", n, maxSeenEvents)
	}
	if oldestKept || !newestKept {
		t.Fatalf("eviction kept the oldest (%v) or dropped the newest (%v)", oldestKept, !newestKept)
	}
}

func TestSlackEventWithoutBotTokenIsDropped(t *testing.T) {
	p := &fakeProvider{reply: "answer"}
	e := newTestEngine(t, &Config{}, p, nil)
	fake, fakeHS := newFakeSlack(t)
	srv, hs := newTestServer(t, e, "", fakeHS.URL+"/api", 0)

	status, _ := postSigned(t, hs.URL+"/slack/events", "application/json", mentionBody(t, "<@UBOT> hi", "C1", "1.1", ""), testSecret, fixedNow)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	srv.Wait()
	if posts, _, _ := fake.snapshot(); len(posts) != 0 {
		t.Fatalf("posted without a bot token: %+v", posts)
	}
}

func TestSlackEventMalformedBody(t *testing.T) {
	e := newTestEngine(t, &Config{}, nil, nil)
	_, hs := newTestServer(t, e, "", "", 0)
	status, _ := postSigned(t, hs.URL+"/slack/events", "application/json", []byte(`{"type":`), testSecret, fixedNow)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestSlackBusyWhenConcurrencyIsExhausted(t *testing.T) {
	block := make(chan struct{})
	p := &fakeProvider{reply: "first answer", block: block}
	e := newTestEngine(t, &Config{}, p, nil)
	fake, fakeHS := newFakeSlack(t)
	srv, hs := newTestServer(t, e, "", "", 1)

	status, _ := postSigned(t, hs.URL+"/slack/commands", "application/x-www-form-urlencoded", slashBody("first?", fakeHS.URL+"/r1"), testSecret, fixedNow)
	if status != http.StatusOK {
		t.Fatalf("first: status %d", status)
	}

	status, out := postSigned(t, hs.URL+"/slack/commands", "application/x-www-form-urlencoded", slashBody("second?", fakeHS.URL+"/r2"), testSecret, fixedNow)
	if status != http.StatusOK || !strings.Contains(out, "busy") {
		t.Fatalf("second: status %d, body %q", status, out)
	}

	close(block)
	srv.Wait()
	_, _, responses := fake.snapshot()
	if len(responses) != 1 || responses[0]["text"] != "first answer" {
		t.Fatalf("responses = %+v", responses)
	}
}

func TestSlackPostResponseFailureIsLogged(t *testing.T) {
	e := newTestEngine(t, &Config{}, &fakeProvider{reply: "x"}, nil)
	srv, hs := newTestServer(t, e, "", "", 0)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)

	status, _ := postSigned(t, hs.URL+"/slack/commands", "application/x-www-form-urlencoded", slashBody("q?", failing.URL+"/r"), testSecret, fixedNow)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	// The failed post is logged in the background; Wait must still return.
	srv.Wait()
}

func TestServeStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, "127.0.0.1:0", http.NotFoundHandler())
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop after cancellation")
	}
}
