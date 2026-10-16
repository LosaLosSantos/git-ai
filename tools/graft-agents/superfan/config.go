package superfan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/notify"
)

// UnassignedComponent names the digest of changesets that touch no component.
const UnassignedComponent = "unassigned"

// AnyComponent, as a route's component, matches every component including
// UnassignedComponent.
const AnyComponent = "*"

// DefaultWindow is the digest window when none is configured.
const DefaultWindow = time.Hour

// Config is the superfan configuration. It is a JSON document; see doc.go.
type Config struct {
	// Window is the digest window, a duration such as "1h" or "15m".
	Window     string            `json:"window,omitempty"`
	Components []ComponentConfig `json:"components,omitempty"`
	Routes     []RouteConfig     `json:"routes,omitempty"`
	Notifiers  []NotifierConfig  `json:"notifiers,omitempty"`
	Rules      RulesConfig       `json:"rules,omitempty"`
	Summarizer SummarizerConfig  `json:"summarizer,omitempty"`
	Digest     DigestConfig      `json:"digest,omitempty"`
	// References are reference globs such as "refs/heads/*". When set, events
	// of other references are skipped: they are neither analyzed nor fetched.
	// Ref moves of verification refs, for example, can be left out this way.
	References []string `json:"references,omitempty"`
	// Cursor is the cursor file. The command line may override it.
	Cursor string `json:"cursor,omitempty"`
}

// DigestConfig shapes the digests.
type DigestConfig struct {
	// IncludeQuiet also sends the digests of windows in which a component had
	// changesets but no finding. Off by default: most windows are quiet.
	IncludeQuiet bool `json:"include_quiet,omitempty"`
}

// ComponentConfig maps path globs to a component name.
type ComponentConfig struct {
	Name  string   `json:"name"`
	Paths []string `json:"paths"`
}

// RouteConfig sends the digests of a component to notifiers.
type RouteConfig struct {
	// Component is a component name, UnassignedComponent or AnyComponent.
	Component string   `json:"component"`
	Notifiers []string `json:"notifiers"`
}

// NotifierConfig configures one destination.
type NotifierConfig struct {
	Name string `json:"name"`
	// Type is "stdout", "webhook" or "slack".
	Type string `json:"type"`
	URL  string `json:"url,omitempty"`
	// TokenFile holds the bearer token of a webhook. The file is read once.
	TokenFile string `json:"token_file,omitempty"`
	// Timeout is a duration such as "10s"; empty selects notify.DefaultTimeout.
	Timeout string `json:"timeout,omitempty"`
}

// RulesConfig enables the deterministic rules. A nil or empty rule is off.
type RulesConfig struct {
	LargeChange *LargeChangeRule `json:"large_change,omitempty"`
	// ProtectedPaths are globs; a changeset touching one is flagged.
	ProtectedPaths []string   `json:"protected_paths,omitempty"`
	Burst          *BurstRule `json:"burst,omitempty"`
	// ExpectedActors are actor globs. When set, any other actor is flagged.
	ExpectedActors []string `json:"expected_actors,omitempty"`
}

// LargeChangeRule flags changesets above a file or line count. A zero limit is off.
type LargeChangeRule struct {
	MaxFiles int `json:"max_files,omitempty"`
	MaxLines int `json:"max_lines,omitempty"`
}

// BurstRule flags the moment when Lands events of Operation happen within Window.
type BurstRule struct {
	Lands  int    `json:"lands"`
	Window string `json:"window"`
	// Operation is the event operation that counts; default "land". Every member
	// of a batched land counts as one operation, and with "land" the members of
	// "land_batch" events count too: batching folds concurrent lands, which are
	// what a burst is made of, into batched lands.
	Operation string `json:"operation,omitempty"`
}

// SummarizerConfig selects the summarizer. The default is "none".
type SummarizerConfig struct {
	Type    string          `json:"type,omitempty"`
	Options json.RawMessage `json:"options,omitempty"`
}

const (
	maxComponents    = 1000
	maxNotifiers     = 100
	maxRouteTargets  = 100
	maxLandsPerRule  = 100000
	maxReferences    = 100
	minWindow        = time.Second
	maxWindow        = 7 * 24 * time.Hour
	defaultOperation = OperationLand
)

