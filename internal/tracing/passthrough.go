package tracing

import (
	"context"
	"errors"
	"strings"

	"gitlab.com/gitlab-org/labkit/correlation"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const correlationIDEnvKey = "CORRELATION_ID"

var errNoSpanContextInEnv = errors.New("no span context found in environment variables")

// PropagateFromEnv propagates the SpanContext and correlation ID extracted from
// environment variables into a new context that can later be used to create
// child spans and propagate the correlation ID on outgoing gRPC calls.
func PropagateFromEnv(envs []string) (context.Context, error) {
	envMap := environAsMap(envs)
	carrier := propagation.MapCarrier(envMap)

	propagator := defaultPropagator

	ctx := context.Background()
	spanCtx := propagator.Extract(ctx, carrier)

	hasSpan := trace.SpanContextFromContext(spanCtx).IsValid()
	if hasSpan {
		ctx = spanCtx
	}

	if correlationID, ok := envMap[correlationIDEnvKey]; ok && correlationID != "" {
		ctx = correlation.ContextWithCorrelation(ctx, correlationID)
		return ctx, nil
	}

	if hasSpan {
		return ctx, nil
	}

	return ctx, errNoSpanContextInEnv
}

// UnaryPassthroughInterceptor is a client gRPC unary interceptor that injects a span context into
// the outgoing context of the call. It is useful to propagate span context between processes that do
// not have automatic span propagation between them.
func UnaryPassthroughInterceptor(contextToInject context.Context) grpc.UnaryClientInterceptor {
	return func(requestCtx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctxWithMetadata := injectSpanContextIntoContext(requestCtx, contextToInject)
		return invoker(ctxWithMetadata, method, req, reply, cc, opts...)
	}
}

// StreamPassthroughInterceptor is equivalent to UnaryPassthroughInterceptor, but for streaming gRPC calls.
func StreamPassthroughInterceptor(contextToInject context.Context) grpc.StreamClientInterceptor {
	return func(requestCtx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctxWithMetadata := injectSpanContextIntoContext(requestCtx, contextToInject)
		return streamer(ctxWithMetadata, desc, cc, method, opts...)
	}
}

// injectSpanContextIntoContext inject the span context included in `contextToInject` into `ctx`.
// Because context are immutable objects, the returned context is a new context with `ctx` as its parent
// and includes the span context extracted from `contextToInject`.
func injectSpanContextIntoContext(ctx context.Context, contextToInject context.Context) context.Context {
	grpcMetaData, _ := metadata.FromOutgoingContext(ctx)
	defaultPropagator.Inject(contextToInject, grpcMetadataCarrier(grpcMetaData))
	return metadata.NewOutgoingContext(ctx, grpcMetaData)
}

// InjectIntoEnv injects the span context and correlation ID from ctx into the
// environment variable slice and returns the extended slice. Each propagated
// key-value pair is appended in "key=value" format. Safe to call when ctx
// carries neither a span nor a correlation ID; in that case the slice is
// returned unchanged.
func InjectIntoEnv(ctx context.Context, envs []string) []string {
	carrier := propagation.MapCarrier{}
	defaultPropagator.Inject(ctx, carrier)
	for k, v := range carrier {
		envs = append(envs, k+"="+v)
	}

	if correlationID := correlation.ExtractFromContext(ctx); correlationID != "" {
		envs = append(envs, correlationIDEnvKey+"="+correlationID)
	}

	return envs
}

// environAsMap takes a list of environment variable in the format `key=value`
// and splits each value using the `=` sign such as to return a map with each
// key associated with its value.
func environAsMap(env []string) map[string]string {
	envMap := make(map[string]string, len(env))
	for _, v := range env {
		s := strings.SplitN(v, "=", 2)
		if len(s) == 2 {
			envMap[s[0]] = s[1]
		}
	}
	return envMap
}

// grpcMetadataCarrier implements propagation.TextMapCarrier. The purpose of this
// custom implementation is to convert all metadata keys into lowercase. The default
// OTEL implementation of HeaderCarrier converts all keys into HTTP Headers using the
// canonical form. See:
// * https://github.com/open-telemetry/opentelemetry-go/blob/main/propagation/propagation.go#L92
// * https://github.com/golang/go/blob/master/src/net/http/header.go#L40
//
// However, in gRPC, only lowerkey metadata keys are accepted. The character
// set is `[0-9-a-z-_.]`. See:
// * https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md
type grpcMetadataCarrier metadata.MD

func (g grpcMetadataCarrier) Get(key string) string {
	values := metadata.MD(g).Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (g grpcMetadataCarrier) Set(key string, value string) {
	metadata.MD(g).Set(strings.ToLower(key), value)
}

func (g grpcMetadataCarrier) Keys() []string {
	var keys []string
	for k := range g {
		keys = append(keys, k)
	}
	return keys
}
