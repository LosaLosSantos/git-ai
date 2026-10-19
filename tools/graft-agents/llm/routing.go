package llm

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Routing is an OpenRouter provider-routing preference: the endpoints a call may be served by.
// The openai provider sends it as the request's "provider" object, with allow_fallbacks false so
// that a call can never escape the allowed set; every other provider rejects it.
//
// A name is a provider slug ("openai", "azure") or an endpoint tag ("openai/flex", "azure/eu"),
// as the tag field of OpenRouter's /models/<model>/endpoints lists them. Pricing a call from the
// endpoints (Allows) reads the names the way that can only overstate the cost: a slug in Only
// admits every endpoint of that provider, its tagged variants (such as a priority tier) included,
// while a name in Ignore excludes only the endpoint whose tag is exactly that name. So the budget
// is exact only when routing is pinned to the endpoints the run means to use, and it is never below
// the price of an endpoint the call can reach.
type Routing struct {
	// Only lists the providers or endpoints a call may use; empty allows every one not ignored.
	Only []string `toml:"only" json:"only"`
	// Ignore lists the providers or endpoints a call must not use.
	Ignore []string `toml:"ignore" json:"ignore"`
}

// routingNameRE is the form of an OpenRouter provider slug, optionally with an endpoint variant.
var routingNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(/[a-z0-9][a-z0-9.-]*)?$`)

// Validate checks that r names at least one provider or endpoint, each well-formed, once, and none
// both allowed and ignored. Whether a name is one of the model's endpoints is known only from
// OpenRouter's endpoint list (CheckNames).
func (r *Routing) Validate() error {
	if len(r.Only) == 0 && len(r.Ignore) == 0 {
		return fmt.Errorf("llm: routing names no provider: set only, ignore or both")
	}
	seen := map[string]string{}
	for _, list := range []struct {
		key   string
		names []string
	}{{"only", r.Only}, {"ignore", r.Ignore}} {
		for _, name := range list.names {
			if !routingNameRE.MatchString(name) {
				return fmt.Errorf("llm: routing %s: %q is not a provider slug or endpoint tag (such as openai or azure/eu)", list.key, name)
			}
			if prev, dup := seen[name]; dup {
				if prev == list.key {
					return fmt.Errorf("llm: routing %s: %q is listed twice", list.key, name)
				}
				return fmt.Errorf("llm: routing: %q is listed in both %s and %s", name, prev, list.key)
			}
			seen[name] = list.key
		}
	}
	return nil
}

// Allows reports whether a call routed by r may be served by the endpoint with tag, read the way
// that prices the call no lower than any endpoint it can reach (see Routing).
func (r *Routing) Allows(tag string) bool {
	if r == nil {
		return true
	}
	if slices.Contains(r.Ignore, tag) {
		return false
	}
	if len(r.Only) == 0 {
		return true
	}
	return slices.ContainsFunc(r.Only, func(name string) bool { return routingNameMatches(name, tag) })
}

// CheckNames checks every name of r against the tags of the model's endpoints: a name that matches
// none is unknown, most likely a typo, and an error, as is a preference that allows no endpoint.
func (r *Routing) CheckNames(tags []string) error {
	for _, name := range slices.Concat(r.Only, r.Ignore) {
		if !slices.ContainsFunc(tags, func(tag string) bool { return routingNameMatches(name, tag) }) {
			return fmt.Errorf("llm: routing names %q, which is no endpoint of the model (endpoints: %v)", name, tags)
		}
	}
	if !slices.ContainsFunc(tags, r.Allows) {
		return fmt.Errorf("llm: routing allows none of the model's endpoints %v", tags)
	}
	return nil
}

// routingNameMatches reports whether name designates the endpoint with tag: the tag itself, or,
// for a slug without a variant, any variant of that provider.
func routingNameMatches(name, tag string) bool {
	return name == tag || (!strings.Contains(name, "/") && strings.HasPrefix(tag, name+"/"))
}