var nameRegexp = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ParseConfig decodes a configuration document. Unknown fields are an error,
// so that a misspelled key cannot silently disable a rule.
func ParseConfig(data []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("superfan config: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("superfan config: trailing data after the document")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the configuration. It does not read token files.
func (c *Config) Validate() error {
	if _, err := c.windowDuration(); err != nil {
		return err
	}
	if err := c.validateComponents(); err != nil {
		return err
	}
	notifiers, err := c.validateNotifiers()
	if err != nil {
		return err
	}
	if err := c.validateRoutes(notifiers); err != nil {
		return err
	}
	if err := c.Rules.validate(); err != nil {
		return err
	}
	if len(c.References) > maxReferences {
		return fmt.Errorf("superfan config: more than %d references", maxReferences)
	}
	for _, r := range c.References {
		if err := ValidGlob(r); err != nil {
			return fmt.Errorf("superfan config: reference %q: %w", r, err)
		}
	}
	typ := c.Summarizer.Type
	if typ == "" {
		typ = summarizerNone
	}
	if !isSummarizerRegistered(typ) {
		return fmt.Errorf("superfan config: unknown summarizer %q (available: %s)", typ, strings.Join(SummarizerTypes(), ", "))
	}
	return nil
}

func (c *Config) windowDuration() (time.Duration, error) {
	if c.Window == "" {
		return DefaultWindow, nil
	}
	d, err := time.ParseDuration(c.Window)
	if err != nil {
		return 0, fmt.Errorf("superfan config: invalid window %q", c.Window)
	}
	if d < minWindow || d > maxWindow {
		return 0, fmt.Errorf("superfan config: window %s must be between %s and %s", d, minWindow, maxWindow)
	}
	return d, nil
}

// WindowDuration returns the digest window. It is valid after Validate.
func (c *Config) WindowDuration() time.Duration {
	d, err := c.windowDuration()
	if err != nil {
		return DefaultWindow
	}
	return d
}

func (c *Config) validateComponents() error {
	if len(c.Components) > maxComponents {
		return fmt.Errorf("superfan config: more than %d components", maxComponents)
	}
	seen := map[string]bool{}
	for _, comp := range c.Components {
		if !nameRegexp.MatchString(comp.Name) {
			return fmt.Errorf("superfan config: invalid component name %q", comp.Name)
		}
		if comp.Name == UnassignedComponent {
			return fmt.Errorf("superfan config: component name %q is reserved", comp.Name)
		}
		if seen[comp.Name] {
			return fmt.Errorf("superfan config: duplicate component %q", comp.Name)
		}
		seen[comp.Name] = true
		if len(comp.Paths) == 0 {
			return fmt.Errorf("superfan config: component %q has no paths", comp.Name)
		}
		for _, p := range comp.Paths {
			if err := ValidGlob(p); err != nil {
				return fmt.Errorf("superfan config: component %q path %q: %w", comp.Name, p, err)
			}
		}
	}
	return nil
}

// validateNotifiers checks the notifier list and returns the set of names.
func (c *Config) validateNotifiers() (map[string]bool, error) {
	if len(c.Notifiers) > maxNotifiers {
		return nil, fmt.Errorf("superfan config: more than %d notifiers", maxNotifiers)
	}
	names := map[string]bool{}
	for _, n := range c.Notifiers {
		if !nameRegexp.MatchString(n.Name) {
			return nil, fmt.Errorf("superfan config: invalid notifier name %q", n.Name)
		}
		if names[n.Name] {
			return nil, fmt.Errorf("superfan config: duplicate notifier %q", n.Name)
		}
		names[n.Name] = true
		if n.Timeout != "" {
			d, err := time.ParseDuration(n.Timeout)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("superfan config: notifier %q: invalid timeout %q", n.Name, n.Timeout)
			}
		}
		switch n.Type {
		case "stdout":
			if n.URL != "" || n.TokenFile != "" {
				return nil, fmt.Errorf("superfan config: stdout notifier %q takes no url or token_file", n.Name)
			}
		case "webhook", "slack":
			if n.Type == "slack" && n.TokenFile != "" {
				return nil, fmt.Errorf("superfan config: slack notifier %q takes no token_file", n.Name)
			}
			if err := validateNotifierURL(n.URL); err != nil {
				return nil, fmt.Errorf("superfan config: notifier %q: %w", n.Name, err)
			}
		default:
			return nil, fmt.Errorf("superfan config: notifier %q: unknown type %q (stdout, webhook, slack)", n.Name, n.Type)
		}
	}
	return names, nil
}

// validateNotifierURL checks the endpoint of a notifier without echoing it.
func validateNotifierURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	return notify.ValidateEndpoint(raw)
}

