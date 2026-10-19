package resolve

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

// helperArg marks a re-execution of the test binary as a fake external tool.
// The tests never run a real merge tool, a shell or the network.
const helperArg = "__graft_resolve_test_helper__"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == helperArg {
		os.Exit(runHelper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// runHelper implements the fake tools used by the tests.
func runHelper(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "helper: no mode")
		return 2
	}
	mode, rest := args[0], args[1:]
	switch mode {
	case "copy": // copy IN OUT
		data, err := os.ReadFile(rest[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return writeOrFail(rest[1], data)
	case "concat": // concat IN1 IN2 OUT
		a, errA := os.ReadFile(rest[0])
		b, errB := os.ReadFile(rest[1])
		if errA != nil || errB != nil {
			fmt.Fprintln(os.Stderr, "cannot read inputs")
			return 1
		}
		return writeOrFail(rest[2], append(append([]byte{}, a...), b...))
	case "fail":
		fmt.Fprintln(os.Stderr, "boom: cannot merge")
		return 3
	case "always-fail":
		fmt.Fprintln(os.Stderr, "nope")
		return 1
	case "sleep":
		time.Sleep(time.Minute)
		return 0
	case "markers": // writes conflict markers to the last argument
		return writeOrFail(rest[len(rest)-1], []byte("<<<<<<< ours\nx\n=======\ny\n>>>>>>> theirs\n"))
	case "nothing":
		return 0
	case "spam-stderr":
		fmt.Fprint(os.Stderr, strings.Repeat("e", 100_000))
		return 1
	case "envcheck": // exits 1 when the test secret variable reaches the tool
		if os.Getenv(testSecretEnv) != "" {
			fmt.Fprintln(os.Stderr, "secret visible")
			return 1
		}
		return 0
	case "expect-base": // exits 1 unless filepath.Base(rest[0]) == rest[1]
		if filepath.Base(rest[0]) != rest[1] {
			fmt.Fprintf(os.Stderr, "base name %q, want %q\n", filepath.Base(rest[0]), rest[1])
			return 1
		}
		return 0
	case "stdout-concat": // writes IN1 followed by IN2 to stdout
		a, errA := os.ReadFile(rest[0])
		b, errB := os.ReadFile(rest[1])
		if errA != nil || errB != nil {
			fmt.Fprintln(os.Stderr, "cannot read inputs")
			return 1
		}
		_, _ = os.Stdout.Write(append(append([]byte{}, a...), b...))
		return 0
	case "stdout-big": // writes more than the command output limit to stdout
		_, _ = os.Stdout.Write(make([]byte, maxCommandOutput+1))
		return 0
	case "mkdir": // creates the directory rest[0]
		if err := os.Mkdir(rest[0], 0o700); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "pass":
		return 0
	case "contains-bad": // exits 1 when the file named by rest[0] contains BAD
		data, err := os.ReadFile(rest[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if strings.Contains(string(data), "BAD") {
			fmt.Fprintln(os.Stderr, "found BAD")
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "helper: unknown mode %q\n", mode)
	return 2
}

func writeOrFail(path string, data []byte) int {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// helperArgv returns argv that runs this test binary as the fake tool.
func helperArgv(args ...string) []string {
	return append([]string{os.Args[0], helperArg}, args...)
}

func commandStrategy(t *testing.T, timeout string, argv ...string) Strategy {
	t.Helper()
	opts := map[string]any{"command": toAny(argv)}
	if timeout != "" {
		opts["timeout"] = timeout
	}
	s, err := newCommand(StrategySpec{Name: "cmd", Type: "command", Options: opts})
	if err != nil {
		t.Fatalf("newCommand: %v", err)
	}
	return s
}

func toAny(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

func TestCommandStrategyAnswersFromOut(t *testing.T) {
	s := commandStrategy(t, "", helperArgv("concat", "{ours}", "{theirs}", "{out}")...)
	f := File{
		Base:   sideOf(protocol.ModeFile, "base\n"),
		Ours:   sideOf(protocol.ModeExecutable, "ours\n"),
		Theirs: sideOf(protocol.ModeExecutable, "theirs\n"),
	}
	res, ok, err := s.Resolve(context.Background(), f, protocol.Request{})
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if res.Action != protocol.ActionWrite || res.Mode != protocol.ModeExecutable || string(res.Content) != "ours\ntheirs\n" {
		t.Fatalf("answer = %+v", res)
	}
}

func TestCommandStrategyAbsentBaseIsEmptyFile(t *testing.T) {
	s := commandStrategy(t, "", helperArgv("copy", "{base}", "{out}")...)
	f := File{Ours: sideOf(protocol.ModeFile, "a\n"), Theirs: sideOf(protocol.ModeFile, "b\n")}
	res, ok, err := s.Resolve(context.Background(), f, protocol.Request{})
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if len(res.Content) != 0 {
		t.Fatalf("absent base must be an empty file, got %q", res.Content)
	}
}

func TestCommandStrategyDeclines(t *testing.T) {
	s := commandStrategy(t, "", helperArgv("pass")...)
	cases := map[string]File{
		"ours absent":      {Base: sideOf(protocol.ModeFile, "x"), Theirs: sideOf(protocol.ModeFile, "y")},
		"theirs absent":    {Base: sideOf(protocol.ModeFile, "x"), Ours: sideOf(protocol.ModeFile, "y")},
		"symlink":          {Base: sideOf(protocol.ModeSymlink, "x"), Ours: sideOf(protocol.ModeFile, "y"), Theirs: sideOf(protocol.ModeFile, "z")},
		"mode mismatch":    {Ours: sideOf(protocol.ModeFile, "y"), Theirs: sideOf(protocol.ModeExecutable, "z")},
		"symlink operands": {Ours: sideOf(protocol.ModeSymlink, "y"), Theirs: sideOf(protocol.ModeSymlink, "z")},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			_, ok, err := s.Resolve(context.Background(), f, protocol.Request{})
			if err != nil || ok {
				t.Fatalf("ok=%v err=%v, want decline", ok, err)
			}
		})
	}
}

func TestCommandStrategyToolFailureIsError(t *testing.T) {
	s := commandStrategy(t, "", helperArgv("fail")...)
	f := File{Ours: sideOf(protocol.ModeFile, "a"), Theirs: sideOf(protocol.ModeFile, "b")}
	_, ok, err := s.Resolve(context.Background(), f, protocol.Request{})
	if err == nil || ok {
		t.Fatalf("ok=%v err=%v, want error", ok, err)
	}
	if !strings.Contains(err.Error(), "boom: cannot merge") {
		t.Fatalf("stderr not in error: %v", err)
	}
}

func TestCommandStrategyTimeout(t *testing.T) {
	s := commandStrategy(t, "200ms", helperArgv("sleep")...)
	f := File{Ours: sideOf(protocol.ModeFile, "a"), Theirs: sideOf(protocol.ModeFile, "b")}
	start := time.Now()
	_, _, err := s.Resolve(context.Background(), f, protocol.Request{})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("timeout not enforced, took %s", elapsed)
	}
}

func TestCommandStrategyMissingProgram(t *testing.T) {
	s := commandStrategy(t, "", filepath.Join(t.TempDir(), "no-such-tool"))
	f := File{Ours: sideOf(protocol.ModeFile, "a"), Theirs: sideOf(protocol.ModeFile, "b")}
	if _, _, err := s.Resolve(context.Background(), f, protocol.Request{}); err == nil {
		t.Fatal("missing program accepted")
	}
}

func TestCommandStrategyRejectsBadConfig(t *testing.T) {
	cases := map[string]StrategySpec{
		"no command":       {Name: "c", Type: "command"},
		"empty argv":       {Name: "c", Type: "command", Options: map[string]any{"command": []any{""}}},
		"bad timeout":      {Name: "c", Type: "command", Options: map[string]any{"command": []any{"x"}, "timeout": "-1s"}},
		"unknown option":   {Name: "c", Type: "command", Options: map[string]any{"command": []any{"x"}, "shell": true}},
		"command not list": {Name: "c", Type: "command", Options: map[string]any{"command": "x"}},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := newCommand(spec); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestRunCommandBoundsStderr(t *testing.T) {
	out, err := runCommand(context.Background(), t.TempDir(), helperArgv("spam-stderr"), 10*time.Second)
	if err == nil {
		t.Fatal("expected failure")
	}
	if len(out) > maxCapturedStderr {
		t.Fatalf("stderr not bounded: %d bytes", len(out))
	}
}

func TestRunCommandEmptyArgv(t *testing.T) {
	if _, err := runCommand(context.Background(), t.TempDir(), nil, time.Second); err == nil {
		t.Fatal("empty argv accepted")
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{limit: 5}
	n, err := io.WriteString(b, "abc")
	if err != nil || n != 3 {
		t.Fatalf("write = %d %v", n, err)
	}
	n, err = io.WriteString(b, "defghij")
	if err != nil || n != 7 {
		t.Fatalf("write past limit = %d %v, want full length", n, err)
	}
	if b.String() != "abcde" {
		t.Fatalf("buffer = %q", b.String())
	}
}

func TestChecksRejectAndChainContinues(t *testing.T) {
	checks := []Check{{Paths: []string{"*.txt"}, Command: helperArgv("contains-bad", "{file}")}}
	r := newTestResolver(t, checks,
		Binding{Name: "sloppy", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "BAD\n")}},
		Binding{Name: "careful", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "good\n")}},
	)
	resp, err := r.Resolve(context.Background(), request(conflict("notes.txt")))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp.Resolver != "graft-resolve/careful" || string(resp.Files[0].Content) != "good\n" {
		t.Fatalf("failing check did not reject the answer: %+v", resp)
	}
}

func TestChecksReasonRecorded(t *testing.T) {
	checks := []Check{{Paths: []string{"*.txt"}, Command: helperArgv("contains-bad", "{file}")}}
	r := newTestResolver(t, checks,
		Binding{Name: "sloppy", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "BAD\n")}},
	)
	_, err := r.Resolve(context.Background(), request(conflict("notes.txt")))
	if err == nil || !strings.Contains(err.Error(), "check failed") || !strings.Contains(err.Error(), "found BAD") {
		t.Fatalf("error = %v, want the check failure and its stderr", err)
	}
}

func TestChecksOnlyMatchingPaths(t *testing.T) {
	checks := []Check{{Paths: []string{"*.txt"}, Command: helperArgv("contains-bad", "{file}")}}
	r := newTestResolver(t, checks,
		Binding{Name: "s", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "BAD\n")}},
	)
	if _, err := r.Resolve(context.Background(), request(conflict("code.go"))); err != nil {
		t.Fatalf("check applied to a non-matching path: %v", err)
	}
}

