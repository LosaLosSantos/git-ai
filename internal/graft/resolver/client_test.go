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
		// The three sides and the resolution of a file must fit one check request.
		{name: "max file bytes over the check bound", cfg: config.GraftResolver{Enabled: true, Command: []string{"/bin/true"}, MaxFileBytes: MaxCheckFileBytes + 1}},
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
	resp, err := client.resolve(context.Background(), sampleRequest())
	require.ErrorIs(t, err, ErrDisabled)
	require.Nil(t, resp)
}

func TestResolve_CommandSuccess(t *testing.T) {
	requireUnix(t)

	command, reqFile := capturingCommand(t, mustJSON(t, validResponse("merged\n")))
	client := newClient(t, config.GraftResolver{Enabled: true, Command: command, MaxFileBytes: 4096})

	req := sampleRequest()
	req.Limits = Limits{MaxFileBytes: 1 << 30} // the caller's value must not leak through

	resp, err := client.resolve(testhelper.Context(t), req)
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

	got, err := client.resolve(testhelper.Context(t), req)
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

	_, err := client.resolve(testhelper.Context(t), sampleRequest())
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

	resp, err := client.resolve(testhelper.Context(t), sampleRequest())
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

	_, err := client.resolve(testhelper.Context(t), sampleRequest())
	require.ErrorIs(t, err, ErrUnresolvable)
	require.ErrorContains(t, err, "exit status 3")
}

func TestResolve_CommandRefusalWithOutputIsStillRefusal(t *testing.T) {
	requireUnix(t)

	// A non-zero exit means refusal whatever the stdout says.
	stdout := mustJSON(t, validResponse("merged"))
	client := newClient(t, commandConfig([]string{writeScript(t, fmt.Sprintf("cat > /dev/null\nprintf '%%s' '%s'\necho nope >&2\nexit 1\n", stdout))}))

	_, err := client.resolve(testhelper.Context(t), sampleRequest())
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

			resp, err := client.resolve(testhelper.Context(t), sampleRequest())
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

	resp, err := client.resolve(testhelper.Context(t), sampleRequest())
	require.Nil(t, resp)
	require.ErrorIs(t, err, ErrInvalidResponse)
	require.ErrorContains(t, err, "exceeds")
}

func TestResolve_CommandTimeout(t *testing.T) {
	requireUnix(t)

	client := newClient(t, config.GraftResolver{Enabled: true, Command: []string{writeScript(t, "cat > /dev/null\nexec sleep 30\n")}, Timeout: "200ms"})

	start := time.Now()
	resp, err := client.resolve(testhelper.Context(t), sampleRequest())
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
	_, err := client.resolve(testhelper.Context(t), sampleRequest())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 15*time.Second)
}

func TestResolve_CallerCanceled(t *testing.T) {
	requireUnix(t)

	client := newClient(t, config.GraftResolver{Enabled: true, Command: []string{writeScript(t, "cat > /dev/null\nexec sleep 30\n")}, Timeout: "1m"})

	ctx, cancel := context.WithCancel(testhelper.Context(t))
	time.AfterFunc(100*time.Millisecond, cancel)

	_, err := client.resolve(ctx, sampleRequest())
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, errors.Is(err, ErrUnresolvable))
}

func TestResolve_CommandNotFound(t *testing.T) {
	requireUnix(t)

	client := newClient(t, commandConfig([]string{filepath.Join(t.TempDir(), "no-such-resolver")}))

	resp, err := client.resolve(testhelper.Context(t), sampleRequest())
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
	_, err := client.resolve(testhelper.Context(t), sampleRequest())
	require.ErrorIs(t, err, ErrInvalidResponse)

	raw, err := os.ReadFile(envFile)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "GRAFT_RESOLVE_TEST_SECRET")
	require.NotContains(t, string(raw), "hunter2")
	require.Contains(t, string(raw), "PATH=")
}

