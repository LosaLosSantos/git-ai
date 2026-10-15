package chat

import (
	"context"
	"strings"
	"testing"
)

func TestRedactorBuiltins(t *testing.T) {
	r, err := newRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, in, want string
	}{
		{"key value", "password=hunter2 ok", "password=[REDACTED] ok"},
		{"json key", `{"token": "abcdef123"}`, `{"token": "[REDACTED]"}`},
		{"bearer", "curl with bearer abcdefghijklmnop", "curl with bearer [REDACTED]"},
		{"url credentials", "clone https://user:s3cr3tpass@git.example.com/repo", "clone https://user:[REDACTED]@git.example.com/repo"},
		{"aws key", "key AKIAABCDEFGHIJKLMNOP x", "key [REDACTED] x"},
		{"github token", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "[REDACTED]"},
		{"slack token", "xoxb-1234567890-abcdefghij", "[REDACTED]"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj\n-----END RSA PRIVATE KEY-----", "[REDACTED PRIVATE KEY]"},
		{"plain text untouched", "changeset 1a2b3c landed in 3 files", "changeset 1a2b3c landed in 3 files"},
		{"url without credentials untouched", "see https://git.example.com:8443/repo", "see https://git.example.com:8443/repo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Apply(tc.in); got != tc.want {
				t.Fatalf("Apply(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactorExtraPatterns(t *testing.T) {
	r, err := newRedactor([]string{`ticket-[0-9]+`})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Apply("ticket-42 is open"); got != "[REDACTED] is open" {
		t.Fatalf("Apply = %q", got)
	}
	if _, err := newRedactor([]string{"("}); err == nil {
		t.Fatal("invalid pattern accepted")
	}
}

func TestRedactorIsIdempotent(t *testing.T) {
	r, err := newRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	once := r.Apply("token=abc123 and https://u:pw@h/x")
	if twice := r.Apply(once); twice != once {
		t.Fatalf("second pass changed %q to %q", once, twice)
	}
	if strings.Contains(once, "abc123") || strings.Contains(once, ":pw@") {
		t.Fatalf("secret survived: %q", once)
	}
}

// TestRedactorHeadersAndTokens covers credentials that the first version of the
// redaction left behind: the whole Authorization and Cookie values, GitLab
// tokens and partial private keys from a log tail.
func TestRedactorHeadersAndTokens(t *testing.T) {
	r, err := newRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, in, want string
	}{
		{"basic authorization", "Authorization: Basic dXNlcjpodW50ZXIy", "Authorization: [REDACTED]"},
		{"basic authorization in log line", "GET /x 200 Authorization: Basic dXNlcjpodW50ZXIy end", "GET /x 200 Authorization: [REDACTED]"},
		{"proxy authorization", "Proxy-Authorization: Basic dXNlcjpodW50ZXIy", "Proxy-Authorization: [REDACTED]"},
		{"json authorization", `{"authorization": "Basic dXNlcjpodW50ZXIy", "n": 1}`, `{"authorization": "[REDACTED]", "n": 1}`},
		{"cookie header", "Cookie: a=1; sid=SUPERSECRETVALUE", "Cookie: [REDACTED]"},
		{"set-cookie header", "Set-Cookie: sid=SUPERSECRETVALUE; Path=/", "Set-Cookie: [REDACTED]"},
		{"gitlab personal token", "glpat-ABCDEFGHIJKLMNOPQRST used", "[REDACTED] used"},
		{"gitlab deploy token", "token gldt-ABCDEFGHIJKLMNOPQRSTUV", "token [REDACTED]"},
		{"dangling private key end", "MIIBOgIBAAJBAKjMIIBOgIBAAJBAKjMIIBOgIBAAJBAKjMIIBOgIBAAJBAKjMIIBOgIBAAJBAKj\nAAAAAAAAAAAAAAAAAAAAAAAA\n-----END RSA PRIVATE KEY-----", "[REDACTED PRIVATE KEY]"},
		{"dangling private key begin", "ok\n-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj", "ok\n[REDACTED PRIVATE KEY]"},
		{"commit ids untouched", "landed 0123456789abcdef0123456789abcdef01234567\nnext", "landed 0123456789abcdef0123456789abcdef01234567\nnext"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Apply(tc.in); got != tc.want {
				t.Fatalf("Apply(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactorEscapedAndQuotedValues covers JSON log lines that embed other
// JSON (escaped quotes around keys and values) and quoted values with spaces,
// which the first version of the key=value rule left (partly) in clear.
func TestRedactorEscapedAndQuotedValues(t *testing.T) {
	r, err := newRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, in, want, secret string
	}{
		{
			name:   "nested escaped json password",
			in:     `{"msg":"request body {\"password\":\"hunter2pass\"}"}`,
			want:   `{"msg":"request body {\"password\":\"[REDACTED]\"}"}`,
			secret: "hunter2pass",
		},
		{
			name:   "nested escaped json token",
			in:     `{"level":"info","grpc.request.payload":"{\"token\":\"supersecretvalue123\"}"}`,
			want:   `{"level":"info","grpc.request.payload":"{\"token\":\"[REDACTED]\"}"}`,
			secret: "supersecretvalue123",
		},
		{
			name:   "nested escaped authorization header",
			in:     `{"msg":"headers {\"Authorization\":\"Basic dXNlcjpodW50ZXIy\"}"}`,
			want:   `{"msg":"headers {\"Authorization\":\"[REDACTED]\"}"}`,
			secret: "dXNlcjpodW50ZXIy",
		},
		{
			name:   "nested escaped cookie header",
			in:     `{"msg":"headers {\"Cookie\":\"a=1; sid=SUPERSECRETVALUE\"}"}`,
			want:   `{"msg":"headers {\"Cookie\":\"[REDACTED]\"}"}`,
			secret: "SUPERSECRETVALUE",
		},
		{
			name:   "escaped quoted value after equals",
			in:     `{"msg":"connecting with password=\"hunter2pass\""}`,
			want:   `{"msg":"connecting with password=\"[REDACTED]\""}`,
			secret: "hunter2pass",
		},
		{
			name:   "escaped quoted value after colon",
			in:     `{"msg":"cfg client_secret: \"abcdefSECRET\""}`,
			want:   `{"msg":"cfg client_secret: \"[REDACTED]\""}`,
			secret: "abcdefSECRET",
		},
		{
			name:   "escaped quoted value with spaces",
			in:     `{"msg":"password=\"correct horse battery\" next"}`,
			want:   `{"msg":"password=\"[REDACTED]\" next"}`,
			secret: "horse",
		},
		{
			name:   "doubly escaped json",
			in:     `{"msg":"{\"inner\":\"{\\\"token\\\":\\\"deepsecretvalue\\\"}\"}"}`,
			want:   `{"msg":"{\"inner\":\"{\\\"token\\\":\\\"[REDACTED]\\\"}\"}"}`,
			secret: "deepsecretvalue",
		},
		{
			name:   "unterminated escaped quote",
			in:     `{"msg":"password=\"hunter2pass`,
			want:   `{"msg":"password=\"[REDACTED]`,
			secret: "hunter2pass",
		},
		{
			name:   "double-quoted value with spaces",
			in:     `password = "correct horse battery staple"`,
			want:   `password = "[REDACTED]"`,
			secret: "horse",
		},
		{
			name:   "json value with spaces",
			in:     `{"api_key": "two part", "n": 1}`,
			want:   `{"api_key": "[REDACTED]", "n": 1}`,
			secret: "part",
		},
		{
			name:   "json value with an escaped quote inside",
			in:     `{"password": "ab\"cd ef", "n": 1}`,
			want:   `{"password": "[REDACTED]", "n": 1}`,
			secret: "cd ef",
		},
		{
			name:   "single-quoted value with spaces",
			in:     `secret: 'my long secret' ok`,
			want:   `secret: '[REDACTED]' ok`,
			secret: "long",
		},
		{
			name:   "unterminated double quote",
			in:     `password="hunter2pass`,
			want:   `password="[REDACTED]`,
			secret: "hunter2pass",
		},
		{
			name:   "unquoted value with a backslash",
			in:     `password=ab\cdef ok`,
			want:   `password=[REDACTED] ok`,
			secret: "cdef",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Apply(tc.in)
			if got != tc.want {
				t.Fatalf("Apply(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, tc.secret) {
				t.Fatalf("Apply(%q) = %q leaks %q", tc.in, got, tc.secret)
			}
			if again := r.Apply(got); again != got {
				t.Fatalf("second pass changed %q to %q", got, again)
			}
		})
	}
}

// TestEscapedJSONLogSecretsDoNotReachThePrompt is the end-to-end form of the
// escaped-JSON leak: a JSON log line with an embedded request payload must not
// put the credential in the provider prompt.
func TestEscapedJSONLogSecretsDoNotReachThePrompt(t *testing.T) {
	logs := writeFile(t, "gitaly.log", `{"level":"info","grpc.request.payload":"{\"token\":\"supersecretvalue123\"}"}`+"\n"+
		`{"msg":"headers {\"Authorization\":\"Basic dXNlcjpodW50ZXIy\"}"}`+"\n"+
		`{"msg":"connecting with password=\"correct horse battery\""}`+"\n")
	p := &fakeProvider{reply: "ok"}
	e := newTestEngine(t, &Config{Sources: SourcesConfig{LogFiles: []string{logs}}}, p, nil)

	if _, err := e.Answer(context.Background(), "why the error in the log?"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	reqs := p.requests()
	if len(reqs) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(reqs))
	}
	if !strings.Contains(reqs[0].Prompt, `\"token\":\"[REDACTED]\"`) {
		t.Fatalf("log line missing from the prompt or not redacted: %q", reqs[0].Prompt)
	}
	for _, secret := range []string{"supersecretvalue123", "dXNlcjpodW50ZXIy", "horse"} {
		if strings.Contains(reqs[0].Prompt, secret) {
			t.Fatalf("prompt leaks %q: %q", secret, reqs[0].Prompt)
		}
	}
}

// TestRedactorNoLeakThroughWholeHeaders checks that no part of the credential
// survives, not only the expected placeholder.
func TestRedactorNoLeakThroughWholeHeaders(t *testing.T) {
	r, err := newRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		"Authorization: Basic dXNlcjpodW50ZXIy",
		"Cookie: a=1; sid=SUPERSECRETVALUE",
		"glpat-ABCDEFGHIJKLMNOPQRST",
	} {
		out := r.Apply(in)
		for _, secret := range []string{"dXNlcjpodW50ZXIy", "SUPERSECRETVALUE", "ABCDEFGHIJKLMNOPQRST"} {
			if strings.Contains(out, secret) {
				t.Fatalf("Apply(%q) = %q leaks %q", in, out, secret)
			}
		}
	}
}
