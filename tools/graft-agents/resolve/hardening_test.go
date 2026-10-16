package resolve

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

// testSecretEnv stands for a provider key in graft-resolve's environment.
const testSecretEnv = "GRAFT_RESOLVE_TEST_SECRET"

// commandSpec builds a command strategy with extra options.
func commandSpec(t *testing.T, opts map[string]any, argv ...string) Strategy {
	t.Helper()
	all := map[string]any{"command": toAny(argv)}
	for k, v := range opts {
		all[k] = v
	}
	s, err := newCommand(StrategySpec{Name: "cmd", Type: "command", Options: all})
	if err != nil {
		t.Fatalf("newCommand: %v", err)
	}
	return s
}

func TestCommandFileModeMissingOutputIsAnError(t *testing.T) {
	// The tool exits 0 without writing {out}. This used to land an empty file.
	s := commandSpec(t, nil, helperArgv("nothing")...)
	f := File{Ours: sideOf(protocol.ModeFile, "a\n"), Theirs: sideOf(protocol.ModeFile, "b\n")}
	_, ok, err := s.Resolve(context.Background(), f, protocol.Request{})
	if err == nil || ok {
		t.Fatalf("ok=%v err=%v, want an error for a missing {out}", ok, err)
	}
	if !strings.Contains(err.Error(), "without writing {out}") {
		t.Fatalf("error = %v", err)
	}
}

func TestCommandFileModeOutputMustBeRegularFile(t *testing.T) {
	s := commandSpec(t, nil, helperArgv("mkdir", "{out}")...)
	f := File{Ours: sideOf(protocol.ModeFile, "a\n"), Theirs: sideOf(protocol.ModeFile, "b\n")}
	_, ok, err := s.Resolve(context.Background(), f, protocol.Request{})
	if err == nil || ok || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("ok=%v err=%v, want a non-regular {out} rejected", ok, err)
	}
}

func TestCommandFileModeEmptyOutputIsExplicit(t *testing.T) {
	// An empty {out} written on purpose is still an answer.
	s := commandSpec(t, nil, helperArgv("copy", "{base}", "{out}")...)
	f := File{Base: sideOf(protocol.ModeFile, ""), Ours: sideOf(protocol.ModeFile, "a\n"), Theirs: sideOf(protocol.ModeFile, "b\n")}
	res, ok, err := s.Resolve(context.Background(), f, protocol.Request{})
	if err != nil || !ok || len(res.Content) != 0 {
		t.Fatalf("ok=%v err=%v content=%q", ok, err, res.Content)
	}
}

func TestCommandStdoutMode(t *testing.T) {
	s := commandSpec(t, map[string]any{"output": "stdout"}, helperArgv("stdout-concat", "{ours}", "{theirs}")...)
	f := File{Ours: sideOf(protocol.ModeExecutable, "ours\n"), Theirs: sideOf(protocol.ModeExecutable, "theirs\n")}
	res, ok, err := s.Resolve(context.Background(), f, protocol.Request{})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if string(res.Content) != "ours\ntheirs\n" || res.Mode != protocol.ModeExecutable {
		t.Fatalf("answer = mode %q content %q", res.Mode, res.Content)
	}
}

func TestCommandStdoutModeRejectsEmptyAndOversized(t *testing.T) {
	f := File{Ours: sideOf(protocol.ModeFile, "a\n"), Theirs: sideOf(protocol.ModeFile, "b\n")}
	empty := commandSpec(t, map[string]any{"output": "stdout"}, helperArgv("pass")...)
	if _, ok, err := empty.Resolve(context.Background(), f, protocol.Request{}); err == nil || ok ||
		!strings.Contains(err.Error(), "no output") {
		t.Fatalf("empty stdout: ok=%v err=%v", ok, err)
	}
	big := commandSpec(t, map[string]any{"output": "stdout"}, helperArgv("stdout-big")...)
	if _, ok, err := big.Resolve(context.Background(), f, protocol.Request{}); err == nil || ok ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized stdout: ok=%v err=%v", ok, err)
	}
}

func TestCommandStdoutModeMatchesDocumentedGitExample(t *testing.T) {
	// The example of config.go: git merge-file -p writes the result to stdout.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	s := commandSpec(t, map[string]any{"output": "stdout"},
		"git", "merge-file", "-p", "{ours}", "{base}", "{theirs}")
	base := sideOf(protocol.ModeFile, "1\n2\n3\n4\n5\n6\n")

	clean := File{
		Base:   base,
		Ours:   sideOf(protocol.ModeFile, "one\n2\n3\n4\n5\n6\n"),
		Theirs: sideOf(protocol.ModeFile, "1\n2\n3\n4\n5\nsix\n"),
	}
	res, ok, err := s.Resolve(context.Background(), clean, protocol.Request{})
	if err != nil || !ok {
		t.Fatalf("clean merge: ok=%v err=%v", ok, err)
	}
	if want := "one\n2\n3\n4\n5\nsix\n"; string(res.Content) != want {
		t.Fatalf("clean merge content = %q, want %q", res.Content, want)
	}

	conflicting := File{
		Base:   base,
		Ours:   sideOf(protocol.ModeFile, "ONE\n2\n3\n4\n5\n6\n"),
		Theirs: sideOf(protocol.ModeFile, "uno\n2\n3\n4\n5\n6\n"),
	}
	if _, ok, err := s.Resolve(context.Background(), conflicting, protocol.Request{}); err == nil || ok {
		t.Fatalf("conflicting merge: ok=%v err=%v, want an error", ok, err)
	}
}

