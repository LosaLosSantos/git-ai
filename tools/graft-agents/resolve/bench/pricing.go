package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/resolve"
)

// DefaultOpenRouterAPI is the base of OpenRouter's API.
const DefaultOpenRouterAPI = "https://openrouter.ai/api/v1"

// Pricing is the price of the measured model, as read at run time (never from memory, PLAN10 12):
// the highest price of each kind over the model's endpoints, in USD per token, since OpenRouter
// may route a call to any of them.
type Pricing struct {
	Model     string    `json:"model"`
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
	Endpoints int       `json:"endpoints"`
	// Prompt, Completion and Reasoning are per token; Request is per call.
	Prompt     float64 `json:"prompt_usd_per_token"`
	Completion float64 `json:"completion_usd_per_token"`
	Reasoning  float64 `json:"internal_reasoning_usd_per_token"`
	Request    float64 `json:"request_usd"`
}

// Prices returns the budget prices of p. Every output token is priced at the completion price
// plus the internal-reasoning price: a reasoning token is reported as output, and charging both
// prices on every output token can only overstate the cost.
func (p Pricing) Prices() resolve.Prices {
	return resolve.Prices{
		InputPerMillion:  p.Prompt * 1e6,
		OutputPerMillion: (p.Completion + p.Reasoning) * 1e6,
		PerRequest:       p.Request,
	}
}

type openRouterEndpoints struct {
	Data struct {
		ID        string `json:"id"`
		Endpoints []struct {
			Name    string                     `json:"name"`
			Pricing map[string]json.RawMessage `json:"pricing"`
		} `json:"endpoints"`
	} `json:"data"`
}

// openRouterPrice reads one price, given as a JSON string (OpenRouter's usual form) or number.
func openRouterPrice(raw json.RawMessage) (float64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strconv.ParseFloat(s, 64)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, fmt.Errorf("%s is neither a string nor a number", raw)
	}
	return f, nil
}

// OpenRouterPricing reads the prices of model from OpenRouter's endpoints API
// (GET <api>/models/<model>/endpoints, public). It fails when the model is unknown, has no
// endpoint, or an endpoint lacks a prompt or completion price, or lists one that is not a
// non-negative number: the run must not start on a price it cannot read.
//
// maxPromptTokens is an upper bound of the prompt of one call; an override tier whose
// min_prompt_tokens is above it is left out, since it cannot apply. 0 counts every tier.
func OpenRouterPricing(ctx context.Context, client *http.Client, api, model string, maxPromptTokens int) (Pricing, error) {
	parts := strings.Split(model, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Pricing{}, fmt.Errorf("pricing: model %q is not author/slug", model)
	}
	source := strings.TrimRight(api, "/") + "/models/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/endpoints"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return Pricing{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Pricing{}, fmt.Errorf("pricing: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Pricing{}, fmt.Errorf("pricing: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Pricing{}, fmt.Errorf("pricing: %s: HTTP %d: %.200s", source, resp.StatusCode, body)
	}
	var doc openRouterEndpoints
	if err := json.Unmarshal(body, &doc); err != nil {
		return Pricing{}, fmt.Errorf("pricing: %s: %w", source, err)
	}
	if len(doc.Data.Endpoints) == 0 {
		return Pricing{}, fmt.Errorf("pricing: %s lists no endpoint for %s", source, model)
	}

	// The prices used are prompt, completion, internal_reasoning and request. "overrides" lists
	// tiers (for very long prompts, say) with prices of their own: those that can apply count
	// towards the highest price too. A discount is ignored, which can only overstate the cost, and so are the prices of
	// what the run never uses (web search, images, cache reads and writes).
	p := Pricing{Model: model, Source: source, FetchedAt: time.Now().UTC(), Endpoints: len(doc.Data.Endpoints)}
	take := func(endpoint string, prices map[string]json.RawMessage, required bool) error {
		for _, f := range []struct {
			key      string
			required bool
			dst      *float64
		}{
			{"prompt", required, &p.Prompt},
			{"completion", required, &p.Completion},
			{"internal_reasoning", false, &p.Reasoning},
			{"request", false, &p.Request},
		} {
			raw, ok := prices[f.key]
			if !ok {
				if f.required {
					return fmt.Errorf("pricing: endpoint %q of %s has no %s price", endpoint, model, f.key)
				}
				continue
			}
			v, err := openRouterPrice(raw)
			if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("pricing: endpoint %q of %s has the %s price %s, not a non-negative number", endpoint, model, f.key, raw)
			}
			*f.dst = math.Max(*f.dst, v)
		}
		return nil
	}
	for _, e := range doc.Data.Endpoints {
		if err := take(e.Name, e.Pricing, true); err != nil {
			return Pricing{}, err
		}
		if raw, ok := e.Pricing["overrides"]; ok {
			var tiers []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &tiers); err != nil {
				return Pricing{}, fmt.Errorf("pricing: endpoint %q of %s: overrides: %w", e.Name, model, err)
			}
			for _, tier := range tiers {
				// A tier that starts above the largest possible prompt never applies.
				if raw, ok := tier["min_prompt_tokens"]; ok && maxPromptTokens > 0 {
					min, err := openRouterPrice(raw)
					if err != nil {
						return Pricing{}, fmt.Errorf("pricing: endpoint %q of %s: min_prompt_tokens %s: %w", e.Name, model, raw, err)
					}
					if min > float64(maxPromptTokens) {
						continue
					}
				}
				if err := take(e.Name+" (override)", tier, false); err != nil {
					return Pricing{}, err
				}
			}
		}
	}
	if p.Prompt == 0 && p.Completion == 0 {
		return Pricing{}, errors.New("pricing: " + model + " is listed as free; refusing to measure on a price of zero")
	}
	return p, nil
}