// TestCheck_runsForEveryActorAndFileCount: actors and max_files bound requests for an AI
// resolution only; the check of an agent's resolution runs whatever the actor and the number of
// files (PLAN10 12), while a resolve request with the same actor and files is refused.
func TestCheck_runsForEveryActorAndFileCount(t *testing.T) {
	requireUnix(t)

	req := sampleRequest()
	req.Resolution = validResponse("merged\n").Files
	answer, err := json.Marshal(CheckResponse{
		Version: ProtocolVersion, Operation: OperationCheck, Resolver: "graft-resolve",
		Files: []CheckFile{{Path: "a.txt", OK: true, Syntax: SyntaxNone}, {Path: "b.txt", OK: true, Syntax: SyntaxNone}},
	})
	require.NoError(t, err)
	cfg := commandConfig(cannedCommand(t, answer))
	cfg.MaxFiles = 1
	cfg.Actors = []string{"someone-else"}
	client := newClient(t, cfg)

	resp, err := checkWithSlot(t, client, req)
	require.NoError(t, err)
	require.Len(t, resp.Files, 2)

	_, err = client.resolve(testhelper.Context(t), req)
	require.ErrorContains(t, err, "2 conflicted files, limit is 1")
	cfg.MaxFiles = 0
	_, err = newClient(t, cfg).resolve(testhelper.Context(t), req)
	require.ErrorContains(t, err, `actor "agent-7" may not request conflict resolution`)
}

// TestCheck_refusesARequestOverItsBound: a check request is bounded in bytes whatever its files;
// one over MaxCheckRequestBytes is refused without running the resolver (the server splits an
// agent's resolution below it).
func TestCheck_refusesARequestOverItsBound(t *testing.T) {
	requireUnix(t)

	marker := filepath.Join(t.TempDir(), "ran")
	cfg := commandConfig([]string{writeScript(t, fmt.Sprintf("touch '%s'\ncat > /dev/null\n", marker))})
	cfg.MaxFileBytes = MaxCheckFileBytes
	client := newClient(t, cfg)

	// Five sides of max_file_bytes each, over the bound once encoded.
	req := sampleRequest()
	big := make([]byte, MaxCheckFileBytes)
	for _, side := range []*Side{req.Files[0].Base, req.Files[0].Ours, req.Files[0].Theirs, req.Files[1].Base, req.Files[1].Theirs} {
		side.Content = big
	}
	req.Resolution = validResponse("merged\n").Files
	_, err := checkWithSlot(t, client, req)
	require.ErrorContains(t, err, "is over the bound")
	require.NoFileExists(t, marker)
}

// TestCheck_aFileAtTheBoundFitsOneRequest: with max_file_bytes at MaxCheckFileBytes, the largest
// the configuration accepts (one byte more is refused there: TestGraftResolverValidateBuildable), a file whose three sides and
// resolution are all that large, with a path of 4 KiB that JSON escapes at six bytes a byte, fits
// one check request.
func TestCheck_aFileAtTheBoundFitsOneRequest(t *testing.T) {
	requireUnix(t)

	path := strings.Repeat("<", 4<<10)
	big := make([]byte, MaxCheckFileBytes)
	for i := range big {
		big[i] = 'x'
	}
	req := sampleRequest()
	req.Files = []File{{
		Path: path, Kind: KindContent,
		Base:   &Side{OID: "b1", Mode: ModeRegular, Content: big},
		Ours:   &Side{OID: "o1", Mode: ModeRegular, Content: big},
		Theirs: &Side{OID: "t1", Mode: ModeRegular, Content: big},
	}}
	req.Resolution = []ResponseFile{{Path: path, Action: ActionWrite, Mode: ModeRegular, Content: big}}

	answer, err := json.Marshal(CheckResponse{
		Version: ProtocolVersion, Operation: OperationCheck, Resolver: "graft-resolve",
		Files: []CheckFile{{Path: path, OK: true, Syntax: SyntaxNone}},
	})
	require.NoError(t, err)
	cfg := commandConfig(cannedCommand(t, answer))
	cfg.MaxFileBytes = MaxCheckFileBytes

	resp, err := checkWithSlot(t, newClient(t, cfg), req)
	require.NoError(t, err)
	require.Len(t, resp.Files, 1)
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

			resp, err := client.resolve(testhelper.Context(t), req)
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

			resp, err := client.resolve(context.Background(), sampleRequest())
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
		"all resolved":   {{Path: "a", Outcome: OutcomeResolved}, {Path: "b", Outcome: OutcomeResolved}},
	} {
		require.Nil(t, validOutcomes(req, files), name)
	}
}

