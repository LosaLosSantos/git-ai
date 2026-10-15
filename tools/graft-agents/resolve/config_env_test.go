package resolve

import (
	"context"
	"os/exec"
	"testing"
)

// documentedConfig is the command strategy and gofmt check that doc/graft
// documents (see the Config comment). Tests keep it runnable.
const documentedConfig = `
[[strategy]]
type = "command"
command = ["git", "merge-file", "-p", "{ours}", "{base}", "{theirs}"]
output = "stdout"
env = ["LANG"]

[[check]]
paths = ["**/*.go"]
command = ['sh', '-c', 'out=$(gofmt -l "$1") && test -z "$out"', 'sh', '{file}']
env = ["HOME"]
`

func TestParseConfigCommandOutputAndEnv(t *testing.T) {
	cfg, err := ParseConfig([]byte(documentedConfig))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if _, err := cfg.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := cfg.Checks[0].Env; len(got) != 1 || got[0] != "HOME" {
		t.Fatalf("check env = %v", got)
	}
	if got := cfg.Checks[0].Command[2]; got != `out=$(gofmt -l "$1") && test -z "$out"` {
		t.Fatalf("check script = %q", got)
	}
}

func TestParseConfigRejectsBadEnvAndOutput(t *testing.T) {
	cases := map[string]string{
		"check env bad name": "[[check]]\npaths = [\"**\"]\ncommand = [\"true\"]\nenv = [\"A B\"]",
		"check env not list": "[[check]]\npaths = [\"**\"]\ncommand = [\"true\"]\nenv = \"LANG\"",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(src)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	bad := "[[strategy]]\ntype = \"command\"\ncommand = [\"x\"]\noutput = \"sideways\""
	cfg, err := ParseConfig([]byte(bad))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if _, err := cfg.Build(); err == nil {
		t.Fatal("Build accepted an unknown output mode")
	}
}

func TestDocumentedGofmtCheckRejectsUnformatted(t *testing.T) {
	// The example check must reject what it claims to reject: unformatted
	// files and syntax errors, not only the first.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not installed")
	}
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("gofmt is not installed")
	}
	cfg, err := ParseConfig([]byte(documentedConfig))
	if err != nil {
		t.Fatal(err)
	}
	check := cfg.Checks[0]
	ctx := context.Background()

	if err := runCheck(ctx, check, "pkg/a.go", []byte("package a\n\nfunc f() {}\n")); err != nil {
		t.Fatalf("formatted file rejected: %v", err)
	}
	if err := runCheck(ctx, check, "pkg/a.go", []byte("package a\nfunc  f( ){}\n")); err == nil {
		t.Fatal("unformatted file accepted")
	}
	if err := runCheck(ctx, check, "pkg/a.go", []byte("package a\nfunc (\n")); err == nil {
		t.Fatal("syntax error accepted")
	}
}
