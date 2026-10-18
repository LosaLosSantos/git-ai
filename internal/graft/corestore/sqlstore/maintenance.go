package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
)

// lease is the maintenance lease of a repository as stored.
type lease struct {
	holder    string
	expiresAt time.Time
}

// readLease reads the lease of the repository; ok is false when there is none.
func (s *Store) readLease(ctx context.Context, q querier, repoID int64) (lease, bool, error) {
	var (
		l          lease
		expiresRaw any
	)
	err := q.QueryRowContext(ctx, s.bind("SELECT holder, expires_at FROM graft_gc_leases WHERE repo_id = ?"), repoID).
		Scan(&l.holder, &expiresRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return lease{}, false, nil
	}
	if err != nil {
		return lease{}, false, fmt.Errorf("read maintenance lease: %w", err)
	}
	if l.expiresAt, err = scanTime(expiresRaw); err != nil {
		return lease{}, false, err
	}
	return l, true, nil
}

// validateLease checks the arguments of the lease methods.
func validateLease(holder string, ttl time.Duration) error {
	if holder == "" || !storableText(holder) {
		return fmt.Errorf("storage core: invalid maintenance lease holder %q", holder)
	}
	if ttl <= 0 {
		return fmt.Errorf("storage core: invalid maintenance lease duration %s", ttl)
	}
	return nil
}

// AcquireMaintenanceLease implements corestore.MetaStore.
func (s *Store) AcquireMaintenanceLease(ctx context.Context, repoID int64, holder string, ttl time.Duration) (bool, error) {
	if err := validateLease(holder, ttl); err != nil {
		return false, err
	}

	var acquired bool
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		acquired = false

		// LOCK ORDER (doc/graft/PLAN7.md): the repository row first; it also makes
		// the read and the write of the lease one compare-and-swap.
		if _, err := s.selectRepository(ctx, tx, "repo_id = ?", []any{repoID}, true); err != nil {
			return err
		}
		now, err := s.wallTime(ctx, tx)
		if err != nil {
			return err
		}

		current, ok, err := s.readLease(ctx, tx, repoID)
		if err != nil {
			return err
		}
		if ok && current.holder != holder && current.expiresAt.After(now) {
			return nil
		}

		if _, err := tx.ExecContext(ctx, s.bind(`
			INSERT INTO graft_gc_leases (repo_id, holder, acquired_at, expires_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (repo_id) DO UPDATE SET
				holder = excluded.holder,
				acquired_at = excluded.acquired_at,
				expires_at = excluded.expires_at
		`), repoID, holder, s.timeArg(now), s.timeArg(now.Add(ttl))); err != nil {
			return fmt.Errorf("write maintenance lease: %w", err)
		}
		acquired = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return acquired, nil
}

// RenewMaintenanceLease implements corestore.MetaStore.
func (s *Store) RenewMaintenanceLease(ctx context.Context, repoID int64, holder string, ttl time.Duration) (bool, error) {
	if err := validateLease(holder, ttl); err != nil {
		return false, err
	}

	var held bool
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		held = false

		if _, err := s.selectRepository(ctx, tx, "repo_id = ?", []any{repoID}, true); err != nil {
			return err
		}
		now, err := s.wallTime(ctx, tx)
		if err != nil {
			return err
		}

		current, ok, err := s.readLease(ctx, tx, repoID)
		if err != nil {
			return err
		}
		if !ok || current.holder != holder {
			return nil
		}

		if _, err := tx.ExecContext(ctx, s.bind("UPDATE graft_gc_leases SET expires_at = ? WHERE repo_id = ? AND holder = ?"),
			s.timeArg(now.Add(ttl)), repoID, holder); err != nil {
			return fmt.Errorf("renew maintenance lease: %w", err)
		}
		held = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return held, nil
}

// ReleaseMaintenanceLease implements corestore.MetaStore.
func (s *Store) ReleaseMaintenanceLease(ctx context.Context, repoID int64, holder string) error {
	if holder == "" || !storableText(holder) {
		return fmt.Errorf("storage core: invalid maintenance lease holder %q", holder)
	}

	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		_, err := s.selectRepository(ctx, tx, "repo_id = ?", []any{repoID}, true)
		if errors.Is(err, corestore.ErrRepositoryNotFound) {
			// The lease went with the repository row.
			return nil
		}
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, s.bind("DELETE FROM graft_gc_leases WHERE repo_id = ? AND holder = ?"), repoID, holder); err != nil {
			return fmt.Errorf("release maintenance lease: %w", err)
		}
		return nil
	})
}

// ListRemovedRepositories implements corestore.MetaStore.
func (s *Store) ListRemovedRepositories(ctx context.Context, storage string, minAge time.Duration) ([]corestore.RemovedRepository, error) {
	if minAge < 0 {
		return nil, fmt.Errorf("storage core: negative tombstone age %s", minAge)
	}

	hostNow := time.Now()
	var removed []corestore.RemovedRepository
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		removed = nil

		now, err := s.snapshotTime(ctx, tx, hostNow)
		if err != nil {
			return err
		}

		query := "SELECT repo_id, storage, relative_path, removed_at FROM graft_gc_removed"
		var args []any
		if storage != "" {
			query += " WHERE storage = ?"
			args = append(args, storage)
		}
		query += " ORDER BY repo_id"

		rows, err := tx.QueryContext(ctx, s.bind(query), args...)
		if err != nil {
			return fmt.Errorf("list removed repositories: %w", err)
		}
		defer rows.Close()

		limit := now.Add(-minAge)
		for rows.Next() {
			var (
				r          corestore.RemovedRepository
				removedRaw any
			)
			if err := rows.Scan(&r.RepoID, &r.Key.Storage, &r.Key.RelativePath, &removedRaw); err != nil {
				return fmt.Errorf("scan removed repository: %w", err)
			}
			if r.RemovedAt, err = scanTime(removedRaw); err != nil {
				return err
			}
			if r.RemovedAt.After(limit) {
				continue
			}
			removed = append(removed, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// DeleteRemovedRepository implements corestore.MetaStore.
func (s *Store) DeleteRemovedRepository(ctx context.Context, repoID int64) error {
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.bind("DELETE FROM graft_gc_removed WHERE repo_id = ?"), repoID); err != nil {
			return fmt.Errorf("delete tombstone: %w", err)
		}
		return nil
	})
}