func (c *Config) validateRoutes(notifiers map[string]bool) error {
	components := map[string]bool{UnassignedComponent: true, AnyComponent: true}
	for _, comp := range c.Components {
		components[comp.Name] = true
	}
	for _, r := range c.Routes {
		if !components[r.Component] {
			return fmt.Errorf("superfan config: route to unknown component %q", r.Component)
		}
		if len(r.Notifiers) == 0 || len(r.Notifiers) > maxRouteTargets {
			return fmt.Errorf("superfan config: route to %q needs 1 to %d notifiers", r.Component, maxRouteTargets)
		}
		for _, n := range r.Notifiers {
			if !notifiers[n] {
				return fmt.Errorf("superfan config: route to %q uses unknown notifier %q", r.Component, n)
			}
		}
	}
	return nil
}

func (r RulesConfig) validate() error {
	if lc := r.LargeChange; lc != nil {
		if lc.MaxFiles < 0 || lc.MaxLines < 0 {
			return errors.New("superfan config: large_change limits must not be negative")
		}
		if lc.MaxFiles == 0 && lc.MaxLines == 0 {
			return errors.New("superfan config: large_change needs max_files or max_lines")
		}
	}
	for _, p := range r.ProtectedPaths {
		if err := ValidGlob(p); err != nil {
			return fmt.Errorf("superfan config: protected path %q: %w", p, err)
		}
	}
	if b := r.Burst; b != nil {
		if b.Lands < 2 || b.Lands > maxLandsPerRule {
			return fmt.Errorf("superfan config: burst lands must be between 2 and %d", maxLandsPerRule)
		}
		d, err := time.ParseDuration(b.Window)
		if err != nil || d <= 0 || d > maxWindow {
			return fmt.Errorf("superfan config: invalid burst window %q", b.Window)
		}
	}
	for _, a := range r.ExpectedActors {
		if err := ValidGlob(a); err != nil {
			return fmt.Errorf("superfan config: expected actor %q: %w", a, err)
		}
	}
	return nil
}

// BuildNotifiers builds the notifiers of the configuration, keyed by name.
// Stdout notifiers write to stdout. Token files are read here.
func (c *Config) BuildNotifiers(stdout io.Writer) (map[string]notify.Notifier, error) {
	out := map[string]notify.Notifier{}
	for _, n := range c.Notifiers {
		var timeout time.Duration
		if n.Timeout != "" {
			timeout, _ = time.ParseDuration(n.Timeout)
		}
		switch n.Type {
		case "stdout":
			out[n.Name] = notify.NewStdout(n.Name, stdout)
		case "webhook":
			token := ""
			if n.TokenFile != "" {
				t, err := readToken(n.TokenFile)
				if err != nil {
					return nil, fmt.Errorf("superfan config: notifier %q: %w", n.Name, err)
				}
				token = t
			}
			w, err := notify.NewWebhook(notify.WebhookOptions{Name: n.Name, URL: n.URL, Token: token, Timeout: timeout})
			if err != nil {
				return nil, fmt.Errorf("superfan config: notifier %q: %w", n.Name, err)
			}
			out[n.Name] = w
		case "slack":
			s, err := notify.NewSlack(notify.WebhookOptions{Name: n.Name, URL: n.URL, Timeout: timeout})
			if err != nil {
				return nil, fmt.Errorf("superfan config: notifier %q: %w", n.Name, err)
			}
			out[n.Name] = s
		default:
			return nil, fmt.Errorf("superfan config: notifier %q: unknown type %q", n.Name, n.Type)
		}
	}
	return out, nil
}

// readToken reads a bearer token file. The token is never part of an error.
func readToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("cannot read token file")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("token file is empty")
	}
	return token, nil
}
