package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrCheckUnsupported is returned by Check when the resolver answers a check request with
// something other than a check response, as a resolver that predates the check operation does.
var ErrCheckUnsupported = errors.New("the resolver does not support the check operation")

// Check asks the resolver to check an agent's resolution of the request's files with the checks
// of its own answers (OperationCheck). It returns the response, with one verdict per file of
// req.Resolution in its order. It fails closed: an error, a refusal, or an answer that is not a
// check response with one verdict per file in order is an error, and the caller must not land the
// resolution.
func (c *Client) Check(ctx context.Context, req Request) (*CheckResponse, error) {
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
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("resolver check timed out after %s: %w", c.timeout, ctx.Err())
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
