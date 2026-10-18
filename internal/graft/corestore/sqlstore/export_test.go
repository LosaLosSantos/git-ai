package sqlstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// OpenV010 opens the database with only the schema of v0.1.0 (the first
// migration), as a v0.1.0 binary left it: no checkpoints, no tombstones and no
// owned-key trigger.
func OpenV010(ctx context.Context, dsn string) (*Store, error) {
	return open(ctx, dsn, 1)
}

// SetRetryHook installs fn as the hook called for every retried transaction and
// returns the function that removes it.
func SetRetryHook(fn func(err error)) (restore func()) {
	retryHook = fn
	return func() { retryHook = nil }
}

// IsDeadlock reports whether err is a Postgres deadlock (40P01).
func IsDeadlock(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "40P01"
}

// NotOwnedPackKeyMessage is the documented error of the owned-key trigger.
const NotOwnedPackKeyMessage = notOwnedPackKeyMessage
