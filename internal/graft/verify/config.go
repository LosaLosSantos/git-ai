package verify

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

const (
	// DefaultStorage is the storage used when the configuration names none.
	DefaultStorage = "default"
	// DefaultBranch is the branch verified when the configuration names none.
	DefaultBranch = "main"
	// DefaultBatchWindow is how long the verifier waits after the first land
	// of a batch before it tests the tip, so that a burst of lands is tested
	// once.
	DefaultBatchWindow = 30 * time.Second
	// DefaultTestTimeout bounds one run of the test command.
	DefaultTestTimeout = 10 * time.Minute
	// greenRefPrefix is the namespace of green refs. It is outside refs/heads
	// and refs/tags so that the verifier never writes a branch or a tag.
	greenRefPrefix = "refs/graft/green/"
	// treeDirName is the directory, inside work_dir, that holds the
	// materialized tree.
	treeDirName = "tree"
	// markerName marks work_dir as owned by graft-verify.
	markerName = ".graft-verify"
)

// Duration is a time.Duration that is written as a string such as "30s" in
// the configuration file.
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// Std returns the duration as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config is the configuration of graft-verify, read from a TOML file.
type Config struct {
	// Address is the Gitaly or Graft server address, such as
	// "unix:/run/gitaly.sock" or "tcp://host:port".
	Address string `toml:"address"`
	// Token is the server's shared token. Prefer TokenFile.
	Token string `toml:"token"`
	// TokenFile is a file holding the shared token. Its content is trimmed.
	TokenFile string `toml:"token_file"`
	// Storage is the storage that holds the repository.
	Storage string `toml:"storage"`
	// Repository is the repository path relative to the storage.
	Repository string `toml:"repository"`
	// Branch is the branch to verify, without the refs/heads/ prefix.
	Branch string `toml:"branch"`
	// GreenRef is the reference that records the last green tip. It must be
	// under refs/graft/green/. The default is refs/graft/green/<branch>.
	GreenRef string `toml:"green_ref"`
	// WorkDir is a directory dedicated to graft-verify. It is created when
	// missing and must otherwise be empty or hold a previous graft-verify
	// work directory.
	WorkDir string `toml:"work_dir"`
	// StateFile is the JSON file that records the changeset cursor.
	StateFile string `toml:"state_file"`
	// Sparse lists the path prefixes that are materialized. Empty means the
	// whole repository.
	Sparse []string `toml:"sparse"`
	// BatchWindow is how long lands are collected before the tip is tested.
	// Unset or zero means DefaultBatchWindow.
	BatchWindow Duration     `toml:"batch_window"`
	Test        TestConfig   `toml:"test"`
	Revert      RevertConfig `toml:"revert"`
}

// TestConfig configures the test command run on the materialized tree.
type TestConfig struct {
	// Command is the argv of the test command. It runs with the materialized
	// tree as its working directory.
	Command []string `toml:"command"`
	// Env lists extra KEY=VALUE environment entries for the command. The
	// process environment is inherited, and GRAFT_VERIFY_COMMIT is always set
	// to the commit under test.
	Env []string `toml:"env"`
	// Timeout bounds one run of the command.
	Timeout Duration `toml:"timeout"`
}

// RevertConfig configures automatic reverts of culprits (see the bisector).
type RevertConfig struct {
	// Enabled turns automatic reverts on. Without it a failing tip is only
	// reported.
	Enabled bool `toml:"enabled"`
	// DryRun reports the revert that would be made without making it.
	DryRun bool `toml:"dry_run"`
	// MaxPerHour caps the reverts made in one hour. Zero means no reverts.
	MaxPerHour int `toml:"max_per_hour"`
	// WebhookURL receives a JSON notification for every revert decision.
	WebhookURL string `toml:"webhook_url"`
}

// LoadConfig reads and validates the configuration file at path. Unknown keys
// are an error, so that a misspelled key does not silently change behavior.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return ParseConfig(data)
}

// ParseConfig parses and validates configuration data.
func ParseConfig(data []byte) (Config, error) {
	var cfg Config
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Finalize(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Finalize fills in defaults, reads the token file and validates the
// configuration. It is called by ParseConfig and may be called on a Config
// built in code.
func (c *Config) Finalize() error {
	if c.Storage == "" {
		c.Storage = DefaultStorage
	}
	if c.Branch == "" {
		c.Branch = DefaultBranch
	}
	if c.GreenRef == "" {
		c.GreenRef = greenRefPrefix + c.Branch
	}
	if c.BatchWindow == 0 {
		c.BatchWindow = Duration(DefaultBatchWindow)
	}
	if c.Test.Timeout == 0 {
		c.Test.Timeout = Duration(DefaultTestTimeout)
	}

	if c.Token != "" && c.TokenFile != "" {
		return errors.New("config: token and token_file are mutually exclusive")
	}
	if c.TokenFile != "" {
		data, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return fmt.Errorf("config: read token_file: %w", err)
		}
		c.Token = strings.TrimSpace(string(data))
	}

	return c.Validate()
}

// Validate checks the configuration. Finalize calls it after the defaults are
// filled in.
func (c Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("config: "+format, args...))
	}

	if c.Address == "" {
		fail("address is required")
	}
	if c.Repository == "" {
		fail("repository is required")
	}
	if !validRefName(c.Branch) || strings.HasPrefix(c.Branch, "-") {
		fail("branch %q is not a valid branch name", c.Branch)
	}
	if !validGreenRef(c.GreenRef) {
		fail("green_ref %q must be a valid reference under %s", c.GreenRef, greenRefPrefix)
	}
	if c.WorkDir == "" {
		fail("work_dir is required")
	}
	if c.StateFile == "" {
		fail("state_file is required")
	}
	if c.BatchWindow < 0 {
		fail("batch_window must not be negative")
	}
	if c.Test.Timeout <= 0 {
		fail("test.timeout must be positive")
	}
	if len(c.Test.Command) == 0 || c.Test.Command[0] == "" {
		fail("test.command is required")
	}
	for _, kv := range c.Test.Env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || key == "" || strings.ContainsRune(key, 0) {
			fail("test.env entry %q must be KEY=VALUE", kv)
		}
	}
	for _, prefix := range c.Sparse {
		if prefix == "" {
			fail("sparse entries must not be empty")
		}
	}
	if c.Revert.MaxPerHour < 0 {
		fail("revert.max_per_hour must not be negative")
	}

	return errors.Join(errs...)
}

// validRefName reports whether name is usable as a branch name: a
// slash-separated path of valid reference components.
func validRefName(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.Contains(name, "//") || strings.Contains(name, "..") ||
		strings.HasSuffix(name, ".") || strings.Contains(name, "@{") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// validGreenRef reports whether ref is a valid reference under the green
// namespace.
func validGreenRef(ref string) bool {
	return strings.HasPrefix(ref, greenRefPrefix) &&
		len(ref) > len(greenRefPrefix) &&
		validRefName(ref)
}
