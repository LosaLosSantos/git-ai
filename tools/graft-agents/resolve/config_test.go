package resolve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fullConfig = `
timeout = "90s"

[[strategy]]
type = "trivial"

[[strategy]]
name = "changelog"
type = "union"
paths = ["CHANGELOG.md", "docs/**"]

[[strategy]]
type = "rerere"
dir = "/tmp/rr"

[[strategy]]
type = "command"
command = ["merge-tool", "{base}", "{ours}", "{theirs}", "{out}"]
timeout = "45s"
paths = ["**"]

[[check]]
paths = ["**/*.go"]
command = ["gofmt", "-l", "{file}"]
timeout = "10s"

[[check]]
paths = ["CHANGELOG.md"]
command = ["true", "{file}"]
`

func TestParseConfigFull(t *testing.T) {
	cfg, err := ParseConfig([]byte(fullConfig))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Timeout != 90*time.Second {
		t.Fatalf("timeout = %s", cfg.Timeout)
	}
	if len(cfg.Strategies) != 4 {
		t.Fatalf("strategies = %+v", cfg.Strategies)
	}
	if s := cfg.Strategies[0]; s.Name != "trivial" || s.Type != "trivial" {
		t.Fatalf("default name not set: %+v", s)
	}
	if s := cfg.Strategies[1]; s.Name != "changelog" || s.Type != "union" ||
		len(s.Paths) != 2 || s.Paths[1] != "docs/**" {
		t.Fatalf("union spec = %+v", s)
	}
	if s := cfg.Strategies[2]; s.Options["dir"] != "/tmp/rr" {
		t.Fatalf("rerere options = %+v", s.Options)
	}
	if s := cfg.Strategies[3]; s.Options["timeout"] != "45s" {
		t.Fatalf("command options = %+v", s.Options)
	}
	if len(cfg.Checks) != 2 || cfg.Checks[0].Timeout != 10*time.Second || cfg.Checks[1].Timeout != 0 {
		t.Fatalf("checks = %+v", cfg.Checks)
	}
	if cfg.Checks[0].Command[2] != "{file}" {
		t.Fatalf("check command = %v", cfg.Checks[0].Command)
	}
}

func TestParseConfigDefaultTimeout(t *testing.T) {
	cfg, err := ParseConfig([]byte(`[[strategy]]
type = "trivial"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != DefaultResolveTimeout {
		t.Fatalf("timeout = %s, want %s", cfg.Timeout, DefaultResolveTimeout)
	}
}

func TestParseConfigRejects(t *testing.T) {
	cases := map[string]string{
		"unknown top-level key":  `colour = "red"`,
		"bad timeout":            `timeout = "soon"`,
		"zero timeout":           `timeout = "0s"`,
		"timeout not a string":   `timeout = 5`,
		"strategy not an array":  "[strategy]\ntype = \"trivial\"",
		"strategy missing type":  "[[strategy]]\nname = \"x\"",
		"strategy type not str":  "[[strategy]]\ntype = 3",
		"strategy bad name":      "[[strategy]]\ntype = \"trivial\"\nname = \"a b\"",
		"strategy bad paths":     "[[strategy]]\ntype = \"union\"\npaths = \"x\"",
		"strategy absolute glob": "[[strategy]]\ntype = \"union\"\npaths = [\"/etc/*\"]",
		"check not an array":     "[check]\npaths = [\"x\"]\ncommand = [\"true\"]",
		"check unknown key":      "[[check]]\npaths = [\"x\"]\ncommand = [\"true\"]\nshell = true",
		"check without paths":    "[[check]]\ncommand = [\"true\"]",
		"check without command":  "[[check]]\npaths = [\"x\"]",
		"check bad timeout":      "[[check]]\npaths = [\"x\"]\ncommand = [\"true\"]\ntimeout = \"-1s\"",
		"syntax error":           `timeout = `,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(src)); err == nil {
				t.Fatalf("accepted invalid config %q", src)
			}
		})
	}
}

func TestBuildRejectsUnknownOptionsAndTypes(t *testing.T) {
	cases := map[string]string{
		"unknown strategy type": "[[strategy]]\ntype = \"telepathy\"",
		"trivial with option":   "[[strategy]]\ntype = \"trivial\"\nfoo = 1",
		"union without paths":   "[[strategy]]\ntype = \"union\"",
		"rerere without dir":    "[[strategy]]\ntype = \"rerere\"",
		"command without argv":  "[[strategy]]\ntype = \"command\"",
		"command empty argv":    "[[strategy]]\ntype = \"command\"\ncommand = [\"\"]",
		"command bad timeout":   "[[strategy]]\ntype = \"command\"\ncommand = [\"x\"]\ntimeout = \"never\"",
		"no strategies":         `timeout = "1m"`,
		"dir not a string":      "[[strategy]]\ntype = \"rerere\"\ndir = 4",
		"paths not strings":     "[[strategy]]\ntype = \"union\"\npaths = [1]",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(src))
			if err != nil {
				// Parse-level rejection is acceptable too.
				return
			}
			if _, err := cfg.Build(); err == nil {
				t.Fatalf("Build accepted %q", src)
			}
		})
	}
}

func TestBuildUnknownTypeListsAvailable(t *testing.T) {
	cfg, err := ParseConfig([]byte("[[strategy]]\ntype = \"nope\""))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfg.Build()
	if err == nil || !strings.Contains(err.Error(), "trivial") {
		t.Fatalf("error = %v, want the available types listed", err)
	}
}

func TestBuildFullConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte(fullConfig))
	if err != nil {
		t.Fatal(err)
	}
	r, err := cfg.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if r.Timeout != 90*time.Second {
		t.Fatalf("resolver timeout = %s", r.Timeout)
	}
	if len(r.bindings) != 4 || len(r.checks) != 2 {
		t.Fatalf("bindings %d checks %d", len(r.bindings), len(r.checks))
	}
	if r.bindings[1].Name != "changelog" {
		t.Fatalf("binding name = %q", r.bindings[1].Name)
	}
}

func TestDefaultConfigBuilds(t *testing.T) {
	cfg := DefaultConfig()
	r, err := cfg.Build()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	if len(r.bindings) != 1 || r.bindings[0].Name != "trivial" {
		t.Fatalf("default bindings = %+v", r.bindings)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolve.toml")
	if err := os.WriteFile(path, []byte(fullConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.toml")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestRegisterStrategyHook(t *testing.T) {
	const typ = "test-registered-hook"
	called := false
	RegisterStrategy(typ, func(spec StrategySpec) (Strategy, error) {
		called = true
		llm, ok := spec.Options["llm"].(map[string]any)
		if !ok || llm["model"] != "m" {
			t.Errorf("factory did not receive its sub-table: %+v", spec.Options)
		}
		return trivial{}, nil
	})
	cfg, err := ParseConfig([]byte("[[strategy]]\ntype = \"" + typ + "\"\n[strategy.llm]\nmodel = \"m\""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Build(); err != nil {
		t.Fatalf("Build with registered type: %v", err)
	}
	if !called {
		t.Fatal("factory not called")
	}
	if !strings.Contains(strings.Join(StrategyTypes(), ","), typ) {
		t.Fatal("registered type missing from StrategyTypes")
	}
}

func TestRegisterStrategyDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration did not panic")
		}
	}()
	RegisterStrategy("trivial", newTrivial)
}
