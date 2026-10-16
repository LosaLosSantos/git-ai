package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve"
)

const testToken = "tok-s3cret"

func newTestHandler(t *testing.T, token string, cfg string) http.Handler {
	t.Helper()
	c := resolve.DefaultConfig()
	if cfg != "" {
		parsed, err := resolve.ParseConfig([]byte(cfg))
		if err != nil {
			t.Fatal(err)
		}
		c = *parsed
	}
	r, err := c.Build()
	if err != nil {
		t.Fatal(err)
	}
	return newHandler(r, token)
}

func post(t *testing.T, url, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestServeResolvesOverHTTP(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t, "", ""))
	defer srv.Close()

	resp := post(t, srv.URL+"/resolve", "", requestJSON(t,
		fileConflict("a.go", "base\n", "same\n", "same\n"),
	))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, readAll(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
	var out protocol.Response
	if err := json.Unmarshal(readAll(t, resp), &out); err != nil {
		t.Fatal(err)
	}
	if out.Resolver != "graft-resolve/trivial" || string(out.Files[0].Content) != "same\n" {
		t.Fatalf("response = %+v", out)
	}
}

func TestServeAnyPathAccepted(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t, "", ""))
	defer srv.Close()
	resp := post(t, srv.URL+"/any/where", "", requestJSON(t))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestServeMethodNotAllowed(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t, "", ""))
	defer srv.Close()
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req, err := http.NewRequest(method, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodPost {
			t.Fatalf("%s: status %d allow %q", method, resp.StatusCode, resp.Header.Get("Allow"))
		}
	}
}

func TestServeBearerToken(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t, testToken, ""))
	defer srv.Close()
	body := requestJSON(t)

	cases := map[string]struct {
		token string
		want  int
	}{
		"correct":  {testToken, http.StatusOK},
		"missing":  {"", http.StatusUnauthorized},
		"wrong":    {"tok-other", http.StatusUnauthorized},
		"prefix":   {testToken[:4], http.StatusUnauthorized},
		"superset": {testToken + "x", http.StatusUnauthorized},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			resp := post(t, srv.URL, c.token, body)
			if resp.StatusCode != c.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, c.want)
			}
			if c.want == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" {
				t.Fatal("401 without WWW-Authenticate")
			}
		})
	}

	// A bare token without the scheme is not accepted.
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token without Bearer scheme: status %d", resp.StatusCode)
	}
}

func TestServeUnresolvedIs422WithError(t *testing.T) {
	cfg := "[[strategy]]\ntype = \"union\"\npaths = [\"CHANGELOG.md\"]\n"
	srv := httptest.NewServer(newTestHandler(t, "", cfg))
	defer srv.Close()

	resp := post(t, srv.URL, "", requestJSON(t, fileConflict("code.go", "a\n", "b\n", "c\n")))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var e protocol.ErrorResponse
	if err := json.Unmarshal(readAll(t, resp), &e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.Error, "code.go") {
		t.Fatalf("error = %q", e.Error)
	}
}

func TestServeMalformedRequests(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t, "", ""))
	defer srv.Close()

	cases := map[string]struct {
		body []byte
		want int
	}{
		"not json":      {[]byte("{oops"), http.StatusBadRequest},
		"unsafe path":   {[]byte(`{"version":1,"files":[{"path":"../x","kind":"content"}]}`), http.StatusBadRequest},
		"wrong version": {[]byte(`{"version":9,"files":[]}`), http.StatusBadRequest},
		"empty body":    {nil, http.StatusBadRequest},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			resp := post(t, srv.URL, "", c.body)
			if resp.StatusCode != c.want {
				t.Fatalf("status %d, want %d (%s)", resp.StatusCode, c.want, readAll(t, resp))
			}
			var e protocol.ErrorResponse
			if err := json.Unmarshal(readAll(t, resp), &e); err != nil || e.Error == "" {
				t.Fatalf("error body not JSON with error field: %v", err)
			}
		})
	}
}

func TestServeConcurrentRequests(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t, testToken, ""))
	defer srv.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := requestJSON(t, fileConflict(fmt.Sprintf("f%d.go", i), "base\n", "x\n", "x\n"))
			req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(string(body)))
			if err != nil {
				errs <- err
				return
			}
			req.Header.Set("Authorization", "Bearer "+testToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("request %d: status %d", i, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestNewServerSetsTimeouts(t *testing.T) {
	c := resolve.DefaultConfig()
	r, err := c.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := newServer("127.0.0.1:9999", r, "")
	if err != nil {
		t.Fatal(err)
	}
	if srv.Addr != "127.0.0.1:9999" || srv.ReadHeaderTimeout == 0 || srv.WriteTimeout < r.Timeout {
		t.Fatalf("server = %+v", srv)
	}
	if _, err := newServer("", r, ""); err == nil {
		t.Fatal("empty listen accepted")
	}
}