// checkWithSlot sends one check request with a check slot, as the server does.
func checkWithSlot(t *testing.T, client *Client, req Request) (*CheckResponse, error) {
	t.Helper()
	slot, err := client.AcquireCheckSlot(testhelper.Context(t))
	require.NoError(t, err)
	defer slot.Release()
	return slot.Check(testhelper.Context(t), req)
}

// TestAcquireCheckSlot_boundsTheChecksAtOnce: at most check_concurrency slots are held at once; a
// wait for one counts against the caller's deadline and fails when it ends; a released slot frees
// its place once, and cannot check any more.
func TestAcquireCheckSlot_boundsTheChecksAtOnce(t *testing.T) {
	cfg := commandConfig([]string{"/bin/true"})
	cfg.CheckConcurrency = 2
	client := newClient(t, cfg)
	ctx := testhelper.Context(t)

	first, err := client.AcquireCheckSlot(ctx)
	require.NoError(t, err)
	second, err := client.AcquireCheckSlot(ctx)
	require.NoError(t, err)

	// Both slots are held: a third waits until its deadline, then fails.
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = client.AcquireCheckSlot(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "waiting for a check slot of the resolver")

	// A slot released twice frees one place only, and checks no more.
	first.Release()
	first.Release()
	_, err = first.Check(ctx, sampleRequest())
	require.ErrorContains(t, err, "check without a check slot")

	third, err := client.AcquireCheckSlot(ctx)
	require.NoError(t, err)
	waitCtx2, cancel2 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel2()
	_, err = client.AcquireCheckSlot(waitCtx2)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// A waiting acquire gets the slot released while it waits.
	got := make(chan error, 1)
	go func() {
		slot, err := client.AcquireCheckSlot(ctx)
		if err == nil {
			slot.Release()
		}
		got <- err
	}()
	second.Release()
	require.NoError(t, <-got)
	third.Release()
}

// TestAcquireCheckSlot_defaultConcurrency: without check_concurrency, the client holds
// DefaultGraftResolverCheckConcurrency slots.
func TestAcquireCheckSlot_defaultConcurrency(t *testing.T) {
	client := newClient(t, commandConfig([]string{"/bin/true"}))
	require.Equal(t, config.DefaultGraftResolverCheckConcurrency, cap(client.checkSlots))
}

// TestAcquireResolveSlot_boundsTheResolutionsAtOnce: at most resolve_concurrency slots are held at
// once; a wait for one counts against the caller's deadline and fails when it ends; a released slot
// frees its place once, and cannot resolve any more.
func TestAcquireResolveSlot_boundsTheResolutionsAtOnce(t *testing.T) {
	cfg := commandConfig([]string{"/bin/true"})
	cfg.ResolveConcurrency = 1
	client := newClient(t, cfg)
	ctx := testhelper.Context(t)

	first, err := client.AcquireResolveSlot(ctx)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = client.AcquireResolveSlot(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "waiting for a resolve slot of the resolver")

	first.Release()
	first.Release()
	_, err = first.Resolve(ctx, sampleRequest())
	require.ErrorContains(t, err, "resolve without a resolve slot")

	second, err := client.AcquireResolveSlot(ctx)
	require.NoError(t, err)
	waitCtx2, cancel2 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel2()
	_, err = client.AcquireResolveSlot(waitCtx2)
	require.ErrorIs(t, err, context.DeadlineExceeded, "a second release must not free a second place")
	second.Release()

	var none *ResolveSlot
	_, err = none.Resolve(ctx, sampleRequest())
	require.ErrorContains(t, err, "resolve without a resolve slot")
	none.Release()

	var disabled *Client
	_, err = disabled.AcquireResolveSlot(ctx)
	require.ErrorIs(t, err, ErrDisabled)
}

// timedOut reports whether acquiring a verify slot of repo gives up at once: the wait counts against
// the context, so a short one ends it.
func timedOut(t *testing.T, pool *VerifyPool, repo string) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(testhelper.Context(t), 50*time.Millisecond)
	defer cancel()
	slot, err := pool.Acquire(ctx, repo)
	if err == nil {
		slot.Release()
	}
	return err
}

