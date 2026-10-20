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

	"github.com/feder-cr/git-ai/internal/gitaly/config"
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
	// Files are the resolver's outcomes per requested file, when it gave them
	// (the "files" member of the refusal). Resolve keeps them only when there is
	// exactly one per requested path, each with a known outcome; otherwise Files
	// is nil and every file shares Reason.
	Files []FileOutcome
}

// validOutcomes returns files when it has exactly one entry per requested path
// with a known outcome, and nil otherwise: a malformed list is not trusted in
// part. A refusal whose every file is resolved contradicts itself and is
// malformed too.
func validOutcomes(req Request, files []FileOutcome) []FileOutcome {
	if len(files) != len(req.Files) {
		return nil
	}
	unresolved := false
	for _, f := range files {
		if f.Outcome != OutcomeResolved {
			unresolved = true
		}
	}
	if !unresolved {
		return nil
	}
	want := make(map[string]bool, len(req.Files))
	for _, f := range req.Files {
		want[f.Path] = true
	}
	for _, f := range files {
		switch f.Outcome {
		case OutcomeResolved, OutcomeRejected, OutcomeFailed:
		default:
			return nil
		}
		if !want[f.Path] {
			return nil
		}
		delete(want, f.Path)
	}
	return files
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
// minInlineSlots is the least size of the pool of the lane's own calls: they are rules, which take
// milliseconds, and each lane (one per branch) needs one at a time, so that the pool of one server is
// not the bottleneck of its branches.
const minInlineSlots = 8

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
	// checkSlots bounds the checks of agents' resolutions that run at once (CheckSlot), and
	// resolveSlots the resolutions (ResolveSlot): two pools, so that long resolutions never delay
	// the short checks (PLAN10 15.8).
	checkSlots   chan struct{}
	resolveSlots chan struct{}
	// inlineSlots bounds the calls with which the lane resolves the rules itself (TryAcquireInlineSlot): a pool
	// of its own, which is never waited for, so that a model that holds the resolve slots does not stop the rules.
	inlineSlots chan struct{}
	// sendRules decide the files that are never sent (SendRules).
	sendRules *SendRules
}

// SendRules returns the rules of the files that are never sent to this resolver.
func (c *Client) SendRules() *SendRules {
	return c.sendRules
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
	if _, err := cfg.ProposalTTLOrDefault(); err != nil {
		return nil, err
	}

	if cfg.MaxFiles < 0 || cfg.MaxFileBytes < 0 || cfg.CheckConcurrency < 0 || cfg.ResolveConcurrency < 0 || cfg.VerifyConcurrency < 0 || cfg.VerifyPerRepository < 0 || cfg.ResolveRounds < 0 {
		return nil, errors.New("graft land resolver: max_files, max_file_bytes, check_concurrency, resolve_concurrency, verify_concurrency, verify_per_repository and resolve_rounds must not be negative")
	}
	// The check of an agent's resolution sends a file's three sides and its resolution in one
	// request, which must fit its bound.
	if mfb := cfg.MaxFileBytesOrDefault(); mfb > MaxCheckFileBytes {
		return nil, fmt.Errorf("graft land resolver: max_file_bytes %d is too large: the check of a resolution sends a file's three sides and its resolution in one request of at most %d bytes, so max_file_bytes can be at most %d", mfb, MaxCheckRequestBytes, MaxCheckFileBytes)
	}

	rules, err := NewSendRules(cfg)
	if err != nil {
		return nil, err
	}
	c := &Client{
		command:      slices.Clone(cfg.Command),
		timeout:      timeout,
		maxFiles:     cfg.MaxFilesOrDefault(),
		maxFileBytes: cfg.MaxFileBytesOrDefault(),
		pathsAllow:   slices.Clone(cfg.PathsAllow),
		pathsDeny:    slices.Clone(cfg.PathsDeny),
		actors:       slices.Clone(cfg.Actors),
		sendRules:    rules,
		checkSlots:   make(chan struct{}, cfg.CheckConcurrencyOrDefault()),
		resolveSlots: make(chan struct{}, cfg.ResolveConcurrencyOrDefault()),
		inlineSlots:  make(chan struct{}, max(minInlineSlots, cfg.ResolveConcurrencyOrDefault())),
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

// resolve sends req to the resolver and returns its validated answer; callers
// send it with a resolve slot (ResolveSlot.Resolve). The request's Version and
// Limits are set by the client. A refusal is an error matching
// ErrUnresolvable; any other failure (timeout, bound exceeded, invalid answer,
// transport error) is a distinct error. It returns an error without calling
// the resolver when the request breaks the client's limits or policy.
func (c *Client) resolve(ctx context.Context, req Request) (*Response, error) {
	if c == nil {
		return nil, ErrDisabled
	}

	req.Version = ProtocolVersion
	req.Limits = Limits{MaxFileBytes: c.maxFileBytes}

	if err := c.checkResolvePolicy(req); err != nil {
		return nil, err
	}
	if err := c.checkFiles(req); err != nil {
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

		var refusal *UnresolvableError
		if errors.As(err, &refusal) {
			refusal.Files = validOutcomes(req, refusal.Files)
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

// checkResolvePolicy enforces what bounds a request for an AI resolution only:
// max_files and the actor policy. The check of an agent's resolution is not
// such a request: it runs for every actor that may land, on every file the
// agent resolved (PLAN10 12).
func (c *Client) checkResolvePolicy(req Request) error {
	if len(req.Files) > c.maxFiles {
		return fmt.Errorf("resolve request has %d conflicted files, limit is %d", len(req.Files), c.maxFiles)
	}

	if !c.ActorAllowed(req.Actor) {
		return fmt.Errorf("actor %q may not request conflict resolution", req.Actor)
	}
	return nil
}

// checkFiles enforces the client's rules on the files of an outgoing request,
// a resolve or a check: per-side size, clean unique UTF-8 paths, known kinds
// and the path policy.
func (c *Client) checkFiles(req Request) error {
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
const maxEnvelopeBytes = config.GraftResolverCheckEnvelopeBytes
