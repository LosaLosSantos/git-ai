package resolve

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/syntax"
)

// The "llm" strategy resolves content conflicts with a language model. It is
// registered here, so any program that imports this package can use it from
// configuration. The model backend is chosen by the nested [strategy.llm]
// table and must be registered by the program (blank import of
// tools/graft-agents/llm/anthropic, llm/openai or llm/command):
//
//	[[strategy]]
//	type = "llm"
//	paths = ["**/*.go", "docs/**"]
//	max_prompt_bytes = 65536       # optional: largest prompt sent (whole file or region)
//	context_lines = 8              # optional
//	max_answer_bytes = 1048576     # optional
//	max_regions = 32               # optional: most conflicting regions per file
//
//	[strategy.llm]
//	type = "openai"
//	model = "llama3.1"
//	base_url = "http://localhost:11434/v1"
//	timeout = "120s"
//	max_tokens = 4096              # optional: answer limit; unset = provider default
//
//	[strategy.llm.options]         # optional, provider specific
//	...
//
//	[strategy.llm.budget]          # optional: monthly spending cap (budget_strategy.go)
//	...
//
// The strategy declines (the chain continues, no model call is made) for files
// it cannot merge: a side that is absent, a symlink, a mode conflict, binary
// content, a diff too large to split (the LCS table of the changed middle of
// the file is limited to 8M cells, roughly 2800 changed lines on each side),
// more conflicting regions than max_regions, or a region whose prompt exceeds
// max_prompt_bytes. A rejected or failed answer is a strategy failure and the
// chain continues as well. An answer for a path with a tree-sitter grammar
// (package syntax: TypeScript, JavaScript, Python, Go, Rust, Java) must parse,
// or it is a check failure; the checks configured for the path run after.
//
// The prompt carries the commit messages of both sides (request.messages) as
// the intent of each side. The model reports no confidence: an answer is
// accepted on the checks alone, and min_confidence is no longer an option.
func init() {
	RegisterStrategy("llm", newLLMStrategy)
}

// llmStrategy is immutable after construction. Resolve may run concurrently
// (HTTP mode), so the provider must be safe for concurrent use.
type llmStrategy struct {
	// provider is base, or base behind the budget.
	provider llm.Provider
	base     llm.Provider
	budget   *Budget
	// model is the configured model name.
	model   string
	opt     LLMOptions
	timeout time.Duration
}

