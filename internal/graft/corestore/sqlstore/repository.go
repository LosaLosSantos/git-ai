package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// repoState is a repository row as the operations of a transaction see it.
type repoState struct {
	id           int64
	key          corestore.RepoKey
	objectFormat string
	headSeq      uint64
	logSeq       uint64
	nextReviewID int64
	createdAt    time.Time
}

// repoColumns are the columns read by selectRepository, in order.
const repoColumns = "repo_id, storage, relative_path, object_format, head_seq, log_seq, next_review_id, created_at"

// selectRepository reads the repository matching where. With lock, Postgres
// locks the row until the end of the transaction, which serializes the
// changesets of the repository. SQLite needs no such clause: the immediate
// transaction already holds the write lock.
func (s *Store) selectRepository(ctx context.Context, q querier, where string, args []any, lock bool) (repoState, error) {
	query := "SELECT " + repoColumns + " FROM graft_repositories WHERE " + where
	if lock && s.dialect == Postgres {
		query += " FOR UPDATE"
	}

	var (
		r                     repoState
		storage, relativePath string
		headSeq, logSeq       int64
		nextReviewID          int64
		createdRaw            any
	)
	err := q.QueryRowContext(ctx, s.bind(query), args...).Scan(
		&r.id, &storage, &relativePath, &r.objectFormat, &headSeq, &logSeq, &nextReviewID, &createdRaw,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return repoState{}, corestore.ErrRepositoryNotFound
	}
	if err != nil {
		return repoState{}, fmt.Errorf("select repository: %w", err)
	}

	r.createdAt, err = scanTime(createdRaw)
	if err != nil {
		return repoState{}, fmt.Errorf("select repository: %w", err)
	}

	r.key = corestore.RepoKey{Storage: storage, RelativePath: relativePath}
	r.headSeq = uint64(headSeq)
	r.logSeq = uint64(logSeq)
	r.nextReviewID = nextReviewID

	return r, nil
}

// storableKey reports whether the key can be stored in the text columns of
// both dialects (Postgres refuses bytes that are not UTF-8 and NUL bytes).
func storableKey(key corestore.RepoKey) bool {
	return storableText(key.Storage) && storableText(key.RelativePath)
}

// toRepository returns the public form of a repository row.
func toRepository(r repoState) corestore.Repository {
	return corestore.Repository{
		ID:           r.id,
		Key:          r.key,
		ObjectFormat: r.objectFormat,
		HeadSeq:      r.headSeq,
		LogSeq:       r.logSeq,
		CreatedAt:    r.createdAt,
	}
}

// RegisterRepository implements corestore.MetaStore.
func (s *Store) RegisterRepository(ctx context.Context, key corestore.RepoKey, objectFormat string, init corestore.InitialState) (corestore.Repository, corestore.CommitResult, error) {
	if objectFormat != "sha1" && objectFormat != "sha256" {
		return corestore.Repository{}, corestore.CommitResult{}, fmt.Errorf("unsupported object format %q", objectFormat)
	}

	if !storableKey(key) {
		return corestore.Repository{}, corestore.CommitResult{}, fmt.Errorf("repository key %q/%q is not valid UTF-8 text", key.Storage, key.RelativePath)
	}

	if err := validateImport(init); err != nil {
		return corestore.Repository{}, corestore.CommitResult{}, err
	}

	var (
		repo   repoState
		result corestore.CommitResult
	)
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		result = corestore.CommitResult{}

		var existing int64
		err := tx.QueryRowContext(ctx, s.bind("SELECT repo_id FROM graft_repositories WHERE storage = ? AND relative_path = ?"),
			key.Storage, key.RelativePath).Scan(&existing)
		switch {
		case err == nil:
			return corestore.ErrRepositoryExists
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("look up repository: %w", err)
		}

		now := time.Now().UTC()
		var id int64
		err = tx.QueryRowContext(ctx, s.bind(`
			INSERT INTO graft_repositories (storage, relative_path, object_format, head_seq, log_seq, next_review_id, created_at)
			VALUES (?, ?, ?, 0, 0, 1, ?)
			RETURNING repo_id
		`), key.Storage, key.RelativePath, objectFormat, s.timeArg(now)).Scan(&id)
		if err != nil {
			if isUniqueViolation(err) {
				return corestore.ErrRepositoryExists
			}
			return fmt.Errorf("insert repository: %w", err)
		}

		repo = repoState{
			id:           id,
			key:          key,
			objectFormat: objectFormat,
			nextReviewID: 1,
			createdAt:    now,
		}

		if err := s.insertImport(ctx, tx, &repo, init); err != nil {
			return err
		}

		if init.Changeset != nil {
			in := *init.Changeset
			in.RepoID = repo.id

			committed, err := s.commitChangeset(ctx, tx, &repo, in)
			if err != nil {
				return err
			}
			result = committed
		}

		return nil
	})
	if err != nil {
		return corestore.Repository{}, corestore.CommitResult{}, err
	}

	return toRepository(repo), result, nil
}

