package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// helperEnv selects the behaviour of the helper process. The tests re-execute
// the test binary, so no shell and no external program are needed.
const helperEnv = "GRAFT_LLM_COMMAND_HELPER"

// TestHelperProcess is not a real test: it is the program the provider runs.
// It does nothing unless helperEnv is set.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	os.Exit(runHelper(mode, helperArgs()))
}

// helperArgs returns the arguments after the first "--" in os.Args.
func helperArgs() []string {
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

func runHelper(mode string, args []string) int {
	switch mode {
	case "echo":
		// Write the request received on stdin back to stdout.
		in, _ := io.ReadAll(os.Stdin)
		_, _ = os.Stdout.Write(in)
		return 0
	case "text":
		_, _ = os.Stdout.WriteString("  plain answer \n")
		return 0
	case "fenced":
		_, _ = os.Stdout.WriteString("Here you go:\n```json\n{\"ok\": true}\n```\n")
		return 0
	case "notjson":
		_, _ = os.Stdout.WriteString("I cannot do that as JSON")
		return 0
	case "fail":
		_, _ = os.Stderr.WriteString("model overloaded\n")
		return 3
	case "empty":
		return 0
	case "huge":
		chunk := strings.Repeat("x", 1<<20)
		for i := 0; i < 9; i++ {
			_, _ = os.Stdout.WriteString(chunk)
		}
		return 0
	case "sleep":
		time.Sleep(30 * time.Second)
		return 0
	case "env":
		// Write the GRAFT_LLM_ variables as a JSON object; absent ones are left out.
		env := map[string]string{}
		for _, k := range []string{EnvModel, EnvEffort, EnvMaxTokens} {
			if v, ok := os.LookupEnv(k); ok {
				env[k] = v
			}
		}
		b, _ := json.Marshal(env)
		_, _ = os.Stdout.Write(b)
		return 0
	case "args":
		b, _ := json.Marshal(args)
		_, _ = os.Stdout.Write(b)
		return 0
	}
	_, _ = fmt.Fprintf(os.Stderr, "unknown mode %q", mode)
	return 2
}

// helperConfig returns a config whose command re-executes this test binary in
// the given mode.
func helperConfig(t *testing.T, mode string, extra ...string) llm.Config {
	t.Helper()
	t.Setenv(helperEnv, mode)
	argv := []string{os.Args[0], "-test.run=^TestHelperProcess$", "--"}
	argv = append(argv, extra...)
	return llm.Config{Type: "command", Model: "helper-model", Command: argv}
}

func newProvider(t *testing.T, cfg llm.Config) *Provider {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestNewValidation(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]llm.Config{
		"no command":          {Type: "command"},
		"empty program":       {Type: "command", Command: []string{"  "}},
		"nul in argument":     {Type: "command", Command: []string{"prog", "a\x00b"}},
		"negative max tokens": {Type: "command", Command: []string{"prog"}, MaxTokens: -1},
		"bad timeout":         {Type: "command", Command: []string{"prog"}, Timeout: "soon"},
		"zero timeout":        {Type: "command", Command: []string{"prog"}, Timeout: "0s"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNewCopiesArgvAndNamesProvider(t *testing.T) {
	t.Parallel()

	argv := []string{"/opt/tools/llm-cli", "--json"}
	p, err := New(llm.Config{Type: "command", Model: "m", Command: argv})
	if err != nil {
		t.Fatal(err)
	}
	argv[0] = "/evil"
	if p.argv[0] != "/opt/tools/llm-cli" {
		t.Fatalf("argv aliased the caller's slice: %v", p.argv)
	}
	if got := p.Name(); got != "command:llm-cli" {
		t.Fatalf("Name() = %q", got)
	}
}

func TestRegisteredInRegistry(t *testing.T) {
	t.Parallel()

	p, err := llm.New(llm.Config{Type: "command", Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	if p.Name() != "command:true" {
		t.Fatalf("Name() = %q", p.Name())
	}
}

func TestCompleteSendsRequestPayload(t *testing.T) {
	p := newProvider(t, helperConfig(t, "echo"))

	resp, err := p.Complete(context.Background(), llm.Request{
		System: "be precise",
		Prompt: "merge these",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(resp.Text), &got); err != nil {
		t.Fatalf("stdin was not JSON: %v (%s)", err, resp.Text)
	}
	if got["system"] != "be precise" || got["prompt"] != "merge these" {
		t.Fatalf("payload = %v", got)
	}
	if v, ok := got["json_schema"]; !ok || v != nil {
		t.Fatalf("json_schema = %v (present %v), want null", v, ok)
	}
	if resp.JSON != nil {
		t.Fatalf("JSON set without a schema: %s", resp.JSON)
	}
	if resp.Model != "helper-model" {
		t.Fatalf("Model = %q", resp.Model)
	}
}

func TestCompleteSendsSchema(t *testing.T) {
	p := newProvider(t, helperConfig(t, "echo"))
	schema := map[string]any{"type": "object"}

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x", JSONSchema: schema})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		JSONSchema map[string]any `json:"json_schema"`
	}
	if err := json.Unmarshal([]byte(resp.Text), &got); err != nil {
		t.Fatal(err)
	}
	if got.JSONSchema["type"] != "object" {
		t.Fatalf("schema not forwarded: %v", got.JSONSchema)
	}
}

func TestCompleteText(t *testing.T) {
	p := newProvider(t, helperConfig(t, "text"))

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "plain answer" {
		t.Fatalf("Text = %q, want trimmed answer", resp.Text)
	}
	if resp.JSON != nil {
		t.Fatalf("unexpected JSON %s", resp.JSON)
	}
}

func TestCompleteExtractsJSONFromSchemaAnswer(t *testing.T) {
	p := newProvider(t, helperConfig(t, "fenced"))

	resp, err := p.Complete(context.Background(), llm.Request{
		Prompt:     "x",
		JSONSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.JSON) != `{"ok": true}` {
		t.Fatalf("JSON = %s", resp.JSON)
	}
}

func TestCompleteSchemaAnswerNotJSON(t *testing.T) {
	p := newProvider(t, helperConfig(t, "notjson"))

	_, err := p.Complete(context.Background(), llm.Request{
		Prompt:     "x",
		JSONSchema: map[string]any{"type": "object"},
	})
	if err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Fatalf("err = %v, want not-a-JSON-object", err)
	}
}

func TestCompleteNonZeroExitCarriesStderr(t *testing.T) {
	p := newProvider(t, helperConfig(t, "fail"))

	_, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exited with 3") || !strings.Contains(err.Error(), "model overloaded") {
		t.Fatalf("error lacks exit code or stderr: %v", err)
	}
}

func TestCompleteEmptyAnswer(t *testing.T) {
	p := newProvider(t, helperConfig(t, "empty"))

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x"}); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err = %v, want ErrNoAnswer", err)
	}
}

func TestCompleteOutputTooLarge(t *testing.T) {
	p := newProvider(t, helperConfig(t, "huge"))
	p.maxOutputBytes = 1 << 20

	_, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("err = %v, want ErrOutputTooLarge", err)
	}
}

func TestCompleteOutputWithinLimit(t *testing.T) {
	p := newProvider(t, helperConfig(t, "huge"))
	p.maxOutputBytes = 16 << 20

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Text) != 9<<20 {
		t.Fatalf("len(Text) = %d", len(resp.Text))
	}
}

func TestCompleteTimeoutKillsProgram(t *testing.T) {
	cfg := helperConfig(t, "sleep")
	cfg.Timeout = "300ms"
	p := newProvider(t, cfg)

	start := time.Now()
	_, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("timeout not enforced, took %v", elapsed)
	}
}

