// Package sentryskip carries the marker that tells the Sentry handler not to report an error. It is a separate package
// so that importers, such as the exported proxy package, do not depend on the Sentry SDK.
package sentryskip

import "context"

type key struct{}

// WithMarker returns a context that can be marked with MarkToSkip.
func WithMarker(ctx context.Context) context.Context {
	return context.WithValue(ctx, key{}, new(bool))
}

// MarkToSkip signals to the Sentry handler that the error must not be reported. It is a no-op if the context has no
// marker.
func MarkToSkip(ctx context.Context) {
	if skip, ok := ctx.Value(key{}).(*bool); ok {
		*skip = true
	}
}

// ShouldSkip reports whether MarkToSkip was called on the context.
func ShouldSkip(ctx context.Context) bool {
	skip, ok := ctx.Value(key{}).(*bool)
	return ok && *skip
}
