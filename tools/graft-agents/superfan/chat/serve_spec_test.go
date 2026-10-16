package chat

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Black-box spec tests for WP50 (conversational superfan, Slack HTTP server).
// The spec names no Go API for the chat package, so these tests build the
// graft-superfan binary and drive `chat serve` over HTTP.
//
// ASSUMPTIONS the spec does not state (reconcile with the implementation):
//   - the env var holding the signing secret (specSecretEnv),
//   - the request paths for slash commands and Events API (specCommandsPath,
//     specEventsPath),
//   - the config file layout (only provider "none" is named by the spec),
//   - the Slack headers X-Slack-Request-Timestamp and X-Slack-Signature.

const (
	specPkg           = "github.com/feder-cr/git-ai/tools/graft-agents/cmd/graft-superfan"
	specSecretEnv     = "GRAFT_SUPERFAN_SLACK_SIGNING_SECRET" // gate wiring: the implementation's default env var
	specSecret        = "spec-signing-secret"
	specCommandsPath  = "/slack/commands"
	specEventsPath    = "/slack/events"
	specChallengeBody = `{"type":"url_verification","challenge":"spec-challenge-123"}`
)

func specBuild(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "graft-superfan")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, specPkg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", specPkg, err, out)
	}
	return bin
}

func specConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "superfan.json")
	// ASSUMPTION: only the provider value "none" is named by the spec.
	// Gate wiring: the implementation's configuration is JSON (llm.Config).
	if err := os.WriteFile(p, []byte(`{"provider":{"type":"none"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func specFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// specStartServer starts `graft-superfan chat serve` and returns its base URL.
func specStartServer(t *testing.T) string {
	t.Helper()
	bin := specBuild(t)
	cfg := specConfig(t)
	addr := specFreeAddr(t)

	var stderr bytes.Buffer
	cmd := exec.Command(bin, "chat", "serve", "--listen", addr, "--config", cfg)
	cmd.Env = append(os.Environ(), specSecretEnv+"="+specSecret)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not listen on %s; stderr:\n%s", addr, stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "http://" + addr
}

// specSign computes the Slack v0 signature: "v0=" + hex(HMAC-SHA256(secret, "v0:<ts>:<body>")).
func specSign(secret, ts, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":" + body))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func specNow() string { return strconv.FormatInt(time.Now().Unix(), 10) }

func specAgo(d time.Duration) string {
	return strconv.FormatInt(time.Now().Add(-d).Unix(), 10)
}

// specPost sends a request and returns the status code and body. Headers
// are only set when non-empty so tests can omit them.
func specPost(t *testing.T, base, path, contentType, body, ts, sig string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if ts != "" {
		req.Header.Set("X-Slack-Request-Timestamp", ts)
	}
	if sig != "" {
		req.Header.Set("X-Slack-Signature", sig)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSpecURLVerificationChallengeEchoed(t *testing.T) {
	base := specStartServer(t)
	ts := specNow()
	code, body := specPost(t, base, specEventsPath, "application/json",
		specChallengeBody, ts, specSign(specSecret, ts, specChallengeBody))
	if code != http.StatusOK {
		t.Fatalf("url_verification with valid signature: status %d, want 200", code)
	}
	if !strings.Contains(body, "spec-challenge-123") {
		t.Errorf("response %q does not echo the challenge", body)
	}
}

func TestSpecWrongSecretRejected(t *testing.T) {
	base := specStartServer(t)
	ts := specNow()
	code, _ := specPost(t, base, specEventsPath, "application/json",
		specChallengeBody, ts, specSign("not-the-secret", ts, specChallengeBody))
	if code < 400 || code >= 500 {
		t.Fatalf("wrong signature: status %d, want 4xx", code)
	}
}

func TestSpecTamperedBodyRejected(t *testing.T) {
	base := specStartServer(t)
	ts := specNow()
	tampered := `{"type":"url_verification","challenge":"tampered"}`
	code, _ := specPost(t, base, specEventsPath, "application/json",
		tampered, ts, specSign(specSecret, ts, specChallengeBody))
	if code < 400 || code >= 500 {
		t.Fatalf("signature over a different body: status %d, want 4xx", code)
	}
}

func TestSpecMissingSignatureRejected(t *testing.T) {
	base := specStartServer(t)
	code, _ := specPost(t, base, specEventsPath, "application/json",
		specChallengeBody, "", "")
	if code < 400 || code >= 500 {
		t.Fatalf("no signature headers: status %d, want 4xx", code)
	}
}

func TestSpecTimestampWindowIsFiveMinutes(t *testing.T) {
	base := specStartServer(t)

	// Six minutes old: older than the 5 minute window, must be rejected
	// even though the signature is valid for that timestamp.
	stale := specAgo(6 * time.Minute)
	code, _ := specPost(t, base, specEventsPath, "application/json",
		specChallengeBody, stale, specSign(specSecret, stale, specChallengeBody))
	if code < 400 || code >= 500 {
		t.Fatalf("timestamp 6m old: status %d, want 4xx", code)
	}

	// Four minutes old: inside the window, must be accepted.
	recent := specAgo(4 * time.Minute)
	code, body := specPost(t, base, specEventsPath, "application/json",
		specChallengeBody, recent, specSign(specSecret, recent, specChallengeBody))
	if code != http.StatusOK || !strings.Contains(body, "spec-challenge-123") {
		t.Fatalf("timestamp 4m old: status %d body %q, want 200 with challenge", code, body)
	}
}

func TestSpecSlashCommandAcksFastAndAnswersResponseURL(t *testing.T) {
	base := specStartServer(t)

	got := make(chan string, 4)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case got <- string(b):
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fake.Close()

	form := url.Values{
		"command":      {"/graft"},
		"text":         {"status of the component"},
		"response_url": {fake.URL + "/resp"},
		"user_id":      {"U1"},
		"channel_id":   {"C1"},
	}.Encode()

	ts := specNow()
	start := time.Now()
	code, _ := specPost(t, base, specCommandsPath, "application/x-www-form-urlencoded",
		form, ts, specSign(specSecret, ts, form))
	elapsed := time.Since(start)

	if code != http.StatusOK {
		t.Fatalf("slash command with valid signature: status %d, want 200", code)
	}
	if elapsed >= 3*time.Second {
		t.Errorf("slash command ack took %v, want < 3s", elapsed)
	}

	select {
	case body := <-got:
		if strings.TrimSpace(body) == "" {
			t.Errorf("answer posted to response_url is empty")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no answer was posted to response_url within 30s")
	}
}
