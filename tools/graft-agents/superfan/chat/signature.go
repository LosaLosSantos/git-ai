package chat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// Slack request headers and limits.
const (
	SlackSignatureHeader = "X-Slack-Signature"
	SlackTimestampHeader = "X-Slack-Request-Timestamp"
	// slackRetryHeader is set by Slack on the retries of an unacknowledged event.
	slackRetryHeader = "X-Slack-Retry-Num"
	// MaxSlackSignatureAge is the largest clock difference accepted between a
	// signed Slack request and the server.
	MaxSlackSignatureAge  = 5 * time.Minute
	slackSignatureVersion = "v0"
)

// Verification errors. None of them carries the secret or the signature.
var (
	ErrNoSecret         = errors.New("chat: no Slack signing secret")
	ErrMissingSignature = errors.New("chat: missing or malformed Slack signature headers")
	ErrStaleTimestamp   = errors.New("chat: Slack request timestamp outside the allowed window")
	ErrBadSignature     = errors.New("chat: Slack signature mismatch")
)

// SignSlack returns the signature Slack sends for a request: "v0=" followed by
// the hex HMAC-SHA256 of "v0:<timestamp>:<body>" under secret.
func SignSlack(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(slackSignatureVersion + ":" + timestamp + ":"))
	mac.Write(body)
	return slackSignatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySlack checks that a request carries a valid Slack signature made within
// MaxSlackSignatureAge of now. The comparison is constant time.
func VerifySlack(secret string, h http.Header, body []byte, now time.Time) error {
	if secret == "" {
		return ErrNoSecret
	}
	ts := h.Get(SlackTimestampHeader)
	sig := h.Get(SlackSignatureHeader)
	if ts == "" || sig == "" {
		return ErrMissingSignature
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrMissingSignature
	}
	age := now.Sub(time.Unix(sec, 0))
	if age > MaxSlackSignatureAge || age < -MaxSlackSignatureAge {
		return ErrStaleTimestamp
	}
	if !hmac.Equal([]byte(sig), []byte(SignSlack(secret, ts, body))) {
		return ErrBadSignature
	}
	return nil
}
