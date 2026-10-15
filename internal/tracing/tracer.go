package tracing

import (
	"context"
	"io"
	"os"

	labkitv2trace "gitlab.com/gitlab-org/labkit/v2/trace"
	"go.opentelemetry.io/contrib/propagators/jaeger"
	"go.opentelemetry.io/contrib/propagators/ot"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// gitlabTracingEnvVar is the environment variable read by LabKit v2/trace to
// configure the tracing backend. Supported format:
//
//	otlp://host:port        HTTP (insecure)
//	otlps://host:port       HTTPS (secure)
//	otlp+tls://host:port    HTTPS (secure)
const gitlabTracingEnvVar = "GITLAB_TRACING"

// defaultPropagator is the default TextMapPropagator used for context propagation.
// Propagators are objects responsible for propagating tracing data.
// Note: contrary to OpenTracing, OpenTelemetry propagates baggage within contexts, not within spans.
var defaultPropagator = propagation.NewCompositeTextMapPropagator(
	// TraceContext propagator propagates trace IDs across process boundaries.
	propagation.TraceContext{},

	// Baggage propagator propagates baggage across process boundaries.
	propagation.Baggage{},

	// OpenTracing propagator is needed because other components at GitLab still use it.
	ot.OT{},

	// Jaeger/Uber propagator is needed because other components at GitLab still use it.
	jaeger.Jaeger{},
)

// InitializeTracerProvider creates an OpenTelemetry tracer provider using LabKit v2/trace.
// Configuration is read from the GITLAB_TRACING environment variable.
//
// Supported formats:
//
//	otlp://host:port        HTTP (insecure)
//	otlps://host:port       HTTPS (secure)
//	otlp+tls://host:port    HTTPS (secure)
//
// Returns an io.Closer to flush and shut down the provider and an error if
// initialization fails. When GITLAB_TRACING is unset or empty, tracing is
// disabled and a noop closer is returned, preserving the previous behaviour.
func InitializeTracerProvider(ctx context.Context, serviceName string) (io.Closer, error) {
	if os.Getenv(gitlabTracingEnvVar) == "" {
		return io.NopCloser(nil), nil
	}

	_, shutdown, err := labkitv2trace.NewWithConfig(ctx, &labkitv2trace.Config{
		ServiceName: serviceName,
	})
	if err != nil {
		return io.NopCloser(nil), err
	}

	// Override the global propagator to include OpenTracing and Jaeger propagators
	// for backward compatibility with other GitLab components that have not yet
	// migrated to W3C TraceContext.
	otel.SetTextMapPropagator(defaultPropagator)

	return &tracerShutdown{shutdown: shutdown}, nil
}

type tracerShutdown struct {
	shutdown func(context.Context) error
}

func (s *tracerShutdown) Close() error {
	return s.shutdown(context.Background())
}