func TestChecksAllMustPass(t *testing.T) {
	checks := []Check{
		{Paths: []string{"**"}, Command: helperArgv("pass")},
		{Paths: []string{"**"}, Command: helperArgv("always-fail")},
	}
	r := newTestResolver(t, checks, Binding{Name: "s", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "x")}})
	if _, err := r.Resolve(context.Background(), request(conflict("a.go"))); err == nil {
		t.Fatal("second failing check ignored")
	}
}

func TestChecksNotRunForDelete(t *testing.T) {
	checks := []Check{{Paths: []string{"**"}, Command: helperArgv("always-fail")}}
	r := newTestResolver(t, checks, Binding{Name: "del", Strategy: &fakeStrategy{fn: func(File) (Resolution, bool, error) {
		return Resolution{Action: protocol.ActionDelete}, true, nil
	}}})
	resp, err := r.Resolve(context.Background(), request(conflict("gone.go")))
	if err != nil || resp.Files[0].Action != protocol.ActionDelete {
		t.Fatalf("delete rejected by a check: %v", err)
	}
}

func TestCheckTimeoutRejects(t *testing.T) {
	checks := []Check{{Paths: []string{"**"}, Command: helperArgv("sleep"), Timeout: 200 * time.Millisecond}}
	r := newTestResolver(t, checks, Binding{Name: "s", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "x")}})
	_, err := r.Resolve(context.Background(), request(conflict("a.go")))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout", err)
	}
}

