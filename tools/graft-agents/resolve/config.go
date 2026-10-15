package resolve

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"time"
)

// Config is the graft-resolve configuration file (TOML):
//
//	timeout = "5m"                       # optional, whole request
//
//	[[strategy]]                         # ordered; tried first to last
//	type = "trivial"
//	name = "trivial"                     # optional, defaults to type
//
//	[[strategy]]
//	type = "union"
//	paths = ["CHANGELOG.md"]
//
//	[[strategy]]
//	type = "rerere"
//	dir = "/var/lib/graft-resolve/rerere"
//
//	[[strategy]]
//	type = "command"
//	command = ["git", "merge-file", "-p", "{ours}", "{base}", "{theirs}"]
//	output = "stdout"                    # the tool's stdout is the result
//	timeout = "60s"
//	env = ["LANG"]                       # optional extra environment variables
//
//	[[check]]                            # every check whose paths match must pass
//	paths = ["**/*.go"]
//	command = ["sh", "-c", 'out=$(gofmt -l "$1") && test -z "$out"', "sh", "{file}"]
//	timeout = "30s"
//
// Strategy "command" writes its result to {out} by default (output = "file")
// and must create it; see the strategy documentation. Checks and command
// strategies see only a minimal environment (PATH, HOME, temporary directory,
// locale) plus the variables named in their env list, so provider keys in the
// environment of graft-resolve do not reach agent-influenced programs.
//
// Unknown top-level keys and unknown strategy options are errors, so typos
// fail loudly.
type Config struct {
	Timeout    time.Duration
	Strategies []StrategySpec
	Checks     []Check
}

// DefaultConfig is used when no configuration file is given: the trivial
// strategy only.
func DefaultConfig() Config {
	return Config{
		Timeout:    DefaultResolveTimeout,
		Strategies: []StrategySpec{{Name: "trivial", Type: "trivial"}},
	}
}

// ParseConfig parses a configuration file.
func ParseConfig(data []byte) (*Config, error) {
	tbl, err := parseTOML(string(data))
	if err != nil {
		return nil, err
	}
	var unknown []string
	for k := range tbl {
		switch k {
		case "timeout", "strategy", "check":
		default:
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("config: unknown keys %v", unknown)
	}

	cfg := &Config{Timeout: DefaultResolveTimeout}
	if v, ok := tbl["timeout"]; ok {
		d, err := durationValue(v, "timeout")
		if err != nil {
			return nil, err
		}
		cfg.Timeout = d
	}
	if v, ok := tbl["strategy"]; ok {
		items, ok := v.([]map[string]any)
		if !ok {
			return nil, errors.New(`config: "strategy" must be an array of tables ([[strategy]])`)
		}
		for i, item := range items {
			spec, err := decodeStrategy(item)
			if err != nil {
				return nil, fmt.Errorf("config: strategy %d: %w", i+1, err)
			}
			cfg.Strategies = append(cfg.Strategies, spec)
		}
	}
	if v, ok := tbl["check"]; ok {
		items, ok := v.([]map[string]any)
		if !ok {
			return nil, errors.New(`config: "check" must be an array of tables ([[check]])`)
		}
		for i, item := range items {
			c, err := decodeCheck(item)
			if err != nil {
				return nil, fmt.Errorf("config: check %d: %w", i+1, err)
			}
			cfg.Checks = append(cfg.Checks, c)
		}
	}
	return cfg, nil
}

// LoadConfig reads and parses a configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return ParseConfig(data)
}

// Build instantiates the strategies through the registry and returns the
// resolver.
func (c *Config) Build() (*Resolver, error) {
	if len(c.Strategies) == 0 {
		return nil, errors.New("config: no [[strategy]] configured")
	}
	bindings := make([]Binding, 0, len(c.Strategies))
	for _, spec := range c.Strategies {
		factory, ok := lookupStrategy(spec.Type)
		if !ok {
			return nil, fmt.Errorf("config: strategy %q: unknown type %q (available: %v)", spec.Name, spec.Type, StrategyTypes())
		}
		s, err := factory(spec)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		bindings = append(bindings, Binding{Name: spec.Name, Strategy: s, Paths: spec.Paths})
	}
	r, err := NewResolver(bindings, c.Checks, c.Timeout)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return r, nil
}

func decodeStrategy(item map[string]any) (StrategySpec, error) {
	typ, err := stringField(item, "type")
	if err != nil {
		return StrategySpec{}, err
	}
	if typ == "" {
		return StrategySpec{}, errors.New(`"type" is required`)
	}
	name, err := stringField(item, "name")
	if err != nil {
		return StrategySpec{}, err
	}
	if name == "" {
		name = typ
	}
	if !validName(name) {
		return StrategySpec{}, fmt.Errorf("invalid name %q", name)
	}
	paths, err := stringListField(item, "paths")
	if err != nil {
		return StrategySpec{}, err
	}
	if err := validatePatterns(paths); err != nil {
		return StrategySpec{}, err
	}
	opts := map[string]any{}
	for k, v := range item {
		switch k {
		case "type", "name", "paths":
		default:
			opts[k] = v
		}
	}
	return StrategySpec{Name: name, Type: typ, Paths: paths, Options: opts}, nil
}

func decodeCheck(item map[string]any) (Check, error) {
	var c Check
	var err error
	for k := range item {
		switch k {
		case "paths", "command", "timeout", "env":
		default:
			return Check{}, fmt.Errorf("unknown key %q", k)
		}
	}
	if c.Paths, err = stringListField(item, "paths"); err != nil {
		return Check{}, err
	}
	if len(c.Paths) == 0 {
		return Check{}, errors.New(`"paths" is required`)
	}
	if err := validatePatterns(c.Paths); err != nil {
		return Check{}, err
	}
	if c.Command, err = stringListField(item, "command"); err != nil {
		return Check{}, err
	}
	if len(c.Command) == 0 || c.Command[0] == "" {
		return Check{}, errors.New(`"command" is required`)
	}
	if v, ok := item["timeout"]; ok {
		if c.Timeout, err = durationValue(v, "timeout"); err != nil {
			return Check{}, err
		}
	}
	if c.Env, err = stringListField(item, "env"); err != nil {
		return Check{}, err
	}
	if err := validateEnvNames(c.Env); err != nil {
		return Check{}, err
	}
	return c, nil
}

func stringField(item map[string]any, key string) (string, error) {
	v, ok := item[key]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%q must be a string", key)
	}
	return s, nil
}

func stringListField(item map[string]any, key string) ([]string, error) {
	v, ok := item[key]
	if !ok {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%q must be a list of strings", key)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("%q must be a list of strings", key)
		}
		out = append(out, s)
	}
	return out, nil
}

func durationValue(v any, key string) (time.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("%q must be a duration string", key)
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q: invalid duration %q", key, s)
	}
	return d, nil
}
