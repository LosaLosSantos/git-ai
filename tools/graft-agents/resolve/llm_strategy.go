package resolve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

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
//	max_prompt_bytes = 65536       # optional: largest request of one region
//	context_lines = 3              # optional: context lines of the PATCH and the CODE
//	max_answer_bytes = 1048576     # optional
//	max_regions = 20               # optional: most conflicting regions per file (one call each)
//	call_timeout = "120s"          # optional: bound on one model call (one region)
//
//	[strategy.llm]
//	type = "openai"
//	model = "llama3.1"
//	base_url = "http://localhost:11434/v1"
//	timeout = "120s"               # the whole file, all its calls; call_timeout bounds each
//	max_tokens = 4096              # optional: answer limit; unset = provider default
//
//	[strategy.llm.options]         # provider specific; the built-in providers take none
//	...                            # and fail at startup when any is set
//
//	[strategy.llm.budget]          # optional: monthly spending cap (budget_strategy.go)
//	...
//
//	[strategy.llm.agree_with]      # optional, off by default: the agreement gate (PLAN10 15.13)
//	type = "openai"                # a second model, with the keys of [strategy.llm] (not agree_with)
//	model = "..."
//
// With agree_with, both models resolve each file at the same time with the same prompt, and the
// first model's resolution is proposed as always (it lands only through verify.command). It is
// HELD, and never lands, unless the two results are the same file after normalization
// (LLMNormalize) and both composition reports are clean (LLMReportClean): the response carries
// Hold, the report says that the second model disagreed, and the server neither verifies nor
// lands the file. A model that fails makes the file unresolved when it is the first, and holds it
// when it is the second. A budget on one model needs a budget on the other.
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
// The prompt carries the commit message of the change (request.messages.commit)
// as the intent of the PATCH, as synthmerge does; the tip's message is not
// sent. The model reports no confidence: an answer is accepted on the checks
// alone, and min_confidence is no longer an option.
func init() {
	RegisterStrategy("llm", newLLMStrategy)
}

// llmStrategy is immutable after construction. Resolve may run concurrently
// (HTTP mode), so the provider must be safe for concurrent use.
type llmStrategy struct {
	llmSide
	// agree is the second model of the agree_with gate, or nil.
	agree *llmSide
}

// llmSide is one model of the strategy: its provider, behind the budget if there is one, and the
// options of its calls.
type llmSide struct {
	// provider is base, or base behind the budget.
	provider llm.Provider
	base     llm.Provider
	budget   *Budget
	// model is the configured model name, and routing its OpenRouter routing, if any.
	model   string
	routing *llm.Routing
	opt     LLMOptions
	timeout time.Duration
}

// agreeSuffix names the second model of a strategy where the strategy name keys a map (budgets).
const agreeSuffix = "/agree_with"

func newLLMStrategy(spec StrategySpec) (Strategy, error) {
	// Every llm answer gets the syntax check, which needs the tree-sitter grammars: a build
	// without them would reject every answer for those languages, so it does not start.
	if !syntax.Available {
		return nil, fmt.Errorf("strategy %q: the llm strategy needs the tree-sitter grammars of its syntax check, which this graft-resolve lacks: build it with CGO_ENABLED=1 and a C compiler", spec.Name)
	}
	if _, ok := spec.Options["min_confidence"]; ok {
		return nil, fmt.Errorf("strategy %q: min_confidence is gone: the model reports no confidence and an answer is accepted on the checks alone; remove the option", spec.Name)
	}
	if err := allowOnly(spec, "llm", "max_prompt_bytes", "context_lines", "max_answer_bytes", "max_regions", "call_timeout"); err != nil {
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
	if v, ok := spec.Options["call_timeout"]; ok {
		if opt.CallTimeout, err = durationValue(v, "call_timeout"); err != nil || opt.CallTimeout <= 0 {
			return nil, fmt.Errorf("strategy %q: call_timeout must be a positive Go duration", spec.Name)
		}
	}

	tbl, err := llmTable(spec)
	if err != nil {
		return nil, err
	}
	side, err := newLLMSide(spec, tbl, "llm", opt)
	if err != nil {
		return nil, err
	}
	s := &llmStrategy{llmSide: *side}

	if raw, ok := tbl["agree_with"]; ok {
		atbl, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("strategy %q: llm.agree_with must be a table", spec.Name)
		}
		if s.agree, err = newLLMSide(spec, atbl, "llm.agree_with", opt); err != nil {
			return nil, err
		}
		// A spending cap that covers one of the two models would not be a cap.
		if (s.budget == nil) != (s.agree.budget == nil) {
			return nil, fmt.Errorf("strategy %q: llm.budget and llm.agree_with.budget go together: the cap must cover both models", spec.Name)
		}
	}
	return s, nil
}

