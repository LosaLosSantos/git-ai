package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve"
)

// newServer returns an HTTP server with conservative timeouts. The response
// write timeout leaves room for the resolve timeout.
func newServer(listen string, res *resolve.Resolver, token string) (*http.Server, error) {
	if listen == "" {
		return nil, errors.New("empty listen address")
	}
	return &http.Server{
		Addr:              listen,
		Handler:           newHandler(res, token),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       res.Timeout + time.Minute,
		WriteTimeout:      res.Timeout + time.Minute,
		IdleTimeout:       time.Minute,
	}, nil
}

// newHandler answers resolve requests. Only POST is accepted, on any path, so
// that the server can be configured with any URL. When token is non-empty the
// request must carry "Authorization: Bearer <token>", compared in constant
// time.
func newHandler(res *resolve.Resolver, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if token != "" && !authorized(r, token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="graft-resolve"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, res.MaxRequestBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "request too large")
				return
			}
			writeError(w, http.StatusBadRequest, "reading request failed")
			return
		}

		var req protocol.Request
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON request")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), res.Timeout)
		defer cancel()
		resp, err := res.Resolve(ctx, req)
		if err != nil {
			var unresolved *resolve.UnresolvedError
			switch {
			case errors.As(err, &unresolved):
				writeError(w, http.StatusUnprocessableEntity, err.Error())
			case errors.Is(err, resolve.ErrInvalidRequest):
				writeError(w, http.StatusBadRequest, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

func authorized(r *http.Request, token string) bool {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	got := []byte(strings.TrimPrefix(auth, prefix))
	return subtle.ConstantTimeCompare(got, []byte(token)) == 1
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, protocol.ErrorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