// validateImport checks the imported history before anything is written.
func validateImport(init corestore.InitialState) error {
	var last uint64
	for i, entry := range init.Changelog {
		if entry.Sequence == 0 {
			return fmt.Errorf("imported changelog entry %d has sequence 0", i)
		}
		if entry.Sequence <= last {
			return fmt.Errorf("imported changelog sequence %d is not greater than %d", entry.Sequence, last)
		}
		last = entry.Sequence
	}

	for i, review := range init.Reviews {
		if review == nil {
			return fmt.Errorf("imported review %d is nil", i)
		}
		if _, ok := parseReviewID(review.GetId()); !ok {
			return fmt.Errorf("imported review ID %q is not a decimal number without leading zeros", review.GetId())
		}
	}

	if cs := init.Changeset; cs != nil {
		for _, u := range cs.Updates {
			if u.Expect != corestore.ExpectValue || u.Old != "" {
				return fmt.Errorf("initial changeset update of %q must expect the reference to be absent", u.Name)
			}
		}
	}

	return nil
}

// insertImport writes the imported changelog entries and reviews of a new
// repository and sets its log and next review sequences.
func (s *Store) insertImport(ctx context.Context, tx *sql.Tx, repo *repoState, init corestore.InitialState) error {
	if err := insertChangelog(ctx, s, tx, repo.id, init.Changelog); err != nil {
		return err
	}
	if n := len(init.Changelog); n > 0 {
		repo.logSeq = init.Changelog[n-1].Sequence
	}

	for _, review := range init.Reviews {
		if _, err := s.insertImportedReview(ctx, tx, repo, review, false); err != nil {
			return err
		}
	}

	return s.updateRepoSequences(ctx, tx, repo)
}