// newLLMSide builds one model of the strategy from its table (where names it in errors). opt holds
// the options of the strategy; the side's own max_tokens and timeout replace the ones in it.
func newLLMSide(spec StrategySpec, tbl map[string]any, where string, opt LLMOptions) (*llmSide, error) {
	cfg, err := llmProviderConfigOf(spec, tbl, where)
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
		return nil, fmt.Errorf("strategy %q: %s: %w", spec.Name, where, err)
	}
	p, err := llm.New(cfg)
	if errors.Is(err, llm.ErrDisabled) {
		return nil, fmt.Errorf("strategy %q: the %s table needs a provider type", spec.Name, where)
	}
	if err != nil {
		return nil, fmt.Errorf("strategy %q: %s: %w", spec.Name, where, err)
	}
	side := &llmSide{provider: p, base: p, model: cfg.Model, routing: cfg.Routing, opt: opt, timeout: timeout}

	budget, err := llmBudgetConfig(spec, tbl)
	if err != nil {
		return nil, err
	}
	if budget != nil {
		if opt.MaxTokens <= 0 {
			return nil, fmt.Errorf("strategy %q: %s.budget needs max_tokens in [strategy.%s]: it bounds the worst case of a call", spec.Name, where, where)
		}
		side.budget = budget
		side.provider = budgetProvider{p: p, b: budget}
	}
	return side, nil
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
		TheirsMessage: req.Messages.Commit,
	}
	if f.Base != nil {
		lf.Base = llmBytes(f.Base.Content)
	}

	stats := llmRegionStatsFrom(ctx)
	var second *agreeRun
	if s.agree != nil {
		// Both models resolve the file with the same prompt of record, at the same time.
		second = s.agree.start(ctx, lf, stats != nil)
	}

	first, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	res, err := ResolveLLM(first, s.provider, lf, s.opt)
	if second != nil {
		// The second model is waited for whatever the first one did: its calls are paid for, and
		// its usage belongs to the file.
		second.wait()
		if stats != nil {
			stats.Absorb(second.stats, "agree_with: ")
		}
	}
	switch {
	case errors.Is(err, ErrLLMNotApplicable):
		return Resolution{}, false, nil
	case err != nil:
		return Resolution{}, false, err
	}
	if stats != nil {
		stats.Record(res.Hunks, res.RegionsEqualOurs)
	}
	out := Resolution{Action: protocol.ActionWrite, Mode: f.Ours.Mode, Content: res.Content, Report: res.Report}
	if second != nil {
		if why := s.agreement(res, second); why != "" {
			out.Hold = why
			out.Report = append(append([]string(nil), res.Report...), "the second model disagreed, so the resolution is held as a proposal: "+why)
		}
	}
	return out, true, nil
}

// agreeRun is the second model resolving a file, started by agree.start.
type agreeRun struct {
	done  chan struct{}
	res   LLMResult
	err   error
	stats *LLMRegionStats
}

// start resolves lf with the side's model in the background. When counting, the run keeps its own
// stats, which the caller absorbs into its own once the run is waited for.
func (s *llmSide) start(ctx context.Context, lf LLMFile, counting bool) *agreeRun {
	r := &agreeRun{done: make(chan struct{})}
	if counting {
		r.stats = &LLMRegionStats{}
		ctx = WithLLMRegionStats(ctx, r.stats)
	}
	go func() {
		defer close(r.done)
		defer func() {
			if p := recover(); p != nil {
				r.err = fmt.Errorf("resolve: the second model panicked: %v", p)
			}
		}()
		ctx, cancel := context.WithTimeout(ctx, s.timeout)
		defer cancel()
		r.res, r.err = ResolveLLM(ctx, s.provider, lf, s.opt)
	}()
	return r
}

// wait blocks until the run is over.
func (r *agreeRun) wait() { <-r.done }

