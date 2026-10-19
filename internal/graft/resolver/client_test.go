package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// sampleRequest is a request with a content conflict and a modify-delete
// conflict, as the server would build it.
func sampleRequest() Request {
	return Request{
		Repository: Repository{Storage: "default", RelativePath: "x.git"},
		Branch:     "main",
		Tip:        "tip-oid",
		Commit:     "commit-oid",
		MergeBase:  "base-oid",
		Messages:   Messages{Tip: "tip message", Commit: "commit message"},
		Actor:      "agent-7",
		Files: []File{
			{
				Path:   "a.txt",
				Kind:   KindContent,
				Base:   &Side{OID: "b1", Mode: ModeRegular, Content: []byte("base\n")},
				Ours:   &Side{OID: "o1", Mode: ModeRegular, Content: []byte("ours\n")},
				Theirs: &Side{OID: "t1", Mode: ModeRegular, Content: []byte("theirs\n")},
			},
			{
				Path:   "b.txt",
				Kind:   KindModifyDelete,
				Base:   &Side{OID: "b2", Mode: ModeRegular, Content: []byte("gone\n")},
				Ours:   nil,
				Theirs: &Side{OID: "t2", Mode: ModeRegular, Content: []byte("changed\n")},
			},
		},
	}
}

// validResponse answers sampleRequest: a.txt written with content, b.txt
// deleted.
func validResponse(content string) Response {
	return Response{
		Version:  ProtocolVersion,
		Resolver: "graft-resolve/test",
		Files: []ResponseFile{
			{Path: "a.txt", Action: ActionWrite, Mode: ModeRegular, Content: []byte(content)},
			{Path: "b.txt", Action: ActionDelete},
		},
		Explanation: "merged both sides",
	}
}

// writeScript writes an executable sh script with the given body into a new
// temporary directory and returns its path.
func writeScript(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "resolver.sh")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755))
	return path
}

// cannedCommand returns a command that discards its request and prints stdout.
func cannedCommand(t *testing.T, stdout []byte) []string {
	t.Helper()

	respFile := filepath.Join(t.TempDir(), "response.json")
	require.NoError(t, os.WriteFile(respFile, stdout, 0o644))
	return []string{writeScript(t, fmt.Sprintf("cat > /dev/null\ncat '%s'\n", respFile))}
}

// capturingCommand returns a command that stores its stdin in the returned
// file and prints stdout.
func capturingCommand(t *testing.T, stdout []byte) ([]string, string) {
	t.Helper()

	dir := t.TempDir()
	reqFile := filepath.Join(dir, "request.json")
	respFile := filepath.Join(dir, "response.json")
	require.NoError(t, os.WriteFile(respFile, stdout, 0o644))

	return []string{writeScript(t, fmt.Sprintf("cat > '%s'\ncat '%s'\n", reqFile, respFile))}, reqFile
}

func commandConfig(command []string) config.GraftResolver {
	return config.GraftResolver{Enabled: true, Command: command, Timeout: "10s"}
}

func newClient(t *testing.T, cfg config.GraftResolver) *Client {
	t.Helper()

	client, err := New(cfg)
	require.NoError(t, err)
	require.NotNil(t, client)
	return client
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

func requireUnix(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake resolver is a sh script")
	}
}

func TestNew_Disabled(t *testing.T) {
	t.Parallel()

	client, err := New(config.GraftResolver{Enabled: false, Command: []string{"/bin/true"}})
	require.NoError(t, err)
	require.Nil(t, client)
}