// insertImportedReview writes an imported review (version 1) under its own ID
// and advances the next review ID of repo past it. An ID that already exists
// is skipped when skipExisting is set (inserted is then false) and an error
// otherwise. The review ID must have been validated (validateImport).
func (s *Store) insertImportedReview(ctx context.Context, tx *sql.Tx, repo *repoState, review *gitalypb.Review, skipExisting bool) (inserted bool, _ error) {
	id, _ := parseReviewID(review.GetId())
	stored := prepareReview(review, review.GetId())

	body, err := marshalReview(stored)
	if err != nil {
		return false, err
	}

	query := `
		INSERT INTO graft_reviews (repo_id, review_id, version, target_branch, closed, body, updated_at)
		VALUES (?, ?, 1, ?, ?, ?, ?)`
	if skipExisting {
		query += " ON CONFLICT (repo_id, review_id) DO NOTHING"
	}

	res, err := tx.ExecContext(ctx, s.bind(query), repo.id, id, bytesArg(stored.GetTargetBranch()),
		stored.GetState() != gitalypb.Review_STATE_OPEN_UNSPECIFIED, body, s.timeArg(reviewUpdatedAt(stored, time.Now())))
	if err != nil {
		if isUniqueViolation(err) {
			return false, fmt.Errorf("imported review %d is imported twice", id)
		}
		return false, fmt.Errorf("insert imported review %d: %w", id, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert imported review %d: %w", id, err)
	}

	if id >= repo.nextReviewID {
		repo.nextReviewID = id + 1
	}

	return n > 0, nil
}

// importReview writes a file review for ImportHistory: it is inserted when the
// database has no review with its ID, and it replaces the database review when
// corestore.ReviewNeedsImport says that the file changed it since an earlier
// import (shadow mode writes the file review store only). A replaced review
// gets the next version, so that an UpdateReview based on the old version
// fails with ErrReviewVersionConflict and re-reads it. It reports whether the
// review was written. The review ID must have been validated (validateImport).
func (s *Store) importReview(ctx context.Context, tx *sql.Tx, repo *repoState, review *gitalypb.Review) (bool, error) {
	id, _ := parseReviewID(review.GetId())

	var (
		currentBody string
		version     int64
	)
	err := tx.QueryRowContext(ctx, s.bind("SELECT body, version FROM graft_reviews WHERE repo_id = ? AND review_id = ?"),
		repo.id, id).Scan(&currentBody, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return s.insertImportedReview(ctx, tx, repo, review, true)
	}
	if err != nil {
		return false, fmt.Errorf("read review %d: %w", id, err)
	}

	current, err := unmarshalReview(currentBody)
	if err != nil {
		return false, fmt.Errorf("review %d: %w", id, err)
	}
	if !corestore.ReviewNeedsImport(review, current) {
		return false, nil
	}

	stored := prepareReview(review, review.GetId())
	body, err := marshalReview(stored)
	if err != nil {
		return false, err
	}

	// The version condition keeps a concurrent UpdateReview (which writes a
	// newer change) from being overwritten.
	res, err := tx.ExecContext(ctx, s.bind(`
		UPDATE graft_reviews
		SET version = ?, target_branch = ?, closed = ?, body = ?, updated_at = ?
		WHERE repo_id = ? AND review_id = ? AND version = ?
	`), version+1, bytesArg(stored.GetTargetBranch()), stored.GetState() != gitalypb.Review_STATE_OPEN_UNSPECIFIED,
		body, s.timeArg(reviewUpdatedAt(stored, time.Now())), repo.id, id, version)
	if err != nil {
		return false, fmt.Errorf("replace review %d: %w", id, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("replace review %d: %w", id, err)
	}

	return n > 0, nil
}

// updateRepoSequences writes the log sequence and the next review ID of repo.
func (s *Store) updateRepoSequences(ctx context.Context, tx *sql.Tx, repo *repoState) error {
	if _, err := tx.ExecContext(ctx, s.bind("UPDATE graft_repositories SET log_seq = ?, next_review_id = ? WHERE repo_id = ?"),
		int64(repo.logSeq), repo.nextReviewID, repo.id); err != nil {
		return fmt.Errorf("update repository sequences: %w", err)
	}

	return nil
}

// ImportHistory implements corestore.MetaStore.
func (s *Store) ImportHistory(ctx context.Context, repoID int64, entries []changelog.Entry, reviews []*gitalypb.Review) (int, int, error) {
	if err := validateImport(corestore.InitialState{Changelog: entries, Reviews: reviews}); err != nil {
		return 0, 0, err
	}

	var importedEntries, importedReviews int
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		importedEntries, importedReviews = 0, 0

		repo, err := s.selectRepository(ctx, tx, "repo_id = ?", []any{repoID}, true)
		if err != nil {
			return err
		}

		// The sequences continue the database changelog, so that no sequence is
		// used twice and consumers' cursors stay valid.
		if len(entries) > 0 && entries[0].Sequence <= repo.logSeq {
			return fmt.Errorf("imported changelog sequence %d is not greater than the log sequence %d of the repository",
				entries[0].Sequence, repo.logSeq)
		}

		if err := insertChangelog(ctx, s, tx, repo.id, entries); err != nil {
			return err
		}
		if n := len(entries); n > 0 {
			repo.logSeq = entries[n-1].Sequence
		}
		importedEntries = len(entries)

		for _, review := range reviews {
			written, err := s.importReview(ctx, tx, &repo, review)
			if err != nil {
				return err
			}
			if written {
				importedReviews++
			}
		}

		return s.updateRepoSequences(ctx, tx, &repo)
	})
	if err != nil {
		return 0, 0, err
	}

	return importedEntries, importedReviews, nil
}

