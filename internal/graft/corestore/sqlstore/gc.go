package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
)

// GCSnapshot implements corestore.MetaStore. Everything is read in one read
// transaction, with the database time of its snapshot: the garbage collector's
// proof that no live pack is deleted compares that time with the observations
// and relies on every row committed before it being visible
// (doc/graft/PLAN7.md, PACK OWNERSHIP RULE 5).
func (s *Store) GCSnapshot(ctx context.Context, repoID int64) (corestore.GCState, error) {
	hostNow := time.Now()

	var st corestore.GCState
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		st = corestore.GCState{Packs: map[string]uint64{}, Orphans: map[string]time.Time{}}

		now, err := s.snapshotTime(ctx, tx, hostNow)
		if err != nil {
			return err
		}
		st.Now = now

		if err := s.requireRepository(ctx, tx, repoID); err != nil {
			return err
		}

		// The greatest referencing sequence decides a key's liveness: a legacy key
		// can be shared by several changesets (doc/graft/PLAN7.md, change G).
		rows, err := tx.QueryContext(ctx, s.bind(`
			SELECT pack_key, MAX(seq) FROM graft_changesets
			WHERE repo_id = ? AND pack_key IS NOT NULL
			GROUP BY pack_key
		`), repoID)
		if err != nil {
			return fmt.Errorf("read changeset packs: %w", err)
		}
		for rows.Next() {
			var (
				key string
				seq int64
			)
			if err := rows.Scan(&key, &seq); err != nil {
				rows.Close()
				return fmt.Errorf("scan changeset pack: %w", err)
			}
			st.Packs[key] = uint64(seq)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read changeset packs: %w", err)
		}
		rows.Close()

		if st.Checkpoints, err = s.listCheckpoints(ctx, tx, repoID); err != nil {
			return err
		}

		orphans, err := tx.QueryContext(ctx, s.bind("SELECT pack_key, first_seen FROM graft_gc_orphans WHERE repo_id = ?"), repoID)
		if err != nil {
			return fmt.Errorf("read orphan observations: %w", err)
		}
		defer orphans.Close()
		for orphans.Next() {
			var (
				key     string
				seenRaw any
			)
			if err := orphans.Scan(&key, &seenRaw); err != nil {
				return fmt.Errorf("scan orphan observation: %w", err)
			}
			seen, err := scanTime(seenRaw)
			if err != nil {
				return err
			}
			st.Orphans[key] = seen
		}
		return orphans.Err()
	})
	if err != nil {
		return corestore.GCState{}, err
	}
	return st, nil
}

// RecordOrphans implements corestore.MetaStore. The first observation is the
// database time of the insert, which is later than the listing that found the
// keys. It touches only observation rows (doc/graft/PLAN7.md, LOCK ORDER).
func (s *Store) RecordOrphans(ctx context.Context, repoID int64, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := validateOrphanKeys(keys); err != nil {
		return err
	}

	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		now, err := s.wallTime(ctx, tx)
		if err != nil {
			return err
		}

		for _, key := range keys {
			if _, err := tx.ExecContext(ctx, s.bind(`
				INSERT INTO graft_gc_orphans (repo_id, pack_key, first_seen)
				VALUES (?, ?, ?)
				ON CONFLICT (repo_id, pack_key) DO NOTHING
			`), repoID, key, s.timeArg(now)); err != nil {
				return fmt.Errorf("record orphan observation: %w", err)
			}
		}
		return nil
	})
}

// ForgetOrphans implements corestore.MetaStore. It touches only observation
// rows (doc/graft/PLAN7.md, LOCK ORDER).
func (s *Store) ForgetOrphans(ctx context.Context, repoID int64, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := validateOrphanKeys(keys); err != nil {
		return err
	}

	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		for start := 0; start < len(keys); start += maxNamesPerQuery {
			chunk := keys[start:min(start+maxNamesPerQuery, len(keys))]

			args := make([]any, 0, len(chunk)+1)
			args = append(args, repoID)
			placeholders := make([]string, len(chunk))
			for i, key := range chunk {
				placeholders[i] = "?"
				args = append(args, key)
			}

			if _, err := tx.ExecContext(ctx, s.bind(
				"DELETE FROM graft_gc_orphans WHERE repo_id = ? AND pack_key IN ("+strings.Join(placeholders, ", ")+")"),
				args...); err != nil {
				return fmt.Errorf("forget orphan observations: %w", err)
			}
		}
		return nil
	})
}

// validateOrphanKeys checks that the keys can be stored in the text column of
// both dialects: an observation that cannot be recorded would make its object
// impossible to collect, so such a key is an error instead of being skipped.
func validateOrphanKeys(keys []string) error {
	for _, key := range keys {
		if key == "" || !storableText(key) {
			return fmt.Errorf("storage core: bucket key %q cannot be recorded as an orphan", key)
		}
	}
	return nil
}
