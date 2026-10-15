// Package identity carries the authenticated caller identity of a request
// through its context. The gRPC auth layer stores it (see
// internal/gitaly/server/auth); hooks, the standalone access policy and the
// changeset log read it.
package identity

import "context"

// Source tells how an identity was established.
type Source string

const (
	// SourceAgent is an identity authenticated with a per-agent token
	// configured under [[graft.agent]].
	SourceAgent Source = "agent"
	// SourceSharedToken is a caller authenticated with the shared auth.token.
	SourceSharedToken Source = "shared_token"
	// SourceUser is an identity taken from the request's User message
	// (gl_id / gl_username), trusted because the caller was authenticated.
	SourceUser Source = "user"
)

// Identity is an authenticated caller.
type Identity struct {
	// Name identifies the caller, e.g. the agent name.
	Name string
	// Source tells how the identity was established.
	Source Source
}

type contextKey struct{}

// NewContext returns a copy of ctx carrying id.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// FromContext returns the identity stored in ctx, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok && id.Name != ""
}

// Actor returns the name of the identity stored in ctx, or fallback if there
// is none.
func Actor(ctx context.Context, fallback string) string {
	if id, ok := FromContext(ctx); ok {
		return id.Name
	}
	return fallback
}
