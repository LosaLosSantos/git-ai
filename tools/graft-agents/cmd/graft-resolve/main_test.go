package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

func fileConflict(path, base, ours, theirs string) protocol.FileConflict {
	side := func(s string) *protocol.Side {
		return &protocol.Side{OID: "oid", Mode: protocol.ModeFile, Content: []byte(s)}
	}
	return protocol.FileConflict{
		Path:   path,
		Kind:   protocol.KindContent,
		Base:   side(base),
		Ours:   side(ours),
		Theirs: side(theirs),
	}
}

func requestJSON(t *testing.T, files ...protocol.FileConflict) []byte {
	t.Helper()
	req := protocol.Request{
		Version:    protocol.Version,
		Repository: protocol.Repository{Storage: "default", RelativePath: "x.git"},
		Branch:     "main",
		Actor:      "agent-7",
		Files:      files,
		Limits:     protocol.Limits{MaxFileBytes: 1 << 20},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolve.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommandModeResolvesTrivialConflict(t *testing.T) {
	stdin := bytes.NewReader(requestJSON(t,
		fileConflict("same.go", "base\n", "both\n", "both\n"),
		fileConflict("unchanged.go", "base\n", "base\n", "base\n"),
	))
	var stdout, stderr bytes.Buffer
	if code := run(nil, stdin, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	var resp protocol.Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, stdout.String())
	}
	if resp.Version != 1 || resp.Resolver != "graft-resolve/trivial" {
		t.Fatalf("response header = %+v", resp)
	}
	if len(resp.Files) != 2 || string(resp.Files[0].Content) != "both\n" ||
		resp.Files[1].Action != protocol.ActionWrite || string(resp.Files[1].Content) != "base\n" {
		t.Fatalf("files = %+v", resp.Files)
	}
	if !strings.HasSuffix(stdout.String(), "\n") {
		t.Fatal("response must end with a newline")
	}
}

func TestCommandModeUnresolvableExitsOne(t *testing.T) {
	stdin := bytes.NewReader(requestJSON(t, fileConflict("hard.go", "base\n", "ours\n", "theirs\n")))
	var stdout, stderr bytes.Buffer
	if code := run(nil, stdin, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	// The refusal is on stdout as an ErrorResponse with the outcome of every file (the server reads
	// it), and its reason on stderr (for a server that predates the outcomes).
	var refusal protocol.ErrorResponse
	if err := json.Unmarshal(stdout.Bytes(), &refusal); err != nil {
		t.Fatalf("stdout is not an ErrorResponse: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(refusal.Error, "hard.go") || len(refusal.Files) != 1 ||
		refusal.Files[0] != (protocol.FileOutcome{Path: "hard.go", Outcome: protocol.OutcomeFailed, Reason: "no strategy resolved this conflict"}) {
		t.Fatalf("refusal = %+v", refusal)
	}
	if !strings.Contains(stderr.String(), "hard.go") || !strings.Contains(stderr.String(), "unresolved") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCommandModeCheck(t *testing.T) {
	req := protocol.Request{
		Version: protocol.Version, Operation: protocol.OperationCheck,
		Files: []protocol.FileConflict{fileConflict("a.txt", "base\n", "ours\n", "theirs\n")},
		Resolution: []protocol.ResolvedFile{
			{Path: "a.txt", Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: []byte("merged\n")},
		},
		Limits: protocol.Limits{MaxFileBytes: 1 << 20},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(nil, bytes.NewReader(raw), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var resp protocol.CheckResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("stdout is not a CheckResponse: %v\n%s", err, stdout.String())
	}
	if resp.Operation != protocol.OperationCheck || resp.Resolver != "graft-resolve" || len(resp.Files) != 1 ||
		resp.Files[0] != (protocol.CheckFile{Path: "a.txt", OK: true, Syntax: protocol.SyntaxNone}) {
		t.Fatalf("check response %+v", resp)
	}
}

func TestCommandModeInvalidJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader("{not json"), &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "invalid request JSON") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCommandModeInvalidRequest(t *testing.T) {
	body := []byte(`{"version":1,"files":[{"path":"../escape","kind":"content","base":null,"ours":null,"theirs":null}]}`)
	var stdout, stderr bytes.Buffer
	if code := run(nil, bytes.NewReader(body), &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unsafe path") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCommandModeWithConfigFile(t *testing.T) {
	cfg := writeConfig(t, `
[[strategy]]
type = "union"
paths = ["CHANGELOG.md"]
`)
	stdin := bytes.NewReader(requestJSON(t, fileConflict("CHANGELOG.md", "## 1\n", "## 1\n- ours\n", "## 1\n- theirs\n")))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", cfg}, stdin, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	var resp protocol.Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Resolver != "graft-resolve/union" || string(resp.Files[0].Content) != "## 1\n- ours\n- theirs\n" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestCommandModeOutOfScopeFileFailsWithoutConfigStrategy(t *testing.T) {
	cfg := writeConfig(t, "[[strategy]]\ntype = \"union\"\npaths = [\"CHANGELOG.md\"]\n")
	stdin := bytes.NewReader(requestJSON(t, fileConflict("main.go", "a\n", "b\n", "c\n")))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", cfg}, stdin, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

func TestCommandModeBadConfig(t *testing.T) {
	cfg := writeConfig(t, "[[strategy]]\ntype = \"nonexistent\"\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", cfg}, strings.NewReader("{}"), &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown type") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCommandModeMissingConfigFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", filepath.Join(t.TempDir(), "absent.toml")}, strings.NewReader("{}"), &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

func TestCommandModeArgumentErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--no-such-flag"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("unknown flag: exit %d, want 2", code)
	}
	if code := run([]string{"extra"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("positional argument: exit %d, want 2", code)
	}
}

func TestServeRequiresListen(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"serve"}, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--listen is required") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestServeFailsOnMissingTokenFile(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"serve", "--listen", "127.0.0.1:0", "--token-file", filepath.Join(t.TempDir(), "nope")}, strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "token file") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("  s3cret \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if tok, err := readToken(""); err != nil || tok != "" {
		t.Fatalf("no file: tok=%q err=%v", tok, err)
	}
	if tok, err := readToken(good); err != nil || tok != "s3cret" {
		t.Fatalf("good: tok=%q err=%v", tok, err)
	}
	if _, err := readToken(empty); err == nil {
		t.Fatal("empty token file accepted")
	}
	if _, err := readToken(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing token file accepted")
	}
}

func TestLoadResolverDefault(t *testing.T) {
	r, err := loadResolver("")
	if err != nil {
		t.Fatal(err)
	}
	if r.Timeout <= 0 {
		t.Fatalf("timeout = %s", r.Timeout)
	}
}