func newLLMStrategy(spec StrategySpec) (Strategy, error) {
	// Every llm answer gets the syntax check, which needs the tree-sitter grammars: a build
	// without them would reject every answer for those languages, so it does not start.
	if !syntax.Available {
		return nil, fmt.Errorf("strategy %q: the llm strategy needs the tree-sitter grammars of its syntax check, which this graft-resolve lacks: build it with CGO_ENABLED=1 and a C compiler", spec.Name)
	}
	if _, ok := spec.Options["min_confidence"]; ok {
		return nil, fmt.Errorf("strategy %q: min_confidence is gone: the model reports no confidence and an answer is accepted on the checks alone; remove the option", spec.Name)
	}
	if err := allowOnly(spec, "llm", "max_prompt_bytes", "context_lines", "max_answer_bytes", "max_regions"); err != nil {
		return nil, err
	}

	var opt LLMOptions
	var err error
	if opt.MaxPromptBytes, err = llmOptInt(spec, "max_prompt_bytes"); err != nil {
		return nil, err
	}
	if opt.ContextLines, err = llmOptInt(spec, "context_lines"); err != nil {
		return nil, err
	}
	if opt.MaxAnswerBytes, err = llmOptInt(spec, "max_answer_bytes"); err != nil {
		return nil, err
	}
	if opt.MaxRegions, err = llmOptInt(spec, "max_regions"); err != nil {
		return nil, err
	}

	cfg, err := llmProviderConfig(spec)
	if err != nil {
		return nil, err
	}
	// The configured limit is the answer budget of every request. Unset, the
	// provider's own default applies.
	opt.MaxTokens = cfg.MaxTokens
	if opt, err = opt.withDefaults(); err != nil {
		return nil, fmt.Errorf("strategy %q: %w", spec.Name, err)
	}

	timeout, err := cfg.TimeoutOrDefault()
	if err != nil {
		return nil, fmt.Errorf("strategy %q: %w", spec.Name, err)
	}
	p, err := llm.New(cfg)
	if errors.Is(err, llm.ErrDisabled) {
		return nil, fmt.Errorf("strategy %q: the llm table needs a provider type", spec.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("strategy %q: %w", spec.Name, err)
	}
	s := &llmStrategy{provider: p, base: p, model: cfg.Model, opt: opt, timeout: timeout}

	budget, err := llmBudgetConfig(spec, spec.Options["llm"].(map[string]any))
	if err != nil {
		return nil, err
	}
	if budget != nil {
		if opt.MaxTokens <= 0 {
			return nil, fmt.Errorf("strategy %q: llm.budget needs max_tokens in [strategy.llm]: it bounds the worst case of a call", spec.Name)
		}
		s.budget = budget
		s.provider = budgetProvider{p: p, b: budget}
	}
	return s, nil
}

// Resolve implements Strategy.
func (s *llmStrategy) Resolve(ctx context.Context, f File, req protocol.Request) (Resolution, bool, error) {
	if !regularFile(f.Ours) || !regularFile(f.Theirs) || f.Ours.Mode != f.Theirs.Mode {
		return Resolution{}, false, nil
	}
	if f.Base != nil && f.Base.Mode == protocol.ModeSymlink {
		return Resolution{}, false, nil
	}

	lf := LLMFile{
		Path:          f.Path,
		Ours:          llmBytes(f.Ours.Content),
		Theirs:        llmBytes(f.Theirs.Content),
		OursMessage:   req.Messages.Tip,
		TheirsMessage: req.Messages.Commit,
	}
	if f.Base != nil {
		lf.Base = llmBytes(f.Base.Content)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	res, err := ResolveLLM(ctx, s.provider, lf, s.opt)
	switch {
	case errors.Is(err, ErrLLMNotApplicable):
		return Resolution{}, false, nil
	case err != nil:
		return Resolution{}, false, err
	}
	return Resolution{Action: protocol.ActionWrite, Mode: f.Ours.Mode, Content: res.Content}, true, nil
}

// syntaxChecked marks the llm answers for the syntax check (resolveFile).
func (s *llmStrategy) syntaxChecked() {}

// llmBytes keeps an empty side distinct from an absent one: ResolveLLM treats
// nil as absent.
func llmBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// llmProviderConfig decodes the [strategy.llm] table.
func llmProviderConfig(spec StrategySpec) (llm.Config, error) {
	raw, ok := spec.Options["llm"]
	if !ok {
		return llm.Config{}, fmt.Errorf("strategy %q: missing [strategy.llm] table", spec.Name)
	}
	tbl, ok := raw.(map[string]any)
	if !ok {
		return llm.Config{}, fmt.Errorf("strategy %q: llm must be a table", spec.Name)
	}

	var unknown []string
	for k := range tbl {
		switch k {
		case "type", "model", "base_url", "api_key_env", "command", "timeout", "max_tokens", "effort", "options", "budget":
		default:
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return llm.Config{}, fmt.Errorf("strategy %q: llm: unknown keys %v", spec.Name, unknown)
	}

	var cfg llm.Config
	var err error
	strs := []struct {
		key string
		dst *string
	}{
		{"type", &cfg.Type}, {"model", &cfg.Model}, {"base_url", &cfg.BaseURL},
		{"api_key_env", &cfg.APIKeyEnv}, {"timeout", &cfg.Timeout}, {"effort", &cfg.Effort},
	}
	for _, s := range strs {
		if *s.dst, err = stringField(tbl, s.key); err != nil {
			return llm.Config{}, fmt.Errorf("strategy %q: llm: %w", spec.Name, err)
		}
	}
	if cfg.Command, err = stringListField(tbl, "command"); err != nil {
		return llm.Config{}, fmt.Errorf("strategy %q: llm: %w", spec.Name, err)
	}
	if v, ok := tbl["max_tokens"]; ok {
		n, ok := v.(int64)
		if !ok || n < 0 || n > 1<<20 {
			return llm.Config{}, fmt.Errorf("strategy %q: llm: max_tokens must be an integer between 0 and %d", spec.Name, 1<<20)
		}
		cfg.MaxTokens = int(n)
	}
	if v, ok := tbl["options"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return llm.Config{}, fmt.Errorf("strategy %q: llm: options must be a table", spec.Name)
		}
		cfg.Options = make(map[string]string, len(m))
		for k, val := range m {
			s, ok := val.(string)
			if !ok {
				return llm.Config{}, fmt.Errorf("strategy %q: llm: option %q must be a string", spec.Name, k)
			}
			cfg.Options[k] = s
		}
	}
	if cfg.Type == "" {
		return llm.Config{}, fmt.Errorf("strategy %q: llm: type is required", spec.Name)
	}
	return cfg, nil
}

// llmOptInt reads an optional non-negative integer option of the strategy.
func llmOptInt(spec StrategySpec, key string) (int, error) {
	v, ok := spec.Options[key]
	if !ok {
		return 0, nil
	}
	n, ok := v.(int64)
	if !ok || n < 0 || n > 1<<31-1 {
		return 0, fmt.Errorf("strategy %q: %s must be a non-negative integer", spec.Name, key)
	}
	return int(n), nil
}
