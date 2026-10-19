package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
)

// allocatorPosition returns the greatest repository ID allocated, 0 for none.
// The Postgres sequence is not transactional: it counts the IDs of creations
// whose row is not committed yet. SQLite's is: an uncommitted creation's ID is
// not counted, and a rolled back one is allocated again.
func (s *Store) allocatorPosition(ctx context.Context, q querier) (int64, error) {
	query := "SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'graft_repositories'), 0)"
	if s.dialect == Postgres {
		query = "SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM graft_repositories_repo_id_seq"
	}
	var position int64
	if err := q.QueryRowContext(ctx, query).Scan(&position); err != nil {
		return 0, fmt.Errorf("read the repository ID allocator: %w", err)
	}
	return position, nil
}

// ObserveAllocator implements corestore.MetaStore.
func (s *Store) ObserveAllocator(ctx context.Context, minAge time.Duration, dryRun bool) (corestore.AllocatorView, error) {
	if minAge < 0 {
		return corestore.AllocatorView{}, fmt.Errorf("storage core: negative allocation age %s", minAge)
	}

	var view corestore.AllocatorView
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		view = corestore.AllocatorView{}

		now, err := s.wallTime(ctx, tx)
		if err != nil {
			return err
		}
		position, err := s.allocatorPosition(ctx, tx)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT done_through FROM graft_gc_reconcile WHERE id = 1").Scan(&view.DoneThrough); err != nil {
			return fmt.Errorf("read the reconciliation cursor: %w", err)
		}

		// The marks are few (the newest one at least minAge old and those
		// after it), and compared here: SQLite stores times as text whose
		// fractions do not sort.
		type mark struct {
			key        any
			observedAt time.Time
			position   int64
		}
		rows, err := tx.QueryContext(ctx, "SELECT observed_at, position FROM graft_gc_allocator_marks")
		if err != nil {
			return fmt.Errorf("read the allocator marks: %w", err)
		}
		var marks []mark
		for rows.Next() {
			var m mark
			if err := rows.Scan(&m.key, &m.position); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan an allocator mark: %w", err)
			}
			if m.observedAt, err = scanTime(m.key); err != nil {
				_ = rows.Close()
				return err
			}
			marks = append(marks, m)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("read the allocator marks: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read the allocator marks: %w", err)
		}

		limit := now.Add(-minAge)
		var newest *mark
		for i := range marks {
			m := &marks[i]
			if m.observedAt.After(limit) {
				continue
			}
			if m.position > view.Bound {
				view.Bound = m.position
			}
			if newest == nil || m.observedAt.After(newest.observedAt) {
				newest = m
			}
		}
		view.Now, view.Position = now, position
		if dryRun {
			return nil
		}

		if _, err := tx.ExecContext(ctx, s.bind(`
			INSERT INTO graft_gc_allocator_marks (observed_at, position) VALUES (?, ?)
			ON CONFLICT (observed_at) DO NOTHING
		`), s.timeArg(now), position); err != nil {
			return fmt.Errorf("record the allocator mark: %w", err)
		}
		// A mark older than the newest eligible one can never give a greater
		// bound: positions only grow.
		for _, m := range marks {
			if newest == nil || !m.observedAt.Before(newest.observedAt) {
				continue
			}
			if _, err := tx.ExecContext(ctx, s.bind("DELETE FROM graft_gc_allocator_marks WHERE observed_at = ?"), m.key); err != nil {
				return fmt.Errorf("drop an old allocator mark: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return corestore.AllocatorView{}, err
	}
	return view, nil
}

// UnrecordedIDs implements corestore.MetaStore.
func (s *Store) UnrecordedIDs(ctx context.Context, after, upTo int64) ([]int64, error) {
	if upTo <= after {
		return nil, nil
	}

	var ids []int64
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		ids = nil

		recorded := map[int64]struct{}{}
		for _, query := range []string{
			"SELECT repo_id FROM graft_repositories WHERE repo_id > ? AND repo_id <= ?",
			"SELECT repo_id FROM graft_gc_removed WHERE repo_id > ? AND repo_id <= ?",
		} {
			rows, err := tx.QueryContext(ctx, s.bind(query), after, upTo)
			if err != nil {
				return fmt.Errorf("read the recorded repository IDs: %w", err)
			}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan a repository ID: %w", err)
				}
				recorded[id] = struct{}{}
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("read the recorded repository IDs: %w", err)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("read the recorded repository IDs: %w", err)
			}
		}
		for id := after + 1; id <= upTo; id++ {
			if _, ok := recorded[id]; !ok {
				ids = append(ids, id)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// RecordLeak implements corestore.MetaStore.
func (s *Store) RecordLeak(ctx context.Context, repoID int64) (bool, error) {
	var recorded bool
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		recorded = false

		now, err := s.wallTime(ctx, tx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.bind(`
			INSERT INTO graft_gc_removed (repo_id, storage, relative_path, removed_at)
			SELECT ?, '', '', ? WHERE NOT EXISTS (SELECT 1 FROM graft_repositories WHERE repo_id = ?)
			ON CONFLICT (repo_id) DO NOTHING
		`), repoID, s.timeArg(now), repoID)
		if err != nil {
			return fmt.Errorf("record the tombstone of leaked repository %d: %w", repoID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("record the tombstone of leaked repository %d: %w", repoID, err)
		}
		recorded = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return recorded, nil
}

// AdvanceReconcile implements corestore.MetaStore.
func (s *Store) AdvanceReconcile(ctx context.Context, doneThrough int64) error {
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.bind("UPDATE graft_gc_reconcile SET done_through = ? WHERE id = 1 AND done_through < ?"),
			doneThrough, doneThrough); err != nil {
			return fmt.Errorf("advance the reconciliation cursor: %w", err)
		}
		return nil
	})
}
