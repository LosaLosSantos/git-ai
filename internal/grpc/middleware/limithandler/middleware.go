package limithandler

import (
	"context"
	"fmt"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/server/auth"
	"github.com/feder-cr/git-ai/internal/grpc/middleware/requestinfohandler"
	"github.com/feder-cr/git-ai/internal/limiter"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
)

// GetLockKey function defines the lock key of an RPC invocation based on its context
type GetLockKey func(context.Context) string

// LimitConcurrencyByRepo implements GetLockKey by using the repository path as lock.
func LimitConcurrencyByRepo(ctx context.Context) string {
	if info := requestinfohandler.Extract(ctx); info != nil {
		return info.Repository.GetRelativePath()
	}

	return ""
}

// LimiterMiddleware contains rate limiter state
type LimiterMiddleware struct {
	methodLimiters                map[string]limiter.Limiter
	methodLimitersUnauthenticated map[string]limiter.Limiter
	internalClients               map[string]struct{}
	getLockKey                    GetLockKey
	requestsDroppedMetric         *prometheus.CounterVec
	collect                       func(metrics chan<- prometheus.Metric)
}

// New creates a new middleware that limits requests. SetupFunc sets up the
// middleware with a specific kind of limiter.
func New(cfg config.Cfg, getLockKey GetLockKey, setupMiddleware SetupFunc) *LimiterMiddleware {
	internalClients := make(map[string]struct{}, len(cfg.InternalClients))
	for _, name := range cfg.InternalClients {
		internalClients[name] = struct{}{}
	}

	middleware := &LimiterMiddleware{
		getLockKey:      getLockKey,
		internalClients: internalClients,
		requestsDroppedMetric: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gitaly_requests_dropped_total",
				Help: "Number of requests dropped from the queue",
			},
			[]string{
				"system",
				"grpc_service",
				"grpc_method",
				"reason",
				"authenticated",
			},
		),
	}

	setupMiddleware(cfg, middleware)

	return middleware
}

// Describe is used to describe Prometheus metrics.
func (c *LimiterMiddleware) Describe(descs chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, descs)
}

// Collect is used to collect Prometheus metrics.
func (c *LimiterMiddleware) Collect(metrics chan<- prometheus.Metric) {
	c.requestsDroppedMetric.Collect(metrics)
	if c.collect != nil {
		c.collect(metrics)
	}
}

// useUnauthenticatedLimiter reports whether the request should be subject to the unauthenticated
// concurrency limits. A request with an end-user username is always authenticated. A request without
// one is treated as authenticated when it comes from a trusted internal client (for example GitLab
// Sidekiq), which is token-authenticated background work rather than anonymous public Git access.
func (c *LimiterMiddleware) useUnauthenticatedLimiter(ctx context.Context) bool {
	if auth.IsAuthenticated(ctx) {
		return false
	}

	if info := requestinfohandler.Extract(ctx); info != nil {
		if _, ok := c.internalClients[info.ClientName()]; ok {
			return false
		}
	}

	return true
}

// UnaryInterceptor returns a Unary Interceptor
func (c *LimiterMiddleware) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		lockKey := c.getLockKey(ctx)
		if lockKey == "" {
			return handler(ctx, req)
		}

		// Check if request is authenticated
		limiter := c.methodLimiters[info.FullMethod]
		unauthLimiter, ok := c.methodLimitersUnauthenticated[info.FullMethod]
		if c.useUnauthenticatedLimiter(ctx) && ok {
			limiter = unauthLimiter
		}

		if limiter == nil {
			// No concurrency limiting
			return handler(ctx, req)
		}

		return limiter.Limit(ctx, lockKey, func() (interface{}, error) {
			return handler(ctx, req)
		})
	}
}

// StreamInterceptor returns a Stream Interceptor
func (c *LimiterMiddleware) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		wrapper := &wrappedStream{stream, info, c, true}
		return handler(srv, wrapper)
	}
}

// SetupFunc set up a middleware to limiting requests
type SetupFunc func(cfg config.Cfg, middleware *LimiterMiddleware)

type wrappedStream struct {
	grpc.ServerStream
	info              *grpc.StreamServerInfo
	limiterMiddleware *LimiterMiddleware
	initial           bool
}

