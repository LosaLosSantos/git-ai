// Package director composes proxy grants and authorization profiles into a
// gRPC stream director.
//
// The director does not enforce grant expiry or concurrency limits. Hosts own
// session lifetime and admission because ExpiresIn is relative to grant issue
// time and MaxConcurrentStreams applies across streams.
package director

import (
	"context"
	"errors"
	"regexp"

	"github.com/feder-cr/git-ai/proxy"
	"github.com/feder-cr/git-ai/proxy/grant"
	"github.com/feder-cr/git-ai/proxy/policy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Dialer connects to the Gitaly coordinates authorized by a grant.
type Dialer interface {
	Dial(context.Context, grant.Coordinates) (*grpc.ClientConn, error)
}

// DialFunc adapts a function to Dialer.
type DialFunc func(context.Context, grant.Coordinates) (*grpc.ClientConn, error)

// Dial implements Dialer.
func (f DialFunc) Dial(ctx context.Context, coordinates grant.Coordinates) (*grpc.ClientConn, error) {
	return f(ctx, coordinates)
}

// GrantResolver returns the grant bound to an incoming stream. Implementations
// may use the stream context to resolve transport-owned session state.
type GrantResolver interface {
	Resolve(context.Context) (*grant.Grant, error)
}

// GrantResolverFunc adapts a function to GrantResolver.
type GrantResolverFunc func(context.Context) (*grant.Grant, error)

// Resolve implements GrantResolver.
func (f GrantResolverFunc) Resolve(ctx context.Context) (*grant.Grant, error) {
	return f(ctx)
}

// CorrelationIDProvider returns a host-validated correlation ID for an
// incoming stream. Valid IDs contain at most 128 ASCII letters, digits,
// underscores, or hyphens. Empty or invalid values are omitted.
type CorrelationIDProvider interface {
	CorrelationID(context.Context) string
}

// CorrelationIDProviderFunc adapts a function to CorrelationIDProvider.
type CorrelationIDProviderFunc func(context.Context) string

// CorrelationID implements CorrelationIDProvider.
func (f CorrelationIDProviderFunc) CorrelationID(ctx context.Context) string {
	return f(ctx)
}

const (
	correlationIDKey       = "x-gitlab-correlation-id"
	maxCorrelationIDLength = 128
)

var correlationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type config struct {
	correlationIDProvider CorrelationIDProvider
}

// Option configures a stream director.
type Option func(*config)

// WithCorrelationIDProvider configures a host hook for propagating a validated
// correlation ID without forwarding any other incoming metadata.
func WithCorrelationIDProvider(provider CorrelationIDProvider) Option {
	return func(cfg *config) {
		cfg.correlationIDProvider = provider
	}
}

// New returns a stream director that authorizes each stream before dialing
// Gitaly. Inbound metadata is discarded; only grant call metadata and an
// optional host-validated correlation ID are sent to the destination.
func New(dialer Dialer, resolver GrantResolver, options ...Option) proxy.StreamDirector {
	var cfg config
	for _, option := range options {
		option(&cfg)
	}

	return func(ctx context.Context, fullMethod string, peeker proxy.StreamPeeker) (*proxy.StreamParameters, error) {
		resolvedGrant, err := resolver.Resolve(ctx)
		if err != nil {
			return nil, err
		}
		if resolvedGrant == nil {
			return nil, policy.Denial("no_grant", nil)
		}

		profile, ok := policy.LookupProfile(resolvedGrant.Authorization.Profile)
		if !ok {
			return nil, policy.Denial("unknown_profile", nil)
		}
		method, err := policy.CheckUniversalInvariants(fullMethod)
		if err != nil {
			return nil, err
		}
		if err := profile.CheckMethod(method); err != nil {
			return nil, err
		}
		if !policy.NewAllowlist(resolvedGrant.Authorization.AllowedMethods).Allows(fullMethod) {
			return nil, policy.Denial("method_not_allowed", nil)
		}

		frame, err := peeker.Peek()
		if err != nil {
			return nil, newStreamError(codes.Internal, "peek_failed", err)
		}
		if err := profile.CheckRequest(method, frame, resolvedGrant.Authorization.Repository); err != nil {
			if errors.Is(err, policy.ErrMalformedRequest) {
				return nil, policy.Denial("malformed_request", err)
			}
			return nil, err
		}

		outgoingMetadata := metadata.New(resolvedGrant.Coordinates.CallMetadata)
		outgoingMetadata.Delete(correlationIDKey)
		if cfg.correlationIDProvider != nil {
			if correlationID := cfg.correlationIDProvider.CorrelationID(ctx); validCorrelationID(correlationID) {
				outgoingMetadata.Set(correlationIDKey, correlationID)
			}
		}
		outgoingContext := metadata.NewOutgoingContext(ctx, outgoingMetadata)
		connection, err := dialer.Dial(outgoingContext, resolvedGrant.Coordinates)
		if err != nil {
			return nil, newStreamError(codes.Unavailable, "gitaly_unavailable", err)
		}

		return proxy.NewStreamParameters(proxy.Destination{
			Ctx:  outgoingContext,
			Conn: connection,
			Msg:  frame,
		}, nil, nil, []grpc.CallOption{grpc.ForceCodec(proxy.NewCodec())}), nil
	}
}

func validCorrelationID(correlationID string) bool {
	return len(correlationID) <= maxCorrelationIDLength && correlationIDPattern.MatchString(correlationID)
}

type streamError struct {
	code   codes.Code
	reason string
	cause  error
}

func newStreamError(code codes.Code, reason string, cause error) error {
	return &streamError{code: code, reason: reason, cause: cause}
}

func (err *streamError) Error() string { return "proxy: " + err.reason }

func (err *streamError) Unwrap() error { return err.cause }

func (err *streamError) GRPCStatus() *status.Status {
	return status.New(err.code, err.Error())
}

var (
	_ Dialer                = DialFunc(nil)
	_ GrantResolver         = GrantResolverFunc(nil)
	_ CorrelationIDProvider = CorrelationIDProviderFunc(nil)
)
