package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// MaxCheckRequestBytes bounds the encoded check request. graft-resolve reads 128 MiB by default
// (protocol.DefaultMaxRequestBytes); the server splits an agent's resolution into check requests
// within this bound.
const MaxCheckRequestBytes = config.GraftResolverMaxCheckRequestBytes

// CheckBatchBytes bounds the file data of one check request: the sides and the resolution of its
// files, plus CheckFileOverheadBytes for each. base64 makes the contents 4/3 larger, and the
// request's envelope takes at most config.GraftResolverCheckEnvelopeBytes, so the request stays
// within MaxCheckRequestBytes.
const CheckBatchBytes = config.GraftResolverCheckBatchBytes

// MaxCheckFileBytes is the largest max_file_bytes a resolver can have: one file, its three sides
// and its resolution, must fit one check request. The configuration refuses a larger one.
const MaxCheckFileBytes = config.GraftResolverMaxCheckFileBytes

// CheckFileOverheadBytes is what a file adds to a check request besides its contents: its path,
// written in the file and in the resolution, escaped in JSON (at most 6 bytes a byte), and the
// names and values of the other fields.
func CheckFileOverheadBytes(path string) int64 {
	return int64(2*6*len(path)) + 1<<10
}

// ErrCheckUnsupported is returned by Check when the resolver answers a check request with
// something other than a check response, as a resolver that predates the check operation does.
var ErrCheckUnsupported = errors.New("the resolver does not support the check operation")

// checkSlotWait is the time the checks of agents' resolutions waited for a check slot.
var checkSlotWait = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "graft_resolver_check_slot_wait_seconds",
	Help:    "Time a check of an agent's resolution waited for one of the resolver's check slots.",
	Buckets: prometheus.ExponentialBuckets(0.01, 4, 8),
})

// CheckSlot is the right to send one check request at a time: the client runs at most
// check_concurrency checks at once (AcquireCheckSlot). Release it when the request is done.
type CheckSlot struct {
	c        *Client
	released bool
}

// AcquireCheckSlot waits for a check slot. The wait counts against the deadline of ctx: when ctx
// ends first, it fails, and the check fails closed.
func (c *Client) AcquireCheckSlot(ctx context.Context) (*CheckSlot, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	start := time.Now()
	select {
	case c.checkSlots <- struct{}{}:
		checkSlotWait.Observe(time.Since(start).Seconds())
		return &CheckSlot{c: c}, nil
	case <-ctx.Done():
		checkSlotWait.Observe(time.Since(start).Seconds())
		return nil, fmt.Errorf("waiting for a check slot of the resolver: %w", ctx.Err())
	}
}

// Release gives the slot back; a second call does nothing.
func (s *CheckSlot) Release() {
	if s == nil || s.released {
		return
	}
	s.released = true
	<-s.c.checkSlots
}

// resolveSlotWait is the time the resolutions waited for a resolve slot.
var resolveSlotWait = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "graft_resolver_resolve_slot_wait_seconds",
	Help:    "Time a resolution waited for one of the resolver's resolve slots.",
	Buckets: prometheus.ExponentialBuckets(0.01, 4, 8),
})

// ResolveSlot is the right to send one resolve request at a time: the client runs at most
// resolve_concurrency resolutions at once (AcquireResolveSlot), in a pool apart from the checks'.
// Release it when the request is done.
type ResolveSlot struct {
	c        *Client
	released bool
}

// AcquireResolveSlot waits for a resolve slot. The wait counts against the deadline of ctx: when
// ctx ends first, it fails, and the resolution fails closed.
func (c *Client) AcquireResolveSlot(ctx context.Context) (*ResolveSlot, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	start := time.Now()
	select {
	case c.resolveSlots <- struct{}{}:
		resolveSlotWait.Observe(time.Since(start).Seconds())
		return &ResolveSlot{c: c}, nil
	case <-ctx.Done():
		resolveSlotWait.Observe(time.Since(start).Seconds())
		return nil, fmt.Errorf("waiting for a resolve slot of the resolver: %w", ctx.Err())
	}
}

// Release gives the slot back; a second call does nothing.
func (s *ResolveSlot) Release() {
	if s == nil || s.released {
		return
	}
	s.released = true
	<-s.c.resolveSlots
}

// Resolve sends one resolve request with the slot (Client.resolve).
func (s *ResolveSlot) Resolve(ctx context.Context, req Request) (*Response, error) {
	if s == nil || s.released {
		return nil, errors.New("resolve without a resolve slot")
	}
	return s.c.resolve(ctx, req)
}

// verifySlotWait is the time the verifications waited for a verify slot.
var verifySlotWait = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "graft_resolver_verify_slot_wait_seconds",
	Help:    "Time a verification of a model's resolution waited for one of the resolver's verify slots.",
	Buckets: prometheus.ExponentialBuckets(0.01, 4, 8),
})

