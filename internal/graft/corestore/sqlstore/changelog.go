package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
)

// ScanChangelog implements corestore.MetaStore.
func (s *Store) ScanChangelog(ctx context.Context, repoID int64, after uint64, limit int) ([]changelog.Entry, error) {
	// The sequences are BIGINT: no entry follows math.MaxInt64 (a larger
	// cursor must not wrap around to a negative one).
	if after >= math.MaxInt64 {
		return nil, nil
	}

	query := "SELECT entry FROM graft_changelog WHERE repo_id = ? AND log_seq > ? ORDER BY log_seq"
	args := []any{repoID, int64(after)}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, s.bind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("scan changelog: %w", err)
	}
	defer rows.Close()

	var entries []changelog.Entry
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan changelog entry: %w", err)
		}

		var entry changelog.Entry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			return nil, fmt.Errorf("decode changelog entry: %w", err)
		}
		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan changelog: %w", err)
	}

	return entries, nil
}
