package mvcc

import (
	"context"
)

type cacheContextKey struct{}

// ContextWithCache returns a new context with the Cache stored in it. A nil Cache
// is not added into the context, and the passed context is returned as is.
func ContextWithCache(ctx context.Context, c Cache) context.Context {
	return context.WithValue(ctx, cacheContextKey{}, c)
}

// ContextWithoutCache returns a context that carries no Cache, even if ctx does.
// Work that is detached from the request it originated in must not inherit that
// request's Cache: it outlives the request, so committing through that Cache would
// race with the request's own Commit, and it needs to operate on whichever manifest
// is current when it runs rather than the one the request pinned.
func ContextWithoutCache(ctx context.Context) context.Context {
	if CacheFromContext(ctx) == nil {
		return ctx
	}

	return context.WithValue(ctx, cacheContextKey{}, nil)
}

// CacheFromContext returns the Cache in the context, or nil if not found.
func CacheFromContext(ctx context.Context) Cache {
	if c, ok := ctx.Value(cacheContextKey{}).(Cache); ok {
		return c
	}
	return nil
}