func TestNew_Errors(t *testing.T) {
	t.Parallel()

	emptyTokenFile := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.WriteFile(emptyTokenFile, []byte(" \n"), 0o600))

	for _, tc := range []struct {
		name string
		cfg  config.GraftResolver
	}{
		{name: "neither command nor url", cfg: config.GraftResolver{Enabled: true}},
		{name: "command and url", cfg: config.GraftResolver{Enabled: true, Command: []string{"/bin/true"}, URL: "https://r.example"}},
		{name: "empty command program", cfg: config.GraftResolver{Enabled: true, Command: []string{""}}},
		{name: "invalid timeout", cfg: config.GraftResolver{Enabled: true, Command: []string{"/bin/true"}, Timeout: "soon"}},
		{name: "zero timeout", cfg: config.GraftResolver{Enabled: true, Command: []string{"/bin/true"}, Timeout: "0s"}},
		{name: "negative max files", cfg: config.GraftResolver{Enabled: true, Command: []string{"/bin/true"}, MaxFiles: -1}},
		{name: "negative max file bytes", cfg: config.GraftResolver{Enabled: true, Command: []string{"/bin/true"}, MaxFileBytes: -1}},
		{name: "url without scheme", cfg: config.GraftResolver{Enabled: true, URL: "r.example/resolve"}},
		{name: "url with ftp scheme", cfg: config.GraftResolver{Enabled: true, URL: "ftp://r.example/resolve"}},
		{name: "url without host", cfg: config.GraftResolver{Enabled: true, URL: "https:///resolve"}},
		{name: "missing token file", cfg: config.GraftResolver{Enabled: true, URL: "https://r.example", TokenFile: filepath.Join(t.TempDir(), "missing")}},
		{name: "empty token file", cfg: config.GraftResolver{Enabled: true, URL: "https://r.example", TokenFile: emptyTokenFile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, err := New(tc.cfg)
			require.Error(t, err)
			require.Nil(t, client)
		})
	}
}

func TestNilClient(t *testing.T) {
	t.Parallel()

	var client *Client
	resp, err := client.Resolve(context.Background(), sampleRequest())
	require.ErrorIs(t, err, ErrDisabled)
	require.Nil(t, resp)
}

func TestResolve_CommandSuccess(t *testing.T) {
	requireUnix(t)

	command, reqFile := capturingCommand(t, mustJSON(t, validResponse("merged\n")))
	client := newClient(t, config.GraftResolver{Enabled: true, Command: command, MaxFileBytes: 4096})

	req := sampleRequest()
	req.Limits = Limits{MaxFileBytes: 1 << 30} // the caller's value must not leak through

	resp, err := client.Resolve(testhelper.Context(t), req)
	require.NoError(t, err)
	require.Equal(t, "graft-resolve/test", resp.Resolver)
	require.Equal(t, "merged both sides", resp.Explanation)
	require.Equal(t, []byte("merged\n"), resp.Files[0].Content)

	raw, err := os.ReadFile(reqFile)
	require.NoError(t, err)

	var sent Request
	require.NoError(t, json.Unmarshal(raw, &sent))
	require.Equal(t, ProtocolVersion, sent.Version)
	require.Equal(t, int64(4096), sent.Limits.MaxFileBytes)
	require.Equal(t, "agent-7", sent.Actor)
	require.Equal(t, "tip message", sent.Messages.Tip)
	require.Len(t, sent.Files, 2)
	require.Equal(t, []byte("ours\n"), sent.Files[0].Ours.Content)
	require.Nil(t, sent.Files[1].Ours, "an absent side stays absent on the wire")
}

func TestResolve_NonASCIIPathRoundTrips(t *testing.T) {
	requireUnix(t)

	// A path in valid UTF-8 reaches the resolver unchanged, so an answer that
	// echoes it is accepted.
	const p = "docs/café-ü.txt"
	resp := validResponse("merged\n")
	resp.Files[0].Path = p
	command, reqFile := capturingCommand(t, mustJSON(t, resp))
	client := newClient(t, commandConfig(command))

	req := sampleRequest()
	req.Files[0].Path = p

	got, err := client.Resolve(testhelper.Context(t), req)
	require.NoError(t, err)
	require.Equal(t, p, got.Files[0].Path)

	raw, err := os.ReadFile(reqFile)
	require.NoError(t, err)

	var sent Request
	require.NoError(t, json.Unmarshal(raw, &sent))
	require.Equal(t, p, sent.Files[0].Path)
}

func TestResolve_CommandDefaultLimits(t *testing.T) {
	requireUnix(t)

	command, reqFile := capturingCommand(t, mustJSON(t, validResponse("x")))
	client := newClient(t, commandConfig(command))

	_, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.NoError(t, err)

	raw, err := os.ReadFile(reqFile)
	require.NoError(t, err)

	var sent Request
	require.NoError(t, json.Unmarshal(raw, &sent))
	require.Equal(t, int64(config.DefaultGraftResolverMaxFileBytes), sent.Limits.MaxFileBytes)
}

