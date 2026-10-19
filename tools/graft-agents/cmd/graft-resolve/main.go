// Command graft-resolve answers Graft resolve requests (protocol v1, see
// doc/graft/PLAN2.md) with a configurable chain of deterministic strategies.
//
// Usage:
//
//	graft-resolve [--config FILE] [--max-request-bytes N] < request.json > response.json
//	graft-resolve serve --listen ADDR [--config FILE] [--token-file FILE] [--max-request-bytes N]
//	graft-resolve bench mine|run [flags]   (see bench.go and doc/graft/PLAN10.md)
//
// In command mode the request is read from stdin and the response written to
// stdout. An unresolvable request exits 1 with the reason on stderr. In serve
// mode the server answers POST requests: 200 with the response, 422 with
// {"error": ...} when a file stays unresolved, 400 for a malformed request,
// 401 when a bearer token is configured and missing or wrong, 413 when the
// request exceeds --max-request-bytes.
//
// Without --config only the trivial strategy is used.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve"
)

// shutdownTimeout bounds the graceful shutdown of serve mode.
const shutdownTimeout = 10 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "serve" {
		return runServe(args[1:], stderr)
	}
	if len(args) > 0 && args[0] == "bench" {
		return runBench(args[1:], stdout, stderr)
	}
	return runCommand(args, stdin, stdout, stderr)
}

func usage(fs *flag.FlagSet, w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  graft-resolve [--config FILE] [--max-request-bytes N] < request.json")
	fmt.Fprintln(w, "  graft-resolve serve --listen ADDR [--config FILE] [--token-file FILE] [--max-request-bytes N]")
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// loadResolver builds the resolver from the configuration file, or the
// default configuration when path is empty.
func loadResolver(path string) (*resolve.Resolver, error) {
	cfg := resolve.DefaultConfig()
	if path != "" {
		loaded, err := resolve.LoadConfig(path)
		if err != nil {
			return nil, err
		}
		cfg = *loaded
	}
	return cfg.Build()
}

// setMaxRequest applies the --max-request-bytes flag to the resolver.
func setMaxRequest(res *resolve.Resolver, maxRequest int64) error {
	if maxRequest <= 0 {
		return fmt.Errorf("--max-request-bytes must be positive, got %d", maxRequest)
	}
	res.MaxRequestBytes = maxRequest
	return nil
}

func runCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("graft-resolve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "configuration file (TOML)")
	maxRequest := fs.Int64("max-request-bytes", protocol.DefaultMaxRequestBytes, "bound on the request size in bytes")
	fs.Usage = func() { usage(fs, stderr) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "graft-resolve: unexpected arguments %v\n", fs.Args())
		return 2
	}

	res, err := loadResolver(*configPath)
	if err == nil {
		err = setMaxRequest(res, *maxRequest)
	}
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve: %v\n", err)
		return 1
	}
	res.Logf = func(format string, args ...any) {
		fmt.Fprintf(stderr, "graft-resolve: "+format+"\n", args...)
	}

	body, err := io.ReadAll(io.LimitReader(stdin, res.MaxRequestBytes+1))
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve: reading request: %v\n", err)
		return 1
	}
	if int64(len(body)) > res.MaxRequestBytes {
		fmt.Fprintln(stderr, "graft-resolve: request too large")
		return 1
	}

	var req protocol.Request
	if err := json.Unmarshal(body, &req); err != nil {
		fmt.Fprintf(stderr, "graft-resolve: invalid request JSON: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), res.Timeout)
	defer cancel()
	if req.Operation == protocol.OperationCheck {
		verdicts, err := res.Check(ctx, req)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return writeOut(stdout, stderr, verdicts)
	}
	resp, err := res.Resolve(ctx, req)
	if err != nil {
		fmt.Fprintln(stderr, err)
		// A refusal also goes to stdout as an ErrorResponse, with the outcome of every file.
		var unresolved *resolve.UnresolvedError
		if errors.As(err, &unresolved) {
			if out, jerr := json.Marshal(unresolved.Response()); jerr == nil {
				_, _ = stdout.Write(append(out, '\n'))
			}
		}
		return 1
	}
	return writeOut(stdout, stderr, resp)
}

// writeOut writes the answer of command mode on stdout.
func writeOut(stdout, stderr io.Writer, answer any) int {
	out, err := json.Marshal(answer)
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve: encoding response: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(append(out, '\n')); err != nil {
		fmt.Fprintf(stderr, "graft-resolve: writing response: %v\n", err)
		return 1
	}
	return 0
}

func runServe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("graft-resolve serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "address to listen on, for example 127.0.0.1:8099 (required)")
	configPath := fs.String("config", "", "configuration file (TOML)")
	tokenFile := fs.String("token-file", "", "file holding the bearer token; empty disables authentication")
	maxRequest := fs.Int64("max-request-bytes", protocol.DefaultMaxRequestBytes, "bound on the request size in bytes")
	fs.Usage = func() { usage(fs, stderr) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *listen == "" {
		fmt.Fprintln(stderr, "graft-resolve: --listen is required")
		return 2
	}

	token, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve: %v\n", err)
		return 1
	}
	res, err := loadResolver(*configPath)
	if err == nil {
		err = setMaxRequest(res, *maxRequest)
	}
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve: %v\n", err)
		return 1
	}
	res.Logf = func(format string, args ...any) {
		fmt.Fprintf(stderr, "graft-resolve: "+format+"\n", args...)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := newServer(*listen, res, token)
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve: %v\n", err)
		return 1
	}
	if token == "" {
		fmt.Fprintln(stderr, "graft-resolve: warning: no --token-file, requests are not authenticated")
	}
	fmt.Fprintf(stderr, "graft-resolve: listening on %s\n", srv.Addr)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "graft-resolve: %v\n", err)
			return 1
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "graft-resolve: shutdown: %v\n", err)
			return 1
		}
	}
	return 0
}

// readToken reads the bearer token from path, trimming surrounding space. An
// empty path means no authentication.
func readToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("token file is empty")
	}
	return token, nil
}