func TestCompleteCancelledContext(t *testing.T) {
	p := newProvider(t, helperConfig(t, "sleep"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Complete(ctx, llm.Request{Prompt: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
}

func TestCompleteValidatesRequest(t *testing.T) {
	p := newProvider(t, helperConfig(t, "echo"))

	for name, req := range map[string]llm.Request{
		"empty prompt":     {Prompt: "   "},
		"negative max":     {Prompt: "x", MaxTokens: -5},
		"empty schema map": {Prompt: "x", JSONSchema: map[string]any{}},
	} {
		if _, err := p.Complete(context.Background(), req); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNoShellInterpretation(t *testing.T) {
	// Arguments are passed verbatim: metacharacters are not expanded.
	const weird = "$(echo pwned); `id` && rm -rf /"
	cfg := helperConfig(t, "args", weird, "a b")
	p := newProvider(t, cfg)

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal([]byte(resp.Text), &args); err != nil {
		t.Fatalf("args not JSON: %v (%s)", err, resp.Text)
	}
	if len(args) != 2 || args[0] != weird || args[1] != "a b" {
		t.Fatalf("args = %q", args)
	}
}

func TestCompleteConcurrent(t *testing.T) {
	p := newProvider(t, helperConfig(t, "text"))

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
			if err == nil && resp.Text != "plain answer" {
				err = fmt.Errorf("Text = %q", resp.Text)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