func TestResolve_CommandRefusal(t *testing.T) {
	requireUnix(t)

	client := newClient(t, commandConfig([]string{writeScript(t, "cat > /dev/null\necho 'binary conflict' >&2\nexit 3\n")}))

	resp, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.Nil(t, resp)
	require.ErrorIs(t, err, ErrUnresolvable)
	require.False(t, errors.Is(err, ErrInvalidResponse))

	var unresolvable *UnresolvableError
	require.ErrorAs(t, err, &unresolvable)
	require.Equal(t, "binary conflict", unresolvable.Reason)
}

func TestResolve_CommandRefusalWithoutStderr(t *testing.T) {
	requireUnix(t)

	client := newClient(t, commandConfig([]string{writeScript(t, "cat > /dev/null\nexit 3\n")}))

	_, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.ErrorIs(t, err, ErrUnresolvable)
	require.ErrorContains(t, err, "exit status 3")
}

func TestResolve_CommandRefusalWithOutputIsStillRefusal(t *testing.T) {
	requireUnix(t)

	// A non-zero exit means refusal whatever the stdout says.
	stdout := mustJSON(t, validResponse("merged"))
	client := newClient(t, commandConfig([]string{writeScript(t, fmt.Sprintf("cat > /dev/null\nprintf '%%s' '%s'\necho nope >&2\nexit 1\n", stdout))}))

	_, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.ErrorIs(t, err, ErrUnresolvable)
	require.ErrorContains(t, err, "nope")
}

func TestResolve_CommandInvalidAnswers(t *testing.T) {
	requireUnix(t)

	mutate := func(f func(*Response)) []byte {
		resp := validResponse("merged\n")
		f(&resp)
		return mustJSON(t, resp)
	}

	for _, tc := range []struct {
		name    string
		stdout  []byte
		wantErr string
	}{
		{name: "not JSON", stdout: []byte("merged by magic"), wantErr: "invalid character"},
		{name: "empty output", stdout: nil, wantErr: "unexpected end of JSON input"},
		{name: "trailing garbage after the object", stdout: append(mustJSON(t, validResponse("x")), []byte(" trailing")...), wantErr: "invalid character"},
		{name: "wrong version", stdout: mutate(func(r *Response) { r.Version = 2 }), wantErr: "version 2"},
		{name: "path not conflicted", stdout: mutate(func(r *Response) { r.Files[0].Path = "/etc/shadow" }), wantErr: "not a conflicted path"},
		{name: "conflict marker", stdout: mutate(func(r *Response) {
			r.Files[0].Content = []byte("<<<<<<< ours\nx\n=======\ny\n>>>>>>> theirs\n")
		}), wantErr: "conflict marker"},
		{name: "invalid mode", stdout: mutate(func(r *Response) { r.Files[0].Mode = "100666" }), wantErr: "invalid mode"},
		{name: "unanswered path", stdout: mutate(func(r *Response) { r.Files = r.Files[:1] }), wantErr: "not answered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newClient(t, commandConfig(cannedCommand(t, tc.stdout)))

			resp, err := client.Resolve(testhelper.Context(t), sampleRequest())
			require.Nil(t, resp)
			require.ErrorIs(t, err, ErrInvalidResponse)
			require.False(t, errors.Is(err, ErrUnresolvable))
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestResolve_CommandOversizedOutput(t *testing.T) {
	requireUnix(t)

	// Two files of 1 KiB allow about 4 KiB of base64 plus the envelope; 300 KB
	// of output is far beyond that bound.
	client := newClient(t, config.GraftResolver{Enabled: true, Command: []string{writeScript(t, "cat > /dev/null\nhead -c 300000 /dev/zero\n")}, Timeout: "10s", MaxFileBytes: 1024})

	resp, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.Nil(t, resp)
	require.ErrorIs(t, err, ErrInvalidResponse)
	require.ErrorContains(t, err, "exceeds")
}

func TestResolve_CommandTimeout(t *testing.T) {
	requireUnix(t)

	client := newClient(t, config.GraftResolver{Enabled: true, Command: []string{writeScript(t, "cat > /dev/null\nexec sleep 30\n")}, Timeout: "200ms"})

	start := time.Now()
	resp, err := client.Resolve(testhelper.Context(t), sampleRequest())
	elapsed := time.Since(start)

	require.Nil(t, resp)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, errors.Is(err, ErrUnresolvable), "a timeout is not a refusal")
	require.ErrorContains(t, err, "timed out after 200ms")
	require.Less(t, elapsed, 10*time.Second, "the resolver must be killed, not waited for")
}

func TestResolve_CommandTimeoutWithGrandchild(t *testing.T) {
	requireUnix(t)

	// The shell keeps a background grandchild holding the output pipe open;
	// the resolver must still return at its deadline.
	client := newClient(t, config.GraftResolver{Enabled: true, Command: []string{writeScript(t, "cat > /dev/null\nsleep 5 &\nwait\n")}, Timeout: "200ms"})

	start := time.Now()
	_, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 15*time.Second)
}

func TestResolve_CallerCanceled(t *testing.T) {
	requireUnix(t)

	client := newClient(t, config.GraftResolver{Enabled: true, Command: []string{writeScript(t, "cat > /dev/null\nexec sleep 30\n")}, Timeout: "1m"})

	ctx, cancel := context.WithCancel(testhelper.Context(t))
	time.AfterFunc(100*time.Millisecond, cancel)

	_, err := client.Resolve(ctx, sampleRequest())
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, errors.Is(err, ErrUnresolvable))
}