// TestAcquireVerifySlot_boundsTheVerificationsAtOnce: at most verify_concurrency slots are held at once
// in the process, whatever the repositories; a wait counts against the deadline of the caller; a
// released slot frees its place once; the pool is apart from the checks' and the resolutions'.
func TestAcquireVerifySlot_boundsTheVerificationsAtOnce(t *testing.T) {
	cfg := commandConfig([]string{"/bin/true"})
	cfg.VerifyConcurrency = 2
	cfg.CheckConcurrency = 1
	cfg.ResolveConcurrency = 1
	client := newClient(t, cfg)
	pool := NewVerifyPool(cfg)
	ctx := testhelper.Context(t)

	a, err := pool.Acquire(ctx, "a")
	require.NoError(t, err)
	b, err := pool.Acquire(ctx, "b")
	require.NoError(t, err)

	// Two repositories hold the two slots: a third waits until its deadline, then fails.
	err = timedOut(t, pool, "c")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "waiting for a verify slot of the resolver")

	// A held verify slot delays neither a check nor a resolution.
	quick, cancelQuick := context.WithTimeout(ctx, time.Second)
	defer cancelQuick()
	checkSlot, err := client.AcquireCheckSlot(quick)
	require.NoError(t, err)
	resolveSlot, err := client.AcquireResolveSlot(quick)
	require.NoError(t, err)
	checkSlot.Release()
	resolveSlot.Release()

	// A slot released twice frees one place only.
	a.Release()
	a.Release()
	require.NoError(t, timedOut(t, pool, "c"))
	c, err := pool.Acquire(ctx, "c")
	require.NoError(t, err)
	require.ErrorIs(t, timedOut(t, pool, "d"), context.DeadlineExceeded, "a second release must not free a second place")
	b.Release()
	c.Release()

	var none *VerifySlot
	none.Release()
	var disabled *VerifyPool
	_, err = disabled.Acquire(ctx, "a")
	require.ErrorIs(t, err, ErrDisabled)

	require.Equal(t, config.DefaultGraftResolverVerifyConcurrency, cap(NewVerifyPool(commandConfig([]string{"/bin/true"})).slots))
}

// TestAcquireVerifySlot_oneRepositoryCannotTakeThePool: at most verify_per_repository verifications run
// at once for one repository (one by default), so that a busy repository, whose every land builds a tree
// of its own to verify, leaves slots to the others. A land that waits for its repository holds nothing of
// the pool, and nothing is left of a repository when its last holder and waiter are gone.
func TestAcquireVerifySlot_oneRepositoryCannotTakeThePool(t *testing.T) {
	cfg := commandConfig([]string{"/bin/true"})
	cfg.VerifyConcurrency = 2
	pool := NewVerifyPool(cfg)
	ctx := testhelper.Context(t)
	require.Equal(t, config.DefaultGraftResolverVerifyPerRepository, pool.perRepo)

	first, err := pool.Acquire(ctx, "busy")
	require.NoError(t, err)

	// A second verification of the same repository waits, though the pool has a free slot...
	err = timedOut(t, pool, "busy")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "waiting for a verify slot of the repository")

	// ...and takes nothing of the pool while it waits: another repository gets the free slot, even
	// while a land waits for the busy one.
	waiting := make(chan error, 1)
	go func() {
		slot, err := pool.Acquire(ctx, "busy")
		if err == nil {
			slot.Release()
		}
		waiting <- err
	}()
	require.Eventually(t, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return pool.repos["busy"] != nil && pool.repos["busy"].users == 2
	}, 5*time.Second, 5*time.Millisecond, "the second land never waited")
	other, err := pool.Acquire(ctx, "other")
	require.NoError(t, err, "a land that waits for its repository holds nothing of the pool")

	// The release of the first lets the waiter through.
	first.Release()
	require.NoError(t, <-waiting)
	other.Release()

	pool.mu.Lock()
	require.Empty(t, pool.repos, "nothing is kept of a repository without holder or waiter")
	pool.mu.Unlock()

	// A waiter that gave up leaves no trace either.
	held, err := pool.Acquire(ctx, "gone")
	require.NoError(t, err)
	require.Error(t, timedOut(t, pool, "gone"))
	held.Release()
	pool.mu.Lock()
	require.Empty(t, pool.repos)
	pool.mu.Unlock()

	// Configured, the cap is the number of verifications of one repository at once.
	cfg.VerifyPerRepository = 2
	cfg.VerifyConcurrency = 3
	wide := NewVerifyPool(cfg)
	one, err := wide.Acquire(ctx, "r")
	require.NoError(t, err)
	two, err := wide.Acquire(ctx, "r")
	require.NoError(t, err)
	require.ErrorContains(t, timedOut(t, wide, "r"), "of the repository")
	one.Release()
	two.Release()
}

