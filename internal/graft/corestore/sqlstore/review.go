package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// prepareReview returns a copy of r stored under id, without the computed
// fields (requirements and approved), which callers recompute on read.
func prepareReview(r *gitalypb.Review, id string) *gitalypb.Review {
	stored := proto.Clone(r).(*gitalypb.Review)
	stored.Id = id
	stored.Requirements = nil
	stored.Approved = false
	return stored
}

// reviewUpdatedAt returns the time of the review's last change, now when unset.
func reviewUpdatedAt(r *gitalypb.Review, now time.Time) time.Time {
	if r.GetUpdatedAt() == nil {
		return now.UTC()
	}
	return r.GetUpdatedAt().AsTime().UTC()
}

// unmarshalReview decodes the body of graft_reviews.
func unmarshalReview(body string) (*gitalypb.Review, error) {
	var r gitalypb.Review
	if err := (protojson.UnmarshalOptions{}).Unmarshal([]byte(body), &r); err != nil {
		return nil, fmt.Errorf("decode review: %w", err)
	}
	return &r, nil
}

// InsertReview implements corestore.MetaStore.
func (s *Store) InsertReview(ctx context.Context, repoID int64, build func(id string) (*gitalypb.Review, error), requireRoots bool) (*gitalypb.Review, error) {
	var stored *gitalypb.Review

	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		stored = nil

		// LOCK ORDER (doc/graft/PLAN7.md): the repository row first.
		repo, err := s.selectRepository(ctx, tx, "repo_id = ?", []any{repoID}, true)
		if err != nil {
			return err
		}

		id := repo.nextReviewID
		built, err := build(fmt.Sprintf("%d", id))
		if err != nil {
			return err
		}
		if built == nil {
			return errors.New("storage core: review builder returned no review")
		}

		r := prepareReview(built, fmt.Sprintf("%d", id))
		if requireRoots {
			if err := s.requireReviewRoots(ctx, tx, repoID, r); err != nil {
				return err
			}
		}
		body, err := marshalReview(r)
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, s.bind(`
			INSERT INTO graft_reviews (repo_id, review_id, version, target_branch, closed, body, updated_at)
			VALUES (?, ?, 1, ?, ?, ?, ?)
		`), repoID, id, bytesArg(r.GetTargetBranch()), r.GetState() != gitalypb.Review_STATE_OPEN_UNSPECIFIED,
			body, s.timeArg(reviewUpdatedAt(r, time.Now()))); err != nil {
			return fmt.Errorf("insert review: %w", err)
		}

		if _, err := tx.ExecContext(ctx, s.bind("UPDATE graft_repositories SET next_review_id = ? WHERE repo_id = ?"),
			id+1, repoID); err != nil {
			return fmt.Errorf("advance next review ID: %w", err)
		}

		stored = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return proto.Clone(stored).(*gitalypb.Review), nil
}

// GetReview implements corestore.MetaStore.
func (s *Store) GetReview(ctx context.Context, repoID int64, id string) (*gitalypb.Review, int64, error) {
	reviewID, ok := parseReviewID(id)
	if !ok {
		return nil, 0, corestore.ErrReviewNotFound
	}

	var (
		body    string
		version int64
	)
	err := s.db.QueryRowContext(ctx, s.bind("SELECT body, version FROM graft_reviews WHERE repo_id = ? AND review_id = ?"),
		repoID, reviewID).Scan(&body, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, corestore.ErrReviewNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("get review: %w", err)
	}

	r, err := unmarshalReview(body)
	if err != nil {
		return nil, 0, err
	}

	return r, version, nil
}

// ListReviews implements corestore.MetaStore.
func (s *Store) ListReviews(ctx context.Context, repoID int64, targetBranch string, includeClosed bool, limit int) ([]*gitalypb.Review, error) {
	query := "SELECT body FROM graft_reviews WHERE repo_id = ?"
	args := []any{repoID}
	if targetBranch != "" {
		query += " AND target_branch = ?"
		args = append(args, []byte(targetBranch))
	}
	if !includeClosed {
		query += " AND closed = ?"
		args = append(args, false)
	}
	query += " ORDER BY review_id DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, s.bind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}
	defer rows.Close()

	var reviews []*gitalypb.Review
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("scan review: %w", err)
		}

		r, err := unmarshalReview(body)
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}

	return reviews, nil
}

// UpdateReview implements corestore.MetaStore.
func (s *Store) UpdateReview(ctx context.Context, repoID int64, id string, version int64, r *gitalypb.Review, requireRoots bool) error {
	reviewID, ok := parseReviewID(id)
	if !ok {
		return corestore.ErrReviewNotFound
	}

	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		// An update that may move the review's head serializes with the
		// changesets that could delete its keep-around references: LOCK ORDER,
		// the repository row first. The others touch only the review row and
		// never lock the repository row.
		if requireRoots {
			if _, err := s.selectRepository(ctx, tx, "repo_id = ?", []any{repoID}, true); err != nil {
				return err
			}
			if err := s.requireReviewRoots(ctx, tx, repoID, r); err != nil {
				return err
			}
		}

		var current int64
		err := tx.QueryRowContext(ctx, s.bind("SELECT version FROM graft_reviews WHERE repo_id = ? AND review_id = ?"),
			repoID, reviewID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return corestore.ErrReviewNotFound
		}
		if err != nil {
			return fmt.Errorf("read review version: %w", err)
		}
		if current != version {
			return corestore.ErrReviewVersionConflict
		}

		stored := prepareReview(r, id)
		body, err := marshalReview(stored)
		if err != nil {
			return err
		}

		// The version condition makes the update safe even when another process
		// wrote the review after the read above.
		res, err := tx.ExecContext(ctx, s.bind(`
			UPDATE graft_reviews
			SET version = ?, target_branch = ?, closed = ?, body = ?, updated_at = ?
			WHERE repo_id = ? AND review_id = ? AND version = ?
		`), version+1, bytesArg(stored.GetTargetBranch()), stored.GetState() != gitalypb.Review_STATE_OPEN_UNSPECIFIED,
			body, s.timeArg(reviewUpdatedAt(stored, time.Now())), repoID, reviewID, version)
		if err != nil {
			return fmt.Errorf("update review: %w", err)
		}

		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("update review: %w", err)
		}
		if n == 0 {
			return corestore.ErrReviewVersionConflict
		}

		return nil
	})
}
