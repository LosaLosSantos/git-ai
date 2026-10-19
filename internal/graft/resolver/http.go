package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// defaultRefusalReason is the reason of a 422 without a usable error message.
const defaultRefusalReason = "resolver refused the conflict"

// callHTTP POSTs body to the resolver URL and returns the body of a 200
// answer, bounded by limit. HTTP 422 is an UnresolvableError whose reason is
// the "error" member of the JSON answer. Any other status is a failure.
func (c *Client) callHTTP(ctx context.Context, body []byte, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build resolver request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call resolver: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, fmt.Errorf("read resolver response: %w", err)
		}

		if int64(len(raw)) > limit {
			return nil, fmt.Errorf("%w: output exceeds %d bytes", ErrInvalidResponse, limit)
		}

		return raw, nil
	case http.StatusUnprocessableEntity:
		raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
		if err == nil {
			if refusal, ok := parseRefusal(raw); ok {
				return nil, refusal
			}
		}
		return nil, &UnresolvableError{Reason: defaultRefusalReason}
	default:
		return nil, fmt.Errorf("resolver answered HTTP %d", resp.StatusCode)
	}
}

// parseRefusal reads an ErrorResponse: the body of a 422, or the stdout of a
// command that exited non-zero. It reports false when raw is not such an
// object with a non-empty error. The reason is cut to maxErrorBodyBytes; the
// files are checked against the request by Resolve.
func parseRefusal(raw []byte) (*UnresolvableError, bool) {
	var answer ErrorResponse
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, false
	}

	reason := strings.TrimSpace(answer.Error)
	if reason == "" {
		return nil, false
	}
	if len(reason) > maxErrorBodyBytes {
		reason = strings.ToValidUTF8(reason[:maxErrorBodyBytes], "")
	}
	return &UnresolvableError{Reason: reason, Files: answer.Files}, true
}