func TestCheckFileIsNamedAfterPath(t *testing.T) {
	// The check sees the answer as check plus the path's extension (checkFileName),
	// so tools that key on
	// the extension behave as they would on the real file.
	checks := []Check{{Paths: []string{"**"}, Command: helperArgv("contains-bad", "{file}")}}
	r := newTestResolver(t, checks, Binding{Name: "s", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "fine\n")}})
	if _, err := r.Resolve(context.Background(), request(conflict("deep/dir/name.go"))); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestCommandInChainWithRerereRecording(t *testing.T) {
	// The command strategy answers, the check accepts, and rerere records the
	// accepted answer. The next identical conflict is answered from rerere.
	dir := t.TempDir()
	rr := newTestRerere(t, dir)
	cmd := commandStrategy(t, "", helperArgv("concat", "{ours}", "{theirs}", "{out}")...)
	r := newTestResolver(t, []Check{{Paths: []string{"**"}, Command: helperArgv("pass")}},
		Binding{Name: "rr", Strategy: rr},
		Binding{Name: "cmd", Strategy: cmd},
	)
	first, err := r.Resolve(context.Background(), request(conflict("a.go")))
	if err != nil {
		t.Fatal(err)
	}
	if first.Resolver != "graft-resolve/cmd" {
		t.Fatalf("first resolver = %q", first.Resolver)
	}

	second, err := r.Resolve(context.Background(), request(conflict("b.go")))
	if err != nil {
		t.Fatal(err)
	}
	if second.Resolver != "graft-resolve/rr" || string(second.Files[0].Content) != string(first.Files[0].Content) {
		t.Fatalf("rerere did not reuse the recorded answer: %+v", second)
	}
}