// VerifySlot is the right to run one verification at a time: the client runs at most
// verify_concurrency of them at once (AcquireVerifySlot), in a pool apart from the checks' and the
// resolutions'. Release it when the verification is done.
type VerifySlot struct {
	c        *Client
	released bool
}

// AcquireVerifySlot waits for a verify slot. The wait counts against the deadline of ctx: when ctx
// ends first, it fails, and the land cannot be verified in time.
func (c *Client) AcquireVerifySlot(ctx context.Context) (*VerifySlot, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	start := time.Now()
	select {
	case c.verifySlots <- struct{}{}:
		verifySlotWait.Observe(time.Since(start).Seconds())
		return &VerifySlot{c: c}, nil
	case <-ctx.Done():
		verifySlotWait.Observe(time.Since(start).Seconds())
		return nil, fmt.Errorf("waiting for a verify slot of the resolver: %w", ctx.Err())
	}
}

// Release gives the slot back; a second call does nothing.
func (s *VerifySlot) Release() {
	if s == nil || s.released {
		return
	}
	s.released = true
	<-s.c.verifySlots
}

// Check sends one check request with the slot (Client.check).
func (s *CheckSlot) Check(ctx context.Context, req Request) (*CheckResponse, error) {
	if s == nil || s.released {
		return nil, errors.New("check without a check slot")
	}
	return s.c.check(ctx, req)
}

// check asks the resolver to check an agent's resolution of the request's files with the checks
// of its own answers (OperationCheck). It returns the response, with one verdict per file of
// req.Resolution in its order. It fails closed: an error, a refusal, or an answer that is not a
// check response with one verdict per file in order is an error, and the caller must not land the
// resolution.
func (c *Client) check(ctx context.Context, req Request) (*CheckResponse, error) {
	if c == nil {
		return nil, ErrDisabled
	}

	req.Version = ProtocolVersion
	req.Operation = OperationCheck
	req.Limits = Limits{MaxFileBytes: c.maxFileBytes}

	if err := c.checkFiles(req); err != nil {
		return nil, err
	}
	if len(req.Resolution) != len(req.Files) {
		return nil, fmt.Errorf("check request: %d resolved files for %d conflicting files", len(req.Resolution), len(req.Files))
	}
	for _, f := range req.Resolution {
		if int64(len(f.Content)) > c.maxFileBytes {
			return nil, fmt.Errorf("check request: resolved %q is %d bytes, limit is %d", f.Path, len(f.Content), c.maxFileBytes)
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode check request: %w", err)
	}
	if len(body) > MaxCheckRequestBytes {
		return nil, fmt.Errorf("check request of %d bytes is over the bound of %d", len(body), MaxCheckRequestBytes)
	}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// The answer is a verdict per file, without content.
	limit := int64(maxEnvelopeBytes) + int64(len(req.Resolution))*(maxCheckVerdictBytes)

	var raw []byte
	if c.url != "" {
		raw, err = c.callHTTP(ctx, body, limit)
	} else {
		raw, err = c.callCommand(ctx, body, limit)
	}
	if err != nil {
		switch {
		case parent.Err() != nil:
			// The deadline of the whole check (or the caller's) ended first.
			return nil, fmt.Errorf("resolver check stopped at the deadline of the check: %w", parent.Err())
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return nil, fmt.Errorf("resolver check request timed out after %s: %w", c.timeout, ctx.Err())
		}
		return nil, fmt.Errorf("resolver check: %w", err)
	}

	var resp CheckResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	if resp.Operation != OperationCheck {
		return nil, ErrCheckUnsupported
	}
	if resp.Version != ProtocolVersion {
		return nil, fmt.Errorf("%w: check response version %d", ErrInvalidResponse, resp.Version)
	}
	if len(resp.Files) != len(req.Resolution) {
		return nil, fmt.Errorf("%w: %d verdicts for %d files", ErrInvalidResponse, len(resp.Files), len(req.Resolution))
	}
	for i, v := range resp.Files {
		if v.Path != req.Resolution[i].Path {
			return nil, fmt.Errorf("%w: verdict %d is for %q, not %q", ErrInvalidResponse, i, v.Path, req.Resolution[i].Path)
		}
		switch v.Syntax {
		case SyntaxChecked, SyntaxUnchecked, SyntaxNone:
		default:
			return nil, fmt.Errorf("%w: verdict for %q has syntax %q", ErrInvalidResponse, v.Path, v.Syntax)
		}
	}
	return &resp, nil
}

// maxCheckVerdictBytes bounds the answer of one file of a check: its path and a reason.
const maxCheckVerdictBytes = 8 << 10
