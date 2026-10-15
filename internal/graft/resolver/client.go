package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
)

// ErrDisabled is returned by Resolve on a nil (disabled) Client.
var ErrDisabled = errors.New("graft conflict resolver is disabled")

// ErrUnresolvable matches every UnresolvableError: the resolver refused the
// conflict (non-zero exit or HTTP 422).
var ErrUnresolvable = errors.New("conflict is unresolvable")

// ErrInvalidResponse matches every failure to accept the resolver's answer:
// invalid JSON, an output that exceeds its bound, or a violated acceptance
// rule of Validate.
var ErrInvalidResponse = errors.New("invalid resolver response")

// UnresolvableError is the resolver's refusal, with its reason (the command's
// stderr or the 422 error message).
type UnresolvableError struct {
	Reason string
}

// Error implements error.
func (e *UnresolvableError) Error() string {
	return "conflict is unresolvable: " + e.Reason
}

// Is makes errors.Is(err, ErrUnresolvable) match.
func (e *UnresolvableError) Is(target error) bool {
	return target == ErrUnresolvable
}

// Client calls the configured resolver. It is safe for concurrent use. A nil
// *Client is the disabled resolver.
type Client struct {
	command      []string
	url          string
	token        string
	timeout      time.Duration
	maxFiles     int
	maxFileBytes int64
	pathsAllow   []string
	pathsDeny    []string
	actors       []string
	httpClient   *http.Client
}

// New builds the resolver client from the configuration. It returns a nil
// client and no error when the resolver is disabled. For URL resolvers the
// token file is read once, here; a missing or empty token file is an error.
func New(cfg config.GraftResolver) (*Client, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	if (len(cfg.Command) == 0) == (cfg.URL == "") {
		return nil, errors.New("graft land resolver: exactly one of command and url must be set")
	}

	if len(cfg.Command) > 0 && cfg.Command[0] == "" {
		return nil, errors.New("graft land resolver: empty command")
	}

	timeout, err := cfg.TimeoutOrDefault()
	if err != nil {
		return nil, err
	}

	if cfg.MaxFiles < 0 || cfg.MaxFileBytes < 0 {
		return nil, errors.New("graft land resolver: max_files and max_file_bytes must not be negative")
	}

	c := &Client{
		command:      slices.Clone(cfg.Command),
		timeout:      timeout,
		maxFiles:     cfg.MaxFilesOrDefault(),
		maxFileBytes: cfg.MaxFileBytesOrDefault(),
		pathsAllow:   slices.Clone(cfg.PathsAllow),
		pathsDeny:    slices.Clone(cfg.PathsDeny),
		actors:       slices.Clone(cfg.Actors),
	}

	if cfg.URL != "" {
		u, err := url.Parse(cfg.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("graft land resolver: url %q must be an http or https URL with a host", cfg.URL)
		}

		if cfg.TokenFile != "" {
			raw, err := os.ReadFile(cfg.TokenFile)
			if err != nil {
				return nil, fmt.Errorf("graft land resolver: read token file: %w", err)
			}

			c.token = strings.TrimSpace(string(raw))
			if c.token == "" {
				return nil, fmt.Errorf("graft land resolver: token file %q is empty", cfg.TokenFile)
			}
		}

		c.url = cfg.URL
		c.httpClient = &http.Client{
			// Never follow redirects: the bearer token must not travel to
			// another location. The 3xx answer is then a failure.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	return c, nil
}

// Resolve sends req to the resolver and returns its validated answer. The
// request's Version and Limits are set by the client. A refusal is an error
// matching ErrUnresolvable; any other failure (timeout, bound exceeded, invalid
// answer, transport error) is a distinct error. Resolve returns an error
// without calling the resolver when the request breaks the client's limits or
// policy.
func (c *Client) Resolve(ctx context.Context, req Request) (*Response, error) {
	if c == nil {
		return nil, ErrDisabled
	}

	req.Version = ProtocolVersion
	req.Limits = Limits{MaxFileBytes: c.maxFileBytes}

	if err := c.checkRequest(req); err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode resolve request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	limit := c.outputLimit(len(req.Files))

	var raw []byte
	if c.url != "" {
		raw, err = c.callHTTP(ctx, body, limit)
	} else {
		raw, err = c.callCommand(ctx, body, limit)
	}

	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("resolver timed out after %s: %w", c.timeout, ctx.Err())
		}

		return nil, err
	}

	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}

	if err := Validate(&req, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// checkRequest enforces the client's limits and policy on an outgoing request:
// file count, per-side size, clean unique UTF-8 paths, known kinds, the path
// policy and the actor policy.
func (c *Client) checkRequest(req Request) error {
	if len(req.Files) > c.maxFiles {
		return fmt.Errorf("resolve request has %d conflicted files, limit is %d", len(req.Files), c.maxFiles)
	}

	if !c.ActorAllowed(req.Actor) {
		return fmt.Errorf("actor %q may not request conflict resolution", req.Actor)
	}

	seen := make(map[string]struct{}, len(req.Files))
	for _, f := range req.Files {
		if !cleanRelPath(f.Path) {
			return fmt.Errorf("resolve request: unsafe path %q", f.Path)
		}

		// JSON strings carry Unicode text, not bytes: encoding/json replaces
		// every invalid byte with U+FFFD. The resolver would see another path,
		// distinct paths could share one name, and the answer could never match
		// the request.
		if !utf8.ValidString(f.Path) {
			return fmt.Errorf("resolve request: path %q is not valid UTF-8, which the protocol cannot carry", f.Path)
		}

		if _, dup := seen[f.Path]; dup {
			return fmt.Errorf("resolve request: path %q listed more than once", f.Path)
		}
		seen[f.Path] = struct{}{}

		switch f.Kind {
		case KindContent, KindModifyDelete, KindAddAdd, KindMode:
		default:
			return fmt.Errorf("resolve request: path %q has unknown kind %q", f.Path, f.Kind)
		}

		if !c.PathAllowed(f.Path) {
			return fmt.Errorf("resolve request: path %q is not allowed to be resolved", f.Path)
		}

		for _, side := range []*Side{f.Base, f.Ours, f.Theirs} {
			if side != nil && int64(len(side.Content)) > c.maxFileBytes {
				return fmt.Errorf("resolve request: path %q has a %d byte version, limit is %d", f.Path, len(side.Content), c.maxFileBytes)
			}
		}
	}

	return nil
}

// outputLimit bounds the resolver's answer for a request of n files: two
// maximal files per file (base64 and JSON overhead, and room for a resolution
// larger than an input), plus a fixed allowance for the envelope.
func (c *Client) outputLimit(n int) int64 {
	return int64(max(n, 1))*2*c.maxFileBytes + maxEnvelopeBytes
}

// maxEnvelopeBytes is the fixed allowance on top of the per-file output bound.
const maxEnvelopeBytes = 64 << 10
