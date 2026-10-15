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
		return nil, &UnresolvableError{Reason: refusalReason(resp.Body)}
	default:
		return nil, fmt.Errorf("resolver answered HTTP %d", resp.StatusCode)
	}
}

// refusalReason extracts the "error" member of a 422 answer, falling back to
// a generic reason when the body is not such an object.
func refusalReason(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
	if err != nil {
		return defaultRefusalReason
	}

	var answer struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return defaultRefusalReason
	}

	if reason := strings.TrimSpace(answer.Error); reason != "" {
		return reason
	}

	return defaultRefusalReason
}