func TestCommandConfigOptionsAreValidated(t *testing.T) {
	cases := map[string]map[string]any{
		"unknown output":    {"command": []any{"x"}, "output": "sideways"},
		"output not string": {"command": []any{"x"}, "output": 1},
		"env bad name":      {"command": []any{"x"}, "env": []any{"A=B"}},
		"env not list":      {"command": []any{"x"}, "env": "LANG"},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := newCommand(StrategySpec{Name: "c", Type: "command", Options: opts}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestCheckFileNameIsSafe(t *testing.T) {
	cases := map[string]string{
		"dir/a;exit 7.txt":               "check.txt",
		"deep/dir/name.go":               "check.go",
		"Makefile":                       "check",
		"noext/a b":                      "check",
		"x/y.tar.gz":                     "check.gz",
		"x/weird.$(id)":                  "check",
		"x/.gitignore":                   "check.gitignore",
		"dir/a;echo INJECTED":            "check",
		"x/y.tiny-extension":             "check",
		"x/y.averyveryverylongextension": "check",
	}
	for in, want := range cases {
		if got := checkFileName(in); got != want {
			t.Fatalf("checkFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckCommandWithShellStringIsNotInjectable(t *testing.T) {
	// A check that interpolates {file} into a shell string must not run the
	// shell syntax of the repository path.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not installed")
	}
	checks := []Check{{Paths: []string{"**"}, Command: []string{sh, "-c", "cat {file} >/dev/null"}}}
	r := newTestResolver(t, checks, Binding{Name: "s", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "x\n")}})
	for _, p := range []string{"dir/a;exit 7.txt", "dir/a;echo INJECTED >&2;exit 7"} {
		if _, err := r.Resolve(context.Background(), request(conflict(p))); err != nil {
			t.Fatalf("path %q: check failed: %v", p, err)
		}
	}
}

func TestCheckSeesExtensionOfPath(t *testing.T) {
	checks := []Check{{Paths: []string{"**"}, Command: helperArgv("expect-base", "{file}", "check.go")}}
	r := newTestResolver(t, checks, Binding{Name: "s", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "fine\n")}})
	if _, err := r.Resolve(context.Background(), request(conflict("deep/dir/name.go"))); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestSubprocessEnvironmentIsMinimal(t *testing.T) {
	t.Setenv(testSecretEnv, "hunter2")
	ctx := context.Background()

	if _, err := runCommand(ctx, t.TempDir(), helperArgv("envcheck"), 10*time.Second); err != nil {
		t.Fatalf("secret reached a subprocess: %v", err)
	}
	c := Check{Paths: []string{"**"}, Command: helperArgv("envcheck")}
	if err := runCheck(ctx, c, "a.go", []byte("x")); err != nil {
		t.Fatalf("secret reached a check: %v", err)
	}

	// The env allowlist passes the variable through.
	c.Env = []string{testSecretEnv}
	if err := runCheck(ctx, c, "a.go", []byte("x")); err == nil {
		t.Fatal("allowlisted variable did not reach the check")
	}
	if _, _, err := execute(ctx, execSpec{dir: t.TempDir(), argv: helperArgv("envcheck"), timeout: 10 * time.Second, env: []string{testSecretEnv}}); err == nil {
		t.Fatal("allowlisted variable did not reach the command")
	}
}

func TestCheckEnvNamesAreValidated(t *testing.T) {
	_, err := NewResolver([]Binding{{Name: "t", Strategy: trivial{}}},
		[]Check{{Paths: []string{"**"}, Command: []string{"true"}, Env: []string{"A=B"}}}, time.Minute)
	if err == nil {
		t.Fatal("invalid env name accepted")
	}
}

func TestDefaultRequestBoundCoversServerDefaults(t *testing.T) {
	// The server's defaults: 20 files, 1 MiB per side, three sides, base64.
	need := int64(20) * 3 * (1 << 20) * 4 / 3
	if protocol.DefaultMaxRequestBytes < need {
		t.Fatalf("default request bound %d is below the server defaults (%d)", protocol.DefaultMaxRequestBytes, need)
	}
	r, err := NewResolver([]Binding{{Name: "t", Strategy: trivial{}}}, nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxRequestBytes != protocol.DefaultMaxRequestBytes {
		t.Fatalf("resolver bound = %d", r.MaxRequestBytes)
	}
}