// agreement says why the first model's resolution is not the one both models give, or "" when the
// two agree: the same file after the judge's normalization (LLMNormalize), and neither report
// holds a finding (LLMReportClean). The first model's answer is the one that is proposed.
func (s *llmStrategy) agreement(first LLMResult, second *agreeRun) string {
	switch {
	case second.err != nil && errors.Is(second.err, ErrLLMNotApplicable):
		return fmt.Sprintf("the second model (%s) could not take the file", s.agree.model)
	case second.err != nil:
		return fmt.Sprintf("the second model (%s) gave no usable answer: %s", s.agree.model, llmCutText(second.err.Error(), 200))
	case !LLMReportClean(first.Report):
		return fmt.Sprintf("the composition report of the first model (%s) holds a finding", s.model)
	case !LLMReportClean(second.res.Report):
		return fmt.Sprintf("the composition report of the second model (%s) holds a finding", s.agree.model)
	case !bytes.Equal(LLMNormalize(first.Content), LLMNormalize(second.res.Content)):
		return fmt.Sprintf("the second model (%s) resolved the file differently from the first (%s)", s.agree.model, s.model)
	}
	return ""
}

// llmCutText cuts s to n bytes at a rune boundary.
func llmCutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
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

// llmTable returns the [strategy.llm] table.
func llmTable(spec StrategySpec) (map[string]any, error) {
	raw, ok := spec.Options["llm"]
	if !ok {
		return nil, fmt.Errorf("strategy %q: missing [strategy.llm] table", spec.Name)
	}
	tbl, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("strategy %q: llm must be a table", spec.Name)
	}
	return tbl, nil
}

// llmProviderConfigOf decodes a provider table: [strategy.llm], or its agree_with table, which has
// the same keys but no agree_with of its own (where is "llm" or "llm.agree_with").
func llmProviderConfigOf(spec StrategySpec, tbl map[string]any, where string) (llm.Config, error) {
	var unknown []string
	for k := range tbl {
		switch k {
		case "type", "model", "base_url", "api_key_env", "command", "timeout", "max_tokens", "effort", "options", "routing", "budget":
		case "agree_with":
			if where != "llm" {
				unknown = append(unknown, k)
			}
		default:
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return llm.Config{}, fmt.Errorf("strategy %q: %s: unknown keys %v", spec.Name, where, unknown)
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
			return llm.Config{}, fmt.Errorf("strategy %q: %s: %w", spec.Name, where, err)
		}
	}
	if cfg.Command, err = stringListField(tbl, "command"); err != nil {
		return llm.Config{}, fmt.Errorf("strategy %q: %s: %w", spec.Name, where, err)
	}
	if v, ok := tbl["max_tokens"]; ok {
		n, ok := v.(int64)
		if !ok || n < 0 || n > 1<<20 {
			return llm.Config{}, fmt.Errorf("strategy %q: %s: max_tokens must be an integer between 0 and %d", spec.Name, where, 1<<20)
		}
		cfg.MaxTokens = int(n)
	}
	if v, ok := tbl["options"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return llm.Config{}, fmt.Errorf("strategy %q: %s: options must be a table", spec.Name, where)
		}
		cfg.Options = make(map[string]string, len(m))
		for k, val := range m {
			s, ok := val.(string)
			if !ok {
				return llm.Config{}, fmt.Errorf("strategy %q: %s: option %q must be a string", spec.Name, where, k)
			}
			cfg.Options[k] = s
		}
	}
	if v, ok := tbl["routing"]; ok {
		if cfg.Routing, err = routingField(v); err != nil {
			return llm.Config{}, fmt.Errorf("strategy %q: %s: routing: %w", spec.Name, where, err)
		}
	}
	if cfg.Type == "" {
		return llm.Config{}, fmt.Errorf("strategy %q: %s: type is required", spec.Name, where)
	}
	return cfg, nil
}

// routingField reads the [strategy.llm.routing] table: the lists only and ignore, and nothing else.
func routingField(v any) (*llm.Routing, error) {
	tbl, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must be a table")
	}
	for k := range tbl {
		if k != "only" && k != "ignore" {
			return nil, fmt.Errorf("unknown key %q (want only, ignore)", k)
		}
	}
	var r llm.Routing
	var err error
	if r.Only, err = stringListField(tbl, "only"); err != nil {
		return nil, err
	}
	if r.Ignore, err = stringListField(tbl, "ignore"); err != nil {
		return nil, err
	}
	return &r, nil
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
