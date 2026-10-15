package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// writeToken writes a token file and returns its path.
func writeToken(t *testing.T, token string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(token+"\n"), 0o600))
	return path
}

// httpConfig returns an enabled URL resolver configuration.
func httpConfig(url, tokenFile string) config.GraftResolver {
	return config.GraftResolver{Enabled: true, URL: url, TokenFile: tokenFile, Timeout: "10s"}
}

// answeringHandler checks the protocol shape of a request and answers
// validResponse. Failures are reported with t.Errorf, which is safe to call
// from the handler goroutine.
func answeringHandler(t *testing.T, wantAuth string, hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		if r.Method != http.MethodPost {
			t.Errorf("method %s, want POST", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type %q", got)
		}

		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept %q", got)
		}

		if got := r.Header.Get("Authorization"); got != wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if req.Version != ProtocolVersion || len(req.Files) != 2 {
			t.Errorf("unexpected request: version %d, %d files", req.Version, len(req.Files))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(validResponse("merged\n"))
	}
}

func TestResolve_HTTPSuccess(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(answeringHandler(t, "Bearer s3cret", &hits))
	t.Cleanup(srv.Close)

	client := newClient(t, httpConfig(srv.URL, writeToken(t, "s3cret")))

	resp, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.NoError(t, err)
	require.Equal(t, []byte("merged\n"), resp.Files[0].Content)
	require.Equal(t, "graft-resolve/test", resp.Resolver)
	require.Equal(t, int32(1), hits.Load())
}

func TestResolve_HTTPWithoutTokenSendsNoAuthorization(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(answeringHandler(t, "", &hits))
	t.Cleanup(srv.Close)

	client := newClient(t, httpConfig(srv.URL, ""))

	_, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.NoError(t, err)
}

func TestResolve_HTTPTokenIsReadOnce(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(answeringHandler(t, "Bearer first", &hits))
	t.Cleanup(srv.Close)

	tokenFile := writeToken(t, "first")
	client := newClient(t, httpConfig(srv.URL, tokenFile))

	// Rotating the file afterwards does not change what the client sends.
	require.NoError(t, os.WriteFile(tokenFile, []byte("second\n"), 0o600))

	_, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.NoError(t, err)
}

func TestResolve_HTTPRefusal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		body       string
		wantReason string
	}{
		{name: "error member", body: `{"error":"ambiguous rename"}`, wantReason: "ambiguous rename"},
		{name: "error member is trimmed", body: `{"error":"  padded  "}`, wantReason: "padded"},
		{name: "empty error member", body: `{"error":""}`, wantReason: defaultRefusalReason},
		{name: "plain text body", body: `binary conflict`, wantReason: defaultRefusalReason},
		{name: "empty body", body: ``, wantReason: defaultRefusalReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			client := newClient(t, httpConfig(srv.URL, ""))

			resp, err := client.Resolve(testhelper.Context(t), sampleRequest())
			require.Nil(t, resp)
			require.ErrorIs(t, err, ErrUnresolvable)
			require.False(t, errors.Is(err, ErrInvalidResponse))

			var unresolvable *UnresolvableError
			require.ErrorAs(t, err, &unresolvable)
			require.Equal(t, tc.wantReason, unresolvable.Reason)
		})
	}
}

func TestResolve_HTTPOtherStatusIsFailure(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusInternalServerError, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"version":1,"files":[]}`))
			}))
			t.Cleanup(srv.Close)

			client := newClient(t, httpConfig(srv.URL, ""))

			resp, err := client.Resolve(testhelper.Context(t), sampleRequest())
			require.Nil(t, resp)
			require.Error(t, err)
			require.False(t, errors.Is(err, ErrUnresolvable), "only HTTP 422 is a refusal")
		})
	}
}

func TestResolve_HTTPRedirectIsNotFollowed(t *testing.T) {
	t.Parallel()

	var target atomic.Int32
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		target.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(dest.Close)

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusFound)
	}))
	t.Cleanup(redirect.Close)

	client := newClient(t, httpConfig(redirect.URL, writeToken(t, "s3cret")))

	_, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrUnresolvable))
	require.Equal(t, int32(0), target.Load(), "the bearer token must not follow a redirect")
}

func TestResolve_HTTPInvalidAnswers(t *testing.T) {
	t.Parallel()

	markerResp := validResponse("<<<<<<< ours\nx\n=======\ny\n>>>>>>> theirs\n")

	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "not JSON", body: "merged"},
		{name: "wrong version", body: `{"version":3,"files":[]}`},
		{name: "conflict marker", body: string(mustJSON(t, markerResp))},
		{name: "unanswered conflict", body: `{"version":1,"files":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			client := newClient(t, httpConfig(srv.URL, ""))

			resp, err := client.Resolve(testhelper.Context(t), sampleRequest())
			require.Nil(t, resp)
			require.ErrorIs(t, err, ErrInvalidResponse)
			require.False(t, errors.Is(err, ErrUnresolvable))
		})
	}
}

func TestResolve_HTTPOversizedAnswer(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Far beyond the bound for two 1 KiB files.
		_, _ = w.Write([]byte(strings.Repeat(" ", 300_000)))
	}))
	t.Cleanup(srv.Close)

	cfg := httpConfig(srv.URL, "")
	cfg.MaxFileBytes = 1024
	client := newClient(t, cfg)

	resp, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.Nil(t, resp)
	require.ErrorIs(t, err, ErrInvalidResponse)
	require.ErrorContains(t, err, "exceeds")
}

func TestResolve_HTTPTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	cfg := httpConfig(srv.URL, "")
	cfg.Timeout = "200ms"
	client := newClient(t, cfg)

	start := time.Now()
	resp, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.Nil(t, resp)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, errors.Is(err, ErrUnresolvable))
	require.Less(t, time.Since(start), 8*time.Second)
}

func TestResolve_HTTPUnreachable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	client := newClient(t, httpConfig(url, ""))

	resp, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.Nil(t, resp)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrUnresolvable))
}

func TestResolve_HTTPErrorDoesNotLeakToken(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client := newClient(t, httpConfig(srv.URL, writeToken(t, "s3cret-token-value")))

	_, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cret-token-value")
}

func TestResolve_HTTPRejectedRequestNeverSent(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)

	cfg := httpConfig(srv.URL, "")
	cfg.PathsDeny = []string{"a.*"}
	client := newClient(t, cfg)

	_, err := client.Resolve(testhelper.Context(t), sampleRequest())
	require.ErrorContains(t, err, "is not allowed to be resolved")
	require.Equal(t, int32(0), hits.Load())
}

func TestResolve_HTTPCallerCanceled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	client := newClient(t, httpConfig(srv.URL, ""))

	ctx, cancel := context.WithCancel(testhelper.Context(t))
	time.AfterFunc(100*time.Millisecond, cancel)

	_, err := client.Resolve(ctx, sampleRequest())
	require.ErrorIs(t, err, context.Canceled)
}

func TestResolve_HTTPConcurrentCalls(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(answeringHandler(t, "Bearer s3cret", &hits))
	t.Cleanup(srv.Close)

	client := newClient(t, httpConfig(srv.URL, writeToken(t, "s3cret")))

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			_, err := client.Resolve(context.Background(), sampleRequest())
			errs <- err
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(workers), hits.Load())
}
