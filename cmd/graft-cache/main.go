// Command graft-cache runs a Graft cache tier. It sits between clients and a
// Gitaly or another graft-cache, serves ObjectService reads from memory and an
// optional disk store, and proxies every other RPC to its upstream.
//
// Example:
//
//	graft-cache --listen unix:/run/graft-cache.sock --upstream unix:/run/gitaly.sock \
//	    --tier daemon --max-bytes 536870912 --metrics-addr 127.0.0.1:9236
//
// A seed file preloads the cache before it serves. It is written from a
// repository's root tree with the snapshot subcommand, and loaded with --seed:
//
//	graft-cache snapshot --upstream unix:/run/gitaly.sock --repo demo.git --rev main --out demo.seed
//	graft-cache --listen unix:/run/graft-cache.sock --upstream unix:/run/gitaly.sock --seed demo.seed
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/cachetier"
	"github.com/feder-cr/git-ai/internal/log"
	"google.golang.org/grpc"
)

const defaultMaxBytes = 256 << 20

type options struct {
	listen       string
	upstream     string
	tier         string
	maxBytes     int64
	maxObjects   int
	chunkSize    int
	diskDir      string
	diskMaxBytes int64
	metricsAddr  string
	token        string
	// maxRequestBytes bounds the objects one GetObjects request buffers outside
	// the in-memory cache, and the GetTree listings buffered for caching.
	maxRequestBytes int64
	// seed is a comma-separated list of seed files preloaded before serving.
	seed string
}

// gracefulStopTimeout bounds how long shutdown waits for in-flight RPCs. Proxied
// streams such as WatchChangesets only end when their client cancels, so without
// a bound they would keep the process alive indefinitely.
const gracefulStopTimeout = 10 * time.Second

// stopGRPCServer stops srv gracefully, letting in-flight RPCs finish. It returns
// false if the RPCs were still running after timeout, in which case it stops srv
// forcibly. Either way it returns only once srv has stopped.
func stopGRPCServer(srv *grpc.Server, timeout time.Duration) bool {
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-stopped:
		return true
	case <-timer.C:
		srv.Stop()
		<-stopped
		return false
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "graft-cache:", err)
		os.Exit(1)
	}
}

// parseOptions parses args into options. It is separate from run so that the
// flag handling can be tested without starting a server.
func parseOptions(args []string, stderr *os.File) (options, error) {
	var opts options

	fs := flag.NewFlagSet("graft-cache", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.listen, "listen", "", `address to serve on: "unix:<path>" or "tcp://<host:port>" (required)`)
	fs.StringVar(&opts.upstream, "upstream", "", "Gitaly or graft-cache address to proxy to, e.g. unix:<path> or tcp://<host:port> (required)")
	fs.StringVar(&opts.tier, "tier", cachetier.TierCache, `tier role: "cache" (regional, may use --disk-dir) or "daemon" (per node, memory only)`)
	fs.Int64Var(&opts.maxBytes, "max-bytes", defaultMaxBytes, "byte budget of the in-memory object and tree cache")
	fs.IntVar(&opts.maxObjects, "max-objects", config.DefaultGraftMaxObjects, "object IDs per GetObjects request; must match the upstream's ObjectService.MaxObjects")
	fs.IntVar(&opts.chunkSize, "chunk-size", config.DefaultGraftChunkSize, "data bytes per GetObjects message; must match the upstream's ObjectService.ChunkSize")
	fs.StringVar(&opts.diskDir, "disk-dir", "", "directory for the on-disk object store (cache tier only)")
	fs.Int64Var(&opts.diskMaxBytes, "disk-max-bytes", cachetier.DefaultDiskMaxBytes, "byte budget of the on-disk object store; the oldest objects are evicted first")
	fs.Int64Var(&opts.maxRequestBytes, "max-request-bytes", cachetier.DefaultMaxRequestBytes, "bytes of objects not in memory (disk hits and misses) one GetObjects request may buffer, and the largest GetTree listing buffered for caching; a request over it is streamed through the upstream without caching")
	fs.StringVar(&opts.metricsAddr, "metrics-addr", "", "optional HTTP address serving /metrics and /stats")
	fs.StringVar(&opts.token, "token", "", "shared token sent upstream on requests that carry no authorization of their own; requests with authorization always use their own")
	fs.StringVar(&opts.seed, "seed", "", "comma-separated seed files, written by \"graft-cache snapshot\", preloaded into the cache before serving; every object is verified")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.listen == "" {
		return options{}, errors.New("--listen is required")
	}
	if opts.upstream == "" {
		return options{}, errors.New("--upstream is required")
	}
	if opts.chunkSize <= 0 {
		return options{}, errors.New("--chunk-size must be positive")
	}
	if opts.diskMaxBytes <= 0 {
		return options{}, errors.New("--disk-max-bytes must be positive")
	}
	return opts, nil
}

// listenNetworkAndAddress turns a --listen value into a network and address.
func listenNetworkAndAddress(listen string) (string, string, error) {
	if path, ok := strings.CutPrefix(listen, "unix:"); ok && path != "" {
		return "unix", path, nil
	}
	if addr, ok := strings.CutPrefix(listen, "tcp://"); ok && addr != "" {
		return "tcp", addr, nil
	}
	return "", "", fmt.Errorf(`invalid --listen %q: use "unix:<path>" or "tcp://<host:port>"`, listen)
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "snapshot" {
		return runSnapshot(args[1:])
	}

	opts, err := parseOptions(args, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	network, address, err := listenNetworkAndAddress(opts.listen)
	if err != nil {
		return err
	}

	logger, err := log.Configure(os.Stderr, "text", "info")
	if err != nil {
		return fmt.Errorf("configure logger: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	upstream, err := cachetier.DialUpstream(ctx, opts.upstream, logger)
	if err != nil {
		return fmt.Errorf("dial upstream: %w", err)
	}
	defer upstream.Close()

	server, err := cachetier.NewServer(upstream, cachetier.Config{
		Tier:            opts.tier,
		MaxBytes:        opts.maxBytes,
		MaxObjects:      opts.maxObjects,
		ChunkSize:       opts.chunkSize,
		DiskDir:         opts.diskDir,
		DiskMaxBytes:    opts.diskMaxBytes,
		MaxRequestBytes: opts.maxRequestBytes,
		Token:           opts.token,
		Logger:          logger,
	})
	if err != nil {
		return fmt.Errorf("configure cache tier: %w", err)
	}

	if err := loadSeedFiles(server, splitSeedFiles(opts.seed), logger); err != nil {
		return err
	}

	if network == "unix" {
		// A socket file left behind by a previous run would make Listen fail.
		if err := os.Remove(address); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	}

	listener, err := net.Listen(network, address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	grpcServer := cachetier.NewGRPCServer(server)

	var httpServer *http.Server
	if opts.metricsAddr != "" {
		httpServer = &http.Server{
			Addr:              opts.metricsAddr,
			Handler:           server.HTTPHandler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.WithError(err).Error("graft-cache: metrics server stopped")
			}
		}()
	}

	logger.WithField("listen", opts.listen).WithField("tier", opts.tier).Info("graft-cache: serving")

	serveErr := make(chan error, 1)
	go func() { serveErr <- grpcServer.Serve(listener) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	if !stopGRPCServer(grpcServer, gracefulStopTimeout) {
		logger.Warn("graft-cache: streams still open after the graceful stop timeout, stopped them")
	}
	if httpServer != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}
	return nil
}
