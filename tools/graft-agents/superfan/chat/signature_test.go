package chat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func signedHeader(secret string, ts time.Time, body []byte) http.Header {
	stamp := strconv.FormatInt(ts.Unix(), 10)
	h := http.Header{}
	h.Set(SlackTimestampHeader, stamp)
	h.Set(SlackSignatureHeader, SignSlack(secret, stamp, body))
	return h
}

func TestSignSlackMatchesSpecification(t *testing.T) {
	body := []byte(`token=x&text=what+changed`)
	const stamp = "1789387200"

	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte("v0:" + stamp + ":" + string(body)))
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))

	if got := SignSlack(testSecret, stamp, body); got != want {
		t.Fatalf("SignSlack = %q, want %q", got, want)
	}
}

func TestVerifySlackAcceptsValidSignature(t *testing.T) {
	body := []byte(`{"type":"event_callback"}`)
	h := signedHeader(testSecret, fixedNow, body)
	if err := VerifySlack(testSecret, h, body, fixedNow); err != nil {
		t.Fatalf("VerifySlack: %v", err)
	}
	// Four minutes of skew in either direction is inside the window.
	for _, skew := range []time.Duration{-4 * time.Minute, 4 * time.Minute} {
		ts := fixedNow.Add(skew)
		if err := VerifySlack(testSecret, signedHeader(testSecret, ts, body), body, fixedNow); err != nil {
			t.Fatalf("skew %v: %v", skew, err)
		}
	}
}

func TestVerifySlackRejections(t *testing.T) {
	body := []byte("text=hello")

	tests := []struct {
		name    string
		secret  string
		headers func() http.Header
		body    []byte
		now     time.Time
		want    error
	}{
		{
			name:    "wrong secret",
			secret:  "other-secret",
			headers: func() http.Header { return signedHeader(testSecret, fixedNow, body) },
			body:    body,
			now:     fixedNow,
			want:    ErrBadSignature,
		},
		{
			name:    "tampered body",
			secret:  testSecret,
			headers: func() http.Header { return signedHeader(testSecret, fixedNow, body) },
			body:    []byte("text=evil"),
			now:     fixedNow,
			want:    ErrBadSignature,
		},
		{
			name:   "signature without the v0 version",
			secret: testSecret,
			headers: func() http.Header {
				h := signedHeader(testSecret, fixedNow, body)
				h.Set(SlackSignatureHeader, strings.TrimPrefix(h.Get(SlackSignatureHeader), "v0="))
				return h
			},
			body: body,
			now:  fixedNow,
			want: ErrBadSignature,
		},
		{
			name:    "stale timestamp",
			secret:  testSecret,
			headers: func() http.Header { return signedHeader(testSecret, fixedNow.Add(-6*time.Minute), body) },
			body:    body,
			now:     fixedNow,
			want:    ErrStaleTimestamp,
		},
		{
			name:    "timestamp from the future",
			secret:  testSecret,
			headers: func() http.Header { return signedHeader(testSecret, fixedNow.Add(6*time.Minute), body) },
			body:    body,
			now:     fixedNow,
			want:    ErrStaleTimestamp,
		},
		{
			name:    "missing headers",
			secret:  testSecret,
			headers: func() http.Header { return http.Header{} },
			body:    body,
			now:     fixedNow,
			want:    ErrMissingSignature,
		},
		{
			name:   "malformed timestamp",
			secret: testSecret,
			headers: func() http.Header {
				h := signedHeader(testSecret, fixedNow, body)
				h.Set(SlackTimestampHeader, "soon")
				return h
			},
			body: body,
			now:  fixedNow,
			want: ErrMissingSignature,
		},
		{
			name:    "no secret configured",
			secret:  "",
			headers: func() http.Header { return signedHeader(testSecret, fixedNow, body) },
			body:    body,
			now:     fixedNow,
			want:    ErrNoSecret,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifySlack(tc.secret, tc.headers(), tc.body, tc.now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("VerifySlack error = %v, want %v", err, tc.want)
			}
		})
	}
}
