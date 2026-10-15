package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/tools/graft-agents/llm"
)

const testSecret = "signing-secret-for-tests"

// fixedNow is the clock of the Slack tests.
var fixedNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// fakeProvider is an llm.Provider that records its requests. When block is
// set, Complete waits until it is closed.
type fakeProvider struct {
	mu    sync.Mutex
	reply string
	err   error
	block chan struct{}
	reqs  []llm.Request
}

func (f *fakeProvider) Name() string { return "fake:test" }

func (f *fakeProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &llm.Response{Text: f.reply, Model: "fake"}, nil
}

func (f *fakeProvider) requests() []llm.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llm.Request(nil), f.reqs...)
}

// writeFile writes content to a file in a fresh temporary directory.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeSlack records the chat.postMessage calls and the response_url posts.
type fakeSlack struct {
	mu        sync.Mutex
	posts     []map[string]string
	auths     []string
	responses []map[string]string
}

func newFakeSlack(t *testing.T) (*fakeSlack, *httptest.Server) {
	t.Helper()
	fs := &fakeSlack{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/chat.postMessage") {
			fs.posts = append(fs.posts, body)
			fs.auths = append(fs.auths, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		fs.responses = append(fs.responses, body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hs.Close)
	return fs, hs
}

func (f *fakeSlack) snapshot() (posts []map[string]string, auths []string, responses []map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string(nil), f.posts...),
		append([]string(nil), f.auths...),
		append([]map[string]string(nil), f.responses...)
}

// newTestEngine builds an engine that uses the given provider and runner.
func newTestEngine(t *testing.T, cfg *Config, p llm.Provider, run CommandRunner) *Engine {
	t.Helper()
	e, err := NewEngine(Options{Config: cfg, Provider: p, Run: run})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

// newTestServer serves the Slack front end with a fixed clock.
func newTestServer(t *testing.T, e *Engine, botToken, apiBase string, maxConcurrent int) (*Server, *httptest.Server) {
	t.Helper()
	srv, err := NewServer(ServerOptions{
		Engine:        e,
		SigningSecret: testSecret,
		BotToken:      botToken,
		APIBase:       apiBase,
		Now:           func() time.Time { return fixedNow },
		MaxConcurrent: maxConcurrent,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return srv, hs
}

// postSigned sends a request signed with secret at time ts and returns the
// status and the body of the response.
func postSigned(t *testing.T, target, contentType string, body []byte, secret string, ts time.Time) (int, string) {
	t.Helper()
	stamp := strconv.FormatInt(ts.Unix(), 10)
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(SlackTimestampHeader, stamp)
	req.Header.Set(SlackSignatureHeader, SignSlack(secret, stamp, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}
