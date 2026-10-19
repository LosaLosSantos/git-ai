package sqlstore

import (
	"context"
	"fmt"
	"time"
)

// The CLOCK RULE of doc/graft/PLAN7.md: every time that decides a garbage
// collection action (a checkpoint's creation, an orphan's first observation, a
// tombstone, a lease) is a time of the database, written and compared on the
// database's clock, so that the clocks of the nodes never enter the decision.
// SQLite serves the nodes of one host, whose clock it is.

// wallTime returns the current database time, for a time that a write records.
// On Postgres it is clock_timestamp(), the time of the statement rather than of
// the start of the transaction, so that a recorded time is never earlier than
// what the transaction waited for before. On SQLite it is the host clock.
func (s *Store) wallTime(ctx context.Context, q querier) (time.Time, error) {
	if s.dialect != Postgres {
		return time.Now().UTC(), nil
	}

	var raw any
	if err := q.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&raw); err != nil {
		return time.Time{}, fmt.Errorf("read the database time: %w", err)
	}
	return scanTime(raw)
}

// snapshotTime returns the database time of a read transaction: a time no
// later than its snapshot, so that every row committed before that time is
// visible in it. On Postgres it is now(), the start of the transaction, which
// precedes the snapshot taken by the transaction's first statement. On SQLite
// it is hostNow, the host clock that the caller read before the transaction
// began (a deferred transaction takes its snapshot at its first read).
func (s *Store) snapshotTime(ctx context.Context, q querier, hostNow time.Time) (time.Time, error) {
	if s.dialect != Postgres {
		return hostNow.UTC(), nil
	}

	var raw any
	if err := q.QueryRowContext(ctx, "SELECT now()").Scan(&raw); err != nil {
		return time.Time{}, fmt.Errorf("read the database time: %w", err)
	}
	return scanTime(raw)
}