// GetRepository implements corestore.MetaStore.
func (s *Store) GetRepository(ctx context.Context, key corestore.RepoKey) (corestore.Repository, error) {
	if !storableKey(key) {
		// RegisterRepository refuses such keys: the repository cannot exist.
		return corestore.Repository{}, corestore.ErrRepositoryNotFound
	}

	r, err := s.selectRepository(ctx, s.db, "storage = ? AND relative_path = ?", []any{key.Storage, key.RelativePath}, false)
	if err != nil {
		return corestore.Repository{}, err
	}
	return toRepository(r), nil
}

// DeleteRepository implements corestore.MetaStore.
func (s *Store) DeleteRepository(ctx context.Context, key corestore.RepoKey) (bool, error) {
	if !storableKey(key) {
		return false, nil
	}

	var deleted bool
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.bind("DELETE FROM graft_repositories WHERE storage = ? AND relative_path = ?"),
			key.Storage, key.RelativePath)
		if err != nil {
			return fmt.Errorf("delete repository: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete repository: %w", err)
		}
		deleted = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// Refs implements corestore.MetaStore.
func (s *Store) Refs(ctx context.Context, repoID int64) (map[string]corestore.Target, uint64, error) {
	var (
		refs    map[string]corestore.Target
		headSeq uint64
	)
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		refs = map[string]corestore.Target{}

		var head int64
		err := tx.QueryRowContext(ctx, s.bind("SELECT head_seq FROM graft_repositories WHERE repo_id = ?"), repoID).Scan(&head)
		if errors.Is(err, sql.ErrNoRows) {
			return corestore.ErrRepositoryNotFound
		}
		if err != nil {
			return fmt.Errorf("read head sequence: %w", err)
		}
		headSeq = uint64(head)

		rows, err := tx.QueryContext(ctx, s.bind("SELECT name, target FROM graft_refs WHERE repo_id = ?"), repoID)
		if err != nil {
			return fmt.Errorf("read references: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var name []byte
			var target string
			if err := rows.Scan(&name, &target); err != nil {
				return fmt.Errorf("scan reference: %w", err)
			}
			refs[string(name)] = corestore.Target(target)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}

	return refs, headSeq, nil
}

// ReportApplied implements corestore.MetaStore.
func (s *Store) ReportApplied(ctx context.Context, repoID int64, nodeID string, seq uint64) error {
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.bind(`
			INSERT INTO graft_nodes (repo_id, node_id, applied_seq, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (repo_id, node_id) DO UPDATE SET
				applied_seq = excluded.applied_seq,
				updated_at = excluded.updated_at
		`), repoID, validUTF8(nodeID), int64(seq), s.timeArg(time.Now()))
		if err != nil {
			return fmt.Errorf("report applied sequence: %w", err)
		}
		return nil
	})
}

// insertChangelog appends entries to the changelog of the repository. The
// sequences of the entries are their own.
func insertChangelog(ctx context.Context, s *Store, q querier, repoID int64, entries []changelog.Entry) error {
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("encode changelog entry %d: %w", entry.Sequence, err)
		}

		if _, err := q.ExecContext(ctx, s.bind("INSERT INTO graft_changelog (repo_id, log_seq, entry) VALUES (?, ?, ?)"),
			repoID, int64(entry.Sequence), string(data)); err != nil {
			return fmt.Errorf("insert changelog entry %d: %w", entry.Sequence, err)
		}
	}
	return nil
}