// TestCommandStrategyOrigin: a command is a rule unless it is configured as a model (origin = "model"),
// for the command that calls a model; any other origin is refused.
func TestCommandStrategyOrigin(t *testing.T) {
	argv := toAny(helperArgv("copy", "{ours}", "{out}"))
	for origin, want := range map[string]string{"": protocol.OriginRule, "rule": protocol.OriginRule, "model": protocol.OriginModel} {
		opts := map[string]any{"command": argv}
		if origin != "" {
			opts["origin"] = origin
		}
		s, err := newCommand(StrategySpec{Name: "cmd", Type: "command", Options: opts})
		if err != nil {
			t.Fatalf("origin %q: %v", origin, err)
		}
		if got := originOf(s); got != want {
			t.Errorf("origin %q: %q, want %q", origin, got, want)
		}
	}
	if _, err := newCommand(StrategySpec{Name: "cmd", Type: "command", Options: map[string]any{"command": argv, "origin": "ai"}}); err == nil ||
		!strings.Contains(err.Error(), "origin must be") {
		t.Fatalf("origin ai: %v", err)
	}

	// Through a resolver: a command configured as a model gives a proposal, which is not recorded.
	s, err := newCommand(StrategySpec{Name: "cmd", Type: "command", Options: map[string]any{"command": argv, "origin": "model"}})
	if err != nil {
		t.Fatal(err)
	}
	r := newTestResolver(t, nil, Binding{Name: "cmd", Strategy: s})
	resp, err := r.Resolve(context.Background(), request(conflict("a.go")))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 1 || resp.Files[0].Origin != protocol.OriginModel || !resp.Files[0].Proposal {
		t.Fatalf("files %+v", resp.Files)
	}
}