func (w *wrappedStream) RecvMsg(m interface{}) error {
	if err := w.ServerStream.RecvMsg(m); err != nil {
		return err
	}

	// Only perform limiting on the first request of a stream
	if !w.initial {
		return nil
	}

	w.initial = false

	ctx := w.Context()

	lockKey := w.limiterMiddleware.getLockKey(ctx)
	if lockKey == "" {
		return nil
	}

	// Check if request is authenticated
	limiter := w.limiterMiddleware.methodLimiters[w.info.FullMethod]
	unauthLimiter, ok := w.limiterMiddleware.methodLimitersUnauthenticated[w.info.FullMethod]
	if w.limiterMiddleware.useUnauthenticatedLimiter(ctx) && ok {
		limiter = unauthLimiter
	}

	if limiter == nil {
		// No concurrency limiting
		return nil
	}

	ready := make(chan struct{})
	errs := make(chan error, 1)
	go func() {
		if _, err := limiter.Limit(ctx, lockKey, func() (interface{}, error) {
			close(ready)
			<-ctx.Done()
			return nil, nil
		}); err != nil {
			errs <- err
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ready:
		// It's our turn!
		return nil
	case err := <-errs:
		return err
	}
}

// PerRPCLimits holds the limits of the per-RPC concurrency limiters, keyed by the full method of
// the RPC.
type PerRPCLimits struct {
	// Authenticated holds the per-repository limits of authenticated requests.
	Authenticated map[string]*limiter.AdaptiveLimit
	// AuthenticatedGlobal holds the limits shared by authenticated requests across the node.
	AuthenticatedGlobal map[string]*limiter.AdaptiveLimit
	// Unauthenticated holds the per-repository limits of unauthenticated requests.
	Unauthenticated map[string]*limiter.AdaptiveLimit
	// UnauthenticatedGlobal holds the limits shared by unauthenticated requests across the node.
	UnauthenticatedGlobal map[string]*limiter.AdaptiveLimit
}

// WithConcurrencyLimiters sets up middleware to limit the concurrency of
// requests based on RPC and repository
func WithConcurrencyLimiters(cfg config.Cfg) (PerRPCLimits, SetupFunc) {
	limits := PerRPCLimits{
		Authenticated:         map[string]*limiter.AdaptiveLimit{},
		AuthenticatedGlobal:   map[string]*limiter.AdaptiveLimit{},
		Unauthenticated:       map[string]*limiter.AdaptiveLimit{},
		UnauthenticatedGlobal: map[string]*limiter.AdaptiveLimit{},
	}

	for _, concurrency := range cfg.Concurrency {
		// Create authenticated limiter
		limitName := fmt.Sprintf("perRPC%s", concurrency.RPC)
		if concurrency.Adaptive {
			limits.Authenticated[concurrency.RPC] = limiter.NewAdaptiveLimit(limitName, limiter.AdaptiveSetting{
				Initial:       concurrency.InitialLimit,
				Max:           concurrency.MaxLimit,
				Min:           concurrency.MinLimit,
				BackoffFactor: limiter.DefaultBackoffFactor,
			})
		} else {
			limits.Authenticated[concurrency.RPC] = limiter.NewAdaptiveLimit(limitName, limiter.AdaptiveSetting{
				Initial: concurrency.Concurrency(),
			})
		}

		// Create the node-wide authenticated limiter shared across all repositories if configured
		if globalLimits := concurrency.Global; globalLimits.IsSet() {
			limitNameGlobal := fmt.Sprintf("perRPC%s-global", concurrency.RPC)
			if globalLimits.Adaptive {
				limits.AuthenticatedGlobal[concurrency.RPC] = limiter.NewAdaptiveLimit(limitNameGlobal, limiter.AdaptiveSetting{
					Initial:       globalLimits.InitialLimit,
					Max:           globalLimits.MaxLimit,
					Min:           globalLimits.MinLimit,
					BackoffFactor: limiter.DefaultBackoffFactor,
				})
			} else {
				limits.AuthenticatedGlobal[concurrency.RPC] = limiter.NewAdaptiveLimit(limitNameGlobal, limiter.AdaptiveSetting{
					Initial: globalLimits.Concurrency(),
				})
			}
		}

		// Create unauthenticated limiter if configured
		unauthLimits := concurrency.Unauthenticated
		if unauthLimits.IsSet() {
			limitNameUnauth := fmt.Sprintf("perRPC%s-unauthenticated", concurrency.RPC)
			if unauthLimits.Adaptive {
				limits.Unauthenticated[concurrency.RPC] = limiter.NewAdaptiveLimit(limitNameUnauth, limiter.AdaptiveSetting{
					Initial:       unauthLimits.InitialLimit,
					Max:           unauthLimits.MaxLimit,
					Min:           unauthLimits.MinLimit,
					BackoffFactor: limiter.DefaultBackoffFactor,
				})
			} else {
				limits.Unauthenticated[concurrency.RPC] = limiter.NewAdaptiveLimit(limitNameUnauth, limiter.AdaptiveSetting{
					Initial: unauthLimits.Concurrency(),
				})
			}

			// Create the node-wide limiter shared across all repositories if configured
			if globalLimits := unauthLimits.Global; globalLimits.IsSet() {
				limitNameGlobal := fmt.Sprintf("perRPC%s-unauthenticated-global", concurrency.RPC)
				if globalLimits.Adaptive {
					limits.UnauthenticatedGlobal[concurrency.RPC] = limiter.NewAdaptiveLimit(limitNameGlobal, limiter.AdaptiveSetting{
						Initial:       globalLimits.InitialLimit,
						Max:           globalLimits.MaxLimit,
						Min:           globalLimits.MinLimit,
						BackoffFactor: limiter.DefaultBackoffFactor,
					})
				} else {
					limits.UnauthenticatedGlobal[concurrency.RPC] = limiter.NewAdaptiveLimit(limitNameGlobal, limiter.AdaptiveSetting{
						Initial: globalLimits.Concurrency(),
					})
				}
			}
		}
	}
	return limits, func(cfg config.Cfg, middleware *LimiterMiddleware) {
		acquiringSecondsMetric := prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "gitaly",
				Subsystem: "concurrency_limiting",
				Name:      "acquiring_seconds",
				Help:      "Histogram of time calls are rate limited (in seconds)",
				Buckets:   cfg.Prometheus.GRPCLatencyBuckets,
			},
			[]string{"system", "grpc_service", "grpc_method", "authenticated"},
		)
		inProgressMetric := prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "gitaly",
				Subsystem: "concurrency_limiting",
				Name:      "in_progress",
				Help:      "Gauge of number of concurrent in-progress calls",
			},
			[]string{"system", "grpc_service", "grpc_method", "authenticated"},
		)
		queuedMetric := prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "gitaly",
				Subsystem: "concurrency_limiting",
				Name:      "queued",
				Help:      "Gauge of number of queued calls",
			},
			[]string{"system", "grpc_service", "grpc_method", "authenticated"},
		)

		middleware.collect = func(metrics chan<- prometheus.Metric) {
			acquiringSecondsMetric.Collect(metrics)
			inProgressMetric.Collect(metrics)
			queuedMetric.Collect(metrics)
		}

		result := make(map[string]limiter.Limiter)
		resultUnauthenticated := make(map[string]limiter.Limiter)

		for _, concurrency := range cfg.Concurrency {
			// Create authenticated limiter
			var authOpts []limiter.Option
			if globalLimit, ok := limits.AuthenticatedGlobal[concurrency.RPC]; ok {
				authOpts = append(authOpts, limiter.WithGlobalLimit(
					globalLimit,
					concurrency.Global.MaxQueueSize,
					concurrency.Global.MaxQueueWait.Duration(),
					limiter.NewPerRPCPromMonitor(
						"gitaly", concurrency.RPC,
						queuedMetric, inProgressMetric, acquiringSecondsMetric, middleware.requestsDroppedMetric,
						limiter.LabelAuthenticatedGlobal,
					),
				))
			}

			result[concurrency.RPC] = limiter.NewConcurrencyLimiter(
				limits.Authenticated[concurrency.RPC],
				concurrency.MaxQueueSize,
				concurrency.MaxQueueWait.Duration(),
				limiter.NewPerRPCPromMonitor(
					"gitaly", concurrency.RPC,
					queuedMetric, inProgressMetric, acquiringSecondsMetric, middleware.requestsDroppedMetric,
					limiter.LabelAuthenticated,
				),
				authOpts...,
			)

			// Create unauthenticated limiter if configured
			if adaptiveLimit, ok := limits.Unauthenticated[concurrency.RPC]; ok {
				unauthLimits := concurrency.Unauthenticated

				var opts []limiter.Option
				if globalLimit, ok := limits.UnauthenticatedGlobal[concurrency.RPC]; ok {
					opts = append(opts, limiter.WithGlobalLimit(
						globalLimit,
						unauthLimits.Global.MaxQueueSize,
						unauthLimits.Global.MaxQueueWait.Duration(),
						limiter.NewPerRPCPromMonitor(
							"gitaly", concurrency.RPC,
							queuedMetric, inProgressMetric, acquiringSecondsMetric, middleware.requestsDroppedMetric,
							limiter.LabelUnauthenticatedGlobal,
						),
					))
				}

				resultUnauthenticated[concurrency.RPC] = limiter.NewConcurrencyLimiter(
					adaptiveLimit,
					unauthLimits.MaxQueueSize,
					unauthLimits.MaxQueueWait.Duration(),
					limiter.NewPerRPCPromMonitor(
						"gitaly", concurrency.RPC,
						queuedMetric, inProgressMetric, acquiringSecondsMetric, middleware.requestsDroppedMetric,
						limiter.LabelUnauthenticated,
					),
					opts...,
				)
			}
		}

		// Set default for ReplicateRepository.
		replicateRepositoryFullMethod := "/gitaly.RepositoryService/ReplicateRepository"
		if _, ok := result[replicateRepositoryFullMethod]; !ok {
			result[replicateRepositoryFullMethod] = limiter.NewConcurrencyLimiter(
				limiter.NewAdaptiveLimit("staticLimit", limiter.AdaptiveSetting{Initial: 1}),
				0,
				0,
				limiter.NewPerRPCPromMonitor(
					"gitaly", replicateRepositoryFullMethod,
					queuedMetric, inProgressMetric, acquiringSecondsMetric, middleware.requestsDroppedMetric,
					limiter.LabelAuthenticated,
				),
			)
		}

		middleware.methodLimiters = result
		middleware.methodLimitersUnauthenticated = resultUnauthenticated
	}
}
