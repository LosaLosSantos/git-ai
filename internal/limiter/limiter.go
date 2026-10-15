package limiter

import "context"

// Limiter limits incoming requests
type Limiter interface {
	Limit(ctx context.Context, lockKey string, f LimitedFunc) (interface{}, error)
}

// LimitedFunc represents a function that will be limited
type LimitedFunc func() (resp interface{}, err error)