// TestResolveSlots_apartFromTheCheckSlots: the resolutions and the checks have pools of their own
// (PLAN10 15.8): every resolve slot held does not delay a check, and every check slot held does not
// delay a resolution.
func TestResolveSlots_apartFromTheCheckSlots(t *testing.T) {
	cfg := commandConfig([]string{"/bin/true"})
	cfg.CheckConcurrency = 1
	cfg.ResolveConcurrency = 1
	client := newClient(t, cfg)
	ctx := testhelper.Context(t)

	resolveSlot, err := client.AcquireResolveSlot(ctx)
	require.NoError(t, err)
	quick, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	checkSlot, err := client.AcquireCheckSlot(quick)
	require.NoError(t, err, "a held resolve slot delays no check")

	resolveSlot.Release()
	quick2, cancel2 := context.WithTimeout(ctx, time.Second)
	defer cancel2()
	again, err := client.AcquireResolveSlot(quick2)
	require.NoError(t, err, "a held check slot delays no resolution")
	again.Release()
	checkSlot.Release()
}

// TestAcquireResolveSlot_defaultConcurrency: without resolve_concurrency, the client holds
// DefaultGraftResolverResolveConcurrency slots.
func TestAcquireResolveSlot_defaultConcurrency(t *testing.T) {
	client := newClient(t, commandConfig([]string{"/bin/true"}))
	require.Equal(t, config.DefaultGraftResolverResolveConcurrency, cap(client.resolveSlots))
}

// TestNew_refusesNegativePools: a negative resolve_concurrency or resolve_rounds is refused, as a
// negative check_concurrency is.
func TestNew_refusesNegativePools(t *testing.T) {
	for name, set := range map[string]func(*config.GraftResolver){
		"resolve_concurrency":   func(c *config.GraftResolver) { c.ResolveConcurrency = -1 },
		"verify_concurrency":    func(c *config.GraftResolver) { c.VerifyConcurrency = -1 },
		"verify_per_repository": func(c *config.GraftResolver) { c.VerifyPerRepository = -1 },
		"resolve_rounds":        func(c *config.GraftResolver) { c.ResolveRounds = -1 },
	} {
		cfg := commandConfig([]string{"/bin/true"})
		set(&cfg)
		_, err := New(cfg)
		require.ErrorContains(t, err, "must not be negative", name)
	}
}

// TestCheck_namesTheDeadlineThatEnded: a check request that runs out of its own timeout says so; one
// that runs out of the deadline of the whole check names that deadline instead.
func TestCheck_namesTheDeadlineThatEnded(t *testing.T) {
	requireUnix(t)

	cfg := commandConfig([]string{writeScript(t, "cat > /dev/null\nsleep 5\n")})
	req := sampleRequest()
	req.Resolution = validResponse("merged\n").Files

	cfg.Timeout = "100ms"
	_, err := checkWithSlot(t, newClient(t, cfg), req)
	require.ErrorContains(t, err, "resolver check request timed out after 100ms")

	cfg.Timeout = "1m"
	client := newClient(t, cfg)
	slot, err := client.AcquireCheckSlot(testhelper.Context(t))
	require.NoError(t, err)
	defer slot.Release()
	ctx, cancel := context.WithTimeout(testhelper.Context(t), 100*time.Millisecond)
	defer cancel()
	_, err = slot.Check(ctx, req)
	require.ErrorContains(t, err, "resolver check stopped at the deadline of the check")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestNewShared: the resolver of the process carries the client, or why it cannot be built.
func TestNewShared(t *testing.T) {
	shared := NewShared(commandConfig([]string{"/bin/true"}))
	require.NoError(t, shared.Err)
	require.NotNil(t, shared.Client)

	shared = NewShared(config.GraftResolver{Enabled: true, URL: "https://resolver.example/", TokenFile: filepath.Join(t.TempDir(), "gone")})
	require.ErrorContains(t, shared.Err, "read token file")
	require.Nil(t, shared.Client)

	shared = NewShared(config.GraftResolver{})
	require.NoError(t, shared.Err)
	require.Nil(t, shared.Client, "resolution disabled")
}
