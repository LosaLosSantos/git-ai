package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/feder-cr/open_scion/tools/graft-agents/llm"
)

const (
	// DefaultMaxContextBytes bounds the context sent to the model.
	DefaultMaxContextBytes = 64 << 10
	minContextBytes        = 1 << 10
	maxContextBytesLimit   = 1 << 20
	// DefaultLogTailLines is how many lines of each log file are tailed.
	DefaultLogTailLines = 200
	maxLogTailLines     = 2000
	maxLogFiles         = 20
	maxRedactPatterns   = 50
	// maxQuestionBytes bounds a question from Slack or the command line.
	maxQuestionBytes = 1 << 10

	defaultSigningSecretEnv = "GRAFT_SUPERFAN_SLACK_SIGNING_SECRET"
	defaultBotTokenEnv      = "GRAFT_SUPERFAN_SLACK_BOT_TOKEN"
	defaultSlackAPIBase     = "https://slack.com/api"
)

// Config is the chat configuration, a JSON document. Unknown fields are an
// error, so that a misspelled key cannot silently disable a source.
type Config struct {
	// Sources selects the sources of context. Unset sources are not read.
	Sources SourcesConfig `json:"sources,omitempty"`
	// Provider is the language model (llm.Config). Type "none" or empty answers
	// with a deterministic summary of the sources.
	Provider llm.Config `json:"provider,omitempty"`
	// Slack configures the Slack front end.
	Slack SlackConfig `json:"slack,omitempty"`
	// MaxContextBytes bounds the context of one answer. Zero means
	// DefaultMaxContextBytes.
	MaxContextBytes int `json:"max_context_bytes,omitempty"`
	// Redact lists extra regular expressions whose matches are replaced in all
	// context and answers, on top of the built-in secret patterns.
	Redact []string `json:"redact,omitempty"`
}

// SourcesConfig configures where the context comes from.
type SourcesConfig struct {
	// Changesets is the changeset log, usually `graft log --json -n 50`.
	Changesets ChangesetsConfig `json:"changesets,omitempty"`
	// VerifyState is the state file of graft-verify (JSON or text).
	VerifyState string `json:"verify_state,omitempty"`
	// MetricsURL is a Prometheus endpoint; only graft_ lines are kept.
	MetricsURL string `json:"metrics_url,omitempty"`
	// LogFiles are tailed; the last LogTailLines lines of each are used.
	LogFiles     []string `json:"log_files,omitempty"`
	LogTailLines int      `json:"log_tail_lines,omitempty"`
}

// ChangesetsConfig is either a command run without a shell in Dir (argv[0] is
// the graft binary) or a file holding the output of such a command.
type ChangesetsConfig struct {
	Argv []string `json:"argv,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	File string   `json:"file,omitempty"`
}

// SlackConfig names the environment variables that hold the Slack secrets and
// the Slack Web API base URL. The secrets never appear in the configuration.
type SlackConfig struct {
	// SigningSecretEnv names the variable holding the signing secret.
	SigningSecretEnv string `json:"signing_secret_env,omitempty"`
	// BotTokenEnv names the variable holding the bot token (xoxb-...), used for
	// chat.postMessage when answering app_mention events.
	BotTokenEnv string `json:"bot_token_env,omitempty"`
	// APIBase is the Slack Web API base URL; empty means https://slack.com/api.
	APIBase string `json:"api_base,omitempty"`
}

// ParseConfig decodes and validates a configuration document.
func ParseConfig(data []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("chat config: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("chat config: trailing data after the document")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadConfig reads and validates the configuration file at path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("chat config: read: %w", err)
	}
	return ParseConfig(data)
}

// Validate checks the configuration. It does not read any source or secret.
func (c *Config) Validate() error {
	if c.MaxContextBytes != 0 && (c.MaxContextBytes < minContextBytes || c.MaxContextBytes > maxContextBytesLimit) {
		return fmt.Errorf("chat config: max_context_bytes must be 0 or between %d and %d", minContextBytes, maxContextBytesLimit)
	}
	if len(c.Redact) > maxRedactPatterns {
		return fmt.Errorf("chat config: more than %d redact patterns", maxRedactPatterns)
	}
	if _, err := newRedactor(c.Redact); err != nil {
		return err
	}

	s := c.Sources
	if s.LogTailLines < 0 || s.LogTailLines > maxLogTailLines {
		return fmt.Errorf("chat config: log_tail_lines must be between 0 and %d", maxLogTailLines)
	}
	if len(s.LogFiles) > maxLogFiles {
		return fmt.Errorf("chat config: more than %d log files", maxLogFiles)
	}
	for _, f := range s.LogFiles {
		if f == "" {
			return errors.New("chat config: empty log file path")
		}
	}
	cs := s.Changesets
	switch {
	case len(cs.Argv) > 0 && cs.File != "":
		return errors.New("chat config: changesets argv and file are exclusive")
	case len(cs.Argv) > 0 && cs.Argv[0] == "":
		return errors.New("chat config: changesets argv has an empty program")
	case len(cs.Argv) == 0 && cs.Dir != "":
		return errors.New("chat config: changesets dir needs argv")
	}
	if s.MetricsURL != "" {
		if err := validHTTPURL(s.MetricsURL); err != nil {
			return fmt.Errorf("chat config: metrics_url: %w", err)
		}
	}
	if c.Slack.APIBase != "" {
		if err := validHTTPURL(c.Slack.APIBase); err != nil {
			return fmt.Errorf("chat config: slack api_base: %w", err)
		}
	}
	if _, err := c.Provider.TimeoutOrDefault(); err != nil {
		return fmt.Errorf("chat config: provider: %w", err)
	}
	return nil
}

// validHTTPURL requires an absolute http or https URL. The error never echoes
// the URL, which may carry credentials.
func validHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an absolute http or https URL")
	}
	return nil
}

// contextBudget returns the context bound of one answer.
func (c *Config) contextBudget() int {
	if c.MaxContextBytes == 0 {
		return DefaultMaxContextBytes
	}
	return c.MaxContextBytes
}

// tailCount returns how many lines of each log file are tailed.
func (s SourcesConfig) tailCount() int {
	if s.LogTailLines == 0 {
		return DefaultLogTailLines
	}
	return s.LogTailLines
}

// SlackAPIBase returns the Slack Web API base URL without a trailing slash.
func (c *Config) SlackAPIBase() string {
	if c.Slack.APIBase == "" {
		return defaultSlackAPIBase
	}
	return strings.TrimRight(c.Slack.APIBase, "/")
}

func (c *Config) signingSecretEnv() string {
	if c.Slack.SigningSecretEnv == "" {
		return defaultSigningSecretEnv
	}
	return c.Slack.SigningSecretEnv
}

func (c *Config) botTokenEnv() string {
	if c.Slack.BotTokenEnv == "" {
		return defaultBotTokenEnv
	}
	return c.Slack.BotTokenEnv
}

// LoadSlackSecrets reads the Slack signing secret and bot token from the
// environment variables the configuration names (getenv is os.Getenv in
// production). The signing secret is required; the bot token may be empty,
// which disables app_mention replies. Values never appear in errors.
func LoadSlackSecrets(c *Config, getenv func(string) string) (signingSecret, botToken string, err error) {
	signingSecret = getenv(c.signingSecretEnv())
	if signingSecret == "" {
		return "", "", fmt.Errorf("chat: environment variable %s with the Slack signing secret is not set", c.signingSecretEnv())
	}
	return signingSecret, getenv(c.botTokenEnv()), nil
}