func TestResolve_CommandNotFound(t *testing.T) {
	requireUnix(t)

	client := newClient(t, commandConfig([]string{filepath.Join(t.TempDir(), "no-such-resolver")}))

	resp, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.Nil(t, resp)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrUnresolvable))
	require.False(t, errors.Is(err, ErrInvalidResponse))
}

func TestResolve_CommandGetsMinimalEnvironment(t *testing.T) {
	requireUnix(t)

	t.Setenv("GRAFT_RESOLVE_TEST_SECRET", "hunter2")

	envFile := filepath.Join(t.TempDir(), "env.txt")
	command := []string{writeScript(t, fmt.Sprintf("cat > /dev/null\nenv > '%s'\n", envFile))}
	client := newClient(t, commandConfig(command))

	// The output is invalid on purpose: only the environment matters here.
	_, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.ErrorIs(t, err, ErrInvalidResponse)

	raw, err := os.ReadFile(envFile)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "GRAFT_RESOLVE_TEST_SECRET")
	require.NotContains(t, string(raw), "hunter2")
	require.Contains(t, string(raw), "PATH=")
}

func TestResolve_RequestChecksDoNotRunTheResolver(t *testing.T) {
	requireUnix(t)

	marker := filepath.Join(t.TempDir(), "ran")
	command := []string{writeScript(t, fmt.Sprintf("touch '%s'\ncat > /dev/null\n", marker))}

	for _, tc := range []struct {
		name    string
		cfg     func(*config.GraftResolver)
		mutate  func(*Request)
		wantErr string
	}{
		{
			name:    "more files than max_files",
			cfg:     func(c *config.GraftResolver) { c.MaxFiles = 1 },
			wantErr: "2 conflicted files, limit is 1",
		},
		{
			name:    "default max_files is 20",
			mutate:  func(r *Request) { r.Files = make([]File, 21) },
			wantErr: "21 conflicted files",
		},
		{
			name:    "side over max_file_bytes",
			cfg:     func(c *config.GraftResolver) { c.MaxFileBytes = 4 },
			wantErr: `path "a.txt" has a 5 byte version, limit is 4`,
		},
		{
			name:    "path traversal",
			mutate:  func(r *Request) { r.Files[0].Path = "../escape" },
			wantErr: "unsafe path",
		},
		{
			name:    "absolute path",
			mutate:  func(r *Request) { r.Files[0].Path = "/etc/passwd" },
			wantErr: "unsafe path",
		},
		{
			// JSON would turn the byte into U+FFFD, so the answer could never
			// match the request.
			name:    "path that is not valid UTF-8",
			mutate:  func(r *Request) { r.Files[0].Path = "caf\xe9.txt" },
			wantErr: "is not valid UTF-8",
		},
		{
			// Both would reach the resolver as the same name.
			name: "distinct paths that are not valid UTF-8",
			mutate: func(r *Request) {
				r.Files[0].Path = "x\xe9"
				r.Files[1].Path = "x\xe8"
			},
			wantErr: "is not valid UTF-8",
		},
		{
			name:    "duplicate path",
			mutate:  func(r *Request) { r.Files[1].Path = "a.txt" },
			wantErr: `path "a.txt" listed more than once`,
		},
		{
			name:    "unknown kind",
			mutate:  func(r *Request) { r.Files[0].Kind = "rename" },
			wantErr: `unknown kind "rename"`,
		},
		{
			name:    "denied path",
			cfg:     func(c *config.GraftResolver) { c.PathsDeny = []string{"*.txt"} },
			wantErr: `path "a.txt" is not allowed to be resolved`,
		},
		{
			name:    "path outside paths_allow",
			cfg:     func(c *config.GraftResolver) { c.PathsAllow = []string{"docs/**"} },
			wantErr: "is not allowed to be resolved",
		},
		{
			name:    "actor not in actors",
			cfg:     func(c *config.GraftResolver) { c.Actors = []string{"human"} },
			wantErr: `actor "agent-7" may not request conflict resolution`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := commandConfig(command)
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}

			client := newClient(t, cfg)

			req := sampleRequest()
			if tc.mutate != nil {
				tc.mutate(&req)
			}

			resp, err := client.Resolve(testhelper.Context(t), req)
			require.Nil(t, resp)
			require.ErrorContains(t, err, tc.wantErr)
			require.NoFileExists(t, marker, "a rejected request must never reach the resolver")
		})
	}
}

