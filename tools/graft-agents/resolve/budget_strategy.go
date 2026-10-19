package resolve

import (
	"errors"
	"fmt"
	"sort"
)

// The llm strategy's budget is the [strategy.llm.budget] table:
//
//	[strategy.llm.budget]
//	monthly_usd = 50                 # the cap; 0 or absent: none (the spend is still tracked)
//	input_usd_per_million = 2.0      # the model's prices, kept current by the operator
//	output_usd_per_million = 10.0    # output tokens, reasoning included
//	request_usd = 0                  # optional: a fixed price per call
//	state = "/var/lib/graft/resolve-budget.json"
//
// The table needs max_tokens in [strategy.llm], which bounds the worst case of a call. When the
// month's spend plus the worst case of a call would exceed the cap, the call is not made and the
// file fails with "AI budget exhausted", which the server puts into the conflict package.

// llmBudgetConfig decodes the optional budget table of the [strategy.llm] table.
func llmBudgetConfig(spec StrategySpec, tbl map[string]any) (*Budget, error) {
	raw, ok := tbl["budget"]
	if !ok {
		return nil, nil
	}
	bt, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("strategy %q: llm: budget must be a table", spec.Name)
	}
	var unknown []string
	for k := range bt {
		switch k {
		case "monthly_usd", "input_usd_per_million", "output_usd_per_million", "request_usd", "state":
		default:
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("strategy %q: llm.budget: unknown keys %v", spec.Name, unknown)
	}

	number := func(key string) (float64, error) {
		switch v := bt[key].(type) {
		case nil:
			return 0, nil
		case int64:
			if v >= 0 {
				return float64(v), nil
			}
		case float64:
			if v >= 0 {
				return v, nil
			}
		}
		return 0, fmt.Errorf("strategy %q: llm.budget: %s must be a non-negative number", spec.Name, key)
	}
	var prices Prices
	var monthly float64
	var err error
	for _, f := range []struct {
		key string
		dst *float64
	}{
		{"monthly_usd", &monthly},
		{"input_usd_per_million", &prices.InputPerMillion},
		{"output_usd_per_million", &prices.OutputPerMillion},
		{"request_usd", &prices.PerRequest},
	} {
		if *f.dst, err = number(f.key); err != nil {
			return nil, err
		}
	}
	state, err := stringField(bt, "state")
	if err != nil {
		return nil, fmt.Errorf("strategy %q: llm.budget: %w", spec.Name, err)
	}
	if state == "" {
		return nil, fmt.Errorf("strategy %q: llm.budget: state is required: the spend must survive restarts", spec.Name)
	}
	b, err := NewBudget(monthly, prices, state)
	if err != nil {
		return nil, fmt.Errorf("strategy %q: llm.%w", spec.Name, err)
	}
	return b, nil
}

// UseBudget makes every llm strategy of r go through b, in place of a configured budget, and
// returns how many there are. It must be called before the first Resolve. Each llm strategy
// needs max_tokens.
func (r *Resolver) UseBudget(b *Budget) (int, error) {
	if b == nil {
		return 0, errors.New("resolve: nil budget")
	}
	n := 0
	for _, bd := range r.bindings {
		s, ok := bd.Strategy.(*llmStrategy)
		if !ok {
			continue
		}
		if s.opt.MaxTokens <= 0 {
			return 0, fmt.Errorf("strategy %q: a budget needs max_tokens in [strategy.llm]", bd.Name)
		}
		s.budget = b
		s.provider = budgetProvider{p: s.base, b: b}
		n++
	}
	return n, nil
}

// LLMModels returns the configured model of each llm strategy of r, by strategy name.
func (r *Resolver) LLMModels() map[string]string {
	out := map[string]string{}
	for _, bd := range r.bindings {
		if s, ok := bd.Strategy.(*llmStrategy); ok {
			out[bd.Name] = s.model
		}
	}
	return out
}

// LLMMaxPromptTokens returns an upper bound of the input tokens of one model call of r's llm
// strategies, counted like the budget's worst case: one token per byte. A prompt holds file data
// within max_prompt_bytes (whole file or one region), the two commit messages, the system prompt,
// the schema and the framing; 64 KiB covers all but the file data.
func (r *Resolver) LLMMaxPromptTokens() int {
	n := 0
	for _, bd := range r.bindings {
		if s, ok := bd.Strategy.(*llmStrategy); ok {
			n = max(n, s.opt.MaxPromptBytes+2*llmMaxMessageBytes+64<<10)
		}
	}
	return n
}

// Budgets returns the budgets of r's strategies, by strategy name.
func (r *Resolver) Budgets() map[string]*Budget {
	out := map[string]*Budget{}
	for _, bd := range r.bindings {
		if s, ok := bd.Strategy.(*llmStrategy); ok && s.budget != nil {
			out[bd.Name] = s.budget
		}
	}
	return out
}
