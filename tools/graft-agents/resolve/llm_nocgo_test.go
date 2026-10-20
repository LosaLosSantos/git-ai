//go:build !cgo

package resolve

import (
	"strings"
	"testing"
)

// TestLLMStrategyRefusesToStartWithoutGrammars checks that a build without cgo refuses an llm
// strategy at startup, naming the build requirement, instead of rejecting every answer later.
func TestLLMStrategyRefusesToStartWithoutGrammars(t *testing.T) {
	cfg, err := ParseConfig([]byte("[[strategy]]\ntype = \"llm\"\n[strategy.llm]\ntype = \"command\"\ncommand = [\"true\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfg.Build()
	if err == nil || !strings.Contains(err.Error(), "CGO_ENABLED=1") || !strings.Contains(err.Error(), "C compiler") {
		t.Fatalf("err = %v, want a refusal naming the build requirement", err)
	}
}