func TestResolve_ConcurrentCalls(t *testing.T) {
	requireUnix(t)

	client := newClient(t, commandConfig(cannedCommand(t, mustJSON(t, validResponse("merged\n")))))

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			resp, err := client.Resolve(context.Background(), sampleRequest())
			if err == nil && string(resp.Files[0].Content) != "merged\n" {
				err = fmt.Errorf("unexpected content %q", resp.Files[0].Content)
			}
			errs <- err
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func TestParseRefusal(t *testing.T) {
	t.Parallel()

	refusal, ok := parseRefusal([]byte(`{"error":"no"}`))
	require.True(t, ok)
	require.Equal(t, "no", refusal.Reason)
	require.Nil(t, refusal.Files)

	refusal, ok = parseRefusal([]byte(`{"error":"no","files":[{"path":"a","outcome":"rejected","reason":"syntax"}]}`))
	require.True(t, ok)
	require.Equal(t, []FileOutcome{{Path: "a", Outcome: OutcomeRejected, Reason: "syntax"}}, refusal.Files)

	refusal, ok = parseRefusal([]byte(`{"error":"` + strings.Repeat("x", maxErrorBodyBytes+10) + `"}`))
	require.True(t, ok)
	require.Len(t, refusal.Reason, maxErrorBodyBytes)

	for _, raw := range []string{`not json`, `{"error":"   "}`, `{"version":1,"files":[]}`} {
		_, ok := parseRefusal([]byte(raw))
		require.False(t, ok, raw)
	}
}

func TestValidOutcomes(t *testing.T) {
	t.Parallel()

	req := Request{Files: []File{{Path: "a"}, {Path: "b"}}}
	good := []FileOutcome{{Path: "b", Outcome: OutcomeFailed}, {Path: "a", Outcome: OutcomeResolved}}
	require.Equal(t, good, validOutcomes(req, good), "any order")

	for name, files := range map[string][]FileOutcome{
		"missing":        {{Path: "a", Outcome: OutcomeResolved}},
		"extra":          {{Path: "a", Outcome: OutcomeResolved}, {Path: "b", Outcome: OutcomeFailed}, {Path: "c", Outcome: OutcomeFailed}},
		"duplicate":      {{Path: "a", Outcome: OutcomeResolved}, {Path: "a", Outcome: OutcomeFailed}},
		"unknown path":   {{Path: "a", Outcome: OutcomeResolved}, {Path: "z", Outcome: OutcomeFailed}},
		"unknown status": {{Path: "a", Outcome: OutcomeResolved}, {Path: "b", Outcome: "maybe"}},
	} {
		require.Nil(t, validOutcomes(req, files), name)
	}
}
