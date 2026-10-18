package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
)

// CreateCheckpoint implements corestore.MetaStore.
func (s *Store) CreateCheckpoint(ctx context.Context, repoID int64, in corestore.CheckpointInput) (corestore.Checkpoint, bool, error) {
	if err := validateCheckpoint(repoID, in); err != nil {
		return corestore.Checkpoint{}, false, err
	}

	var (
		cp      corestore.Checkpoint
		created bool
	)
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		cp, created = corestore.Checkpoint{}, false

		// LOCK ORDER (doc/graft/PLAN7.md): the repository row first. It serializes
		// the checkpoint with changesets and with the review writes that move a
		// review's commits, which the roots check below relies on.
		repo, err := s.selectRepository(ctx, tx, "repo_id = ?", []any{repoID}, true)
		if err != nil {
			return err
		}
		if in.Seq > repo.headSeq {
			return fmt.Errorf("storage core: checkpoint at changeset %d is beyond the head %d of repository %d", in.Seq, repo.headSeq, repoID)
		}

		existing, found, err := s.checkpointAt(ctx, tx, repoID, in.Seq)
		if err != nil {
			return err
		}
		if found {
			cp = existing
			return nil
		}

		newest, ok, err := s.newestCheckpoint(ctx, tx, repoID)
		if err != nil {
			return err
		}
		if ok && newest.Seq > in.Seq {
			return fmt.Errorf("storage core: checkpoint at changeset %d is older than the newest checkpoint at %d of repository %d", in.Seq, newest.Seq, repoID)
		}

		if err := s.checkReviewRoots(ctx, tx, repoID, in.Roots); err != nil {
			return err
		}

		now, err := s.wallTime(ctx, tx)
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, s.bind(`
			INSERT INTO graft_checkpoints (repo_id, seq, pack_key, pack_bytes, created_at)
			VALUES (?, ?, ?, ?, ?)
		`), repoID, int64(in.Seq), in.PackKey, in.PackBytes, s.timeArg(now)); err != nil {
			return fmt.Errorf("insert checkpoint: %w", err)
		}

		names := make([]string, 0, len(in.Refs))
		for name := range in.Refs {
			names = append(names, name)
		}
		sort.Strings(names)
		for ordinal, name := range names {
			if _, err := tx.ExecContext(ctx, s.bind(`
				INSERT INTO graft_checkpoint_refs (repo_id, seq, ordinal, name, target)
				VALUES (?, ?, ?, ?, ?)
			`), repoID, int64(in.Seq), ordinal, bytesArg([]byte(name)), string(in.Refs[name])); err != nil {
				return fmt.Errorf("insert checkpoint reference: %w", err)
			}
		}

		cp = corestore.Checkpoint{
			RepoID:    repoID,
			Seq:       in.Seq,
			PackKey:   in.PackKey,
			PackBytes: in.PackBytes,
			CreatedAt: now,
		}
		created = true
		return nil
	})
	if err != nil {
		return corestore.Checkpoint{}, false, err
	}

	return cp, created, nil
}

// validateCheckpoint checks a checkpoint before anything is read.
func validateCheckpoint(repoID int64, in corestore.CheckpointInput) error {
	if in.Seq == 0 || in.Seq > math.MaxInt64 {
		return fmt.Errorf("storage core: invalid checkpoint sequence %d", in.Seq)
	}

	id, _, ok := corestore.ParsePackKey(in.PackKey)
	if !ok || id != repoID || !corestore.IsOwnedPackKey(in.PackKey) {
		return fmt.Errorf("storage core: checkpoint pack key %q is not an owned pack key of repository %d", in.PackKey, repoID)
	}
	if in.PackBytes < 0 {
		return fmt.Errorf("storage core: invalid checkpoint pack size %d", in.PackBytes)
	}

	if len(in.Refs) == 0 {
		return errors.New("storage core: checkpoint has no references")
	}
	for name, target := range in.Refs {
		if name == "" {
			return errors.New("storage core: checkpoint reference with an empty name")
		}
		// A snapshot holds the references that exist; the target column is text
		// in both dialects (see validateChangeset).
		if target.Absent() || !storableText(string(target)) {
			return fmt.Errorf("storage core: checkpoint reference %q has an invalid target %q", name, target)
		}
	}

	for _, root := range in.Roots {
		if root == "" || !storableText(root) {
			return fmt.Errorf("storage core: invalid checkpoint root %q", root)
		}
	}

	return nil
}

// checkRootsAt refuses a review write whose commits were checked against the
// reference snapshot at rootsAt when a newer checkpoint was recorded since
// (doc/graft/PLAN7.md, REVIEW ROOTS RULE R1): that checkpoint may not include
// them. The caller holds the repository row, which CreateCheckpoint takes
// first too, so the two are serialized.
func (s *Store) checkRootsAt(ctx context.Context, tx *sql.Tx, repoID int64, rootsAt uint64) error {
	newest, ok, err := s.newestCheckpoint(ctx, tx, repoID)
	if err != nil {
		return err
	}
	if ok && newest.Seq > rootsAt {
		return fmt.Errorf("checkpoint at changeset %d, review commits checked at %d: %w", newest.Seq, rootsAt, corestore.ErrReviewRootsStale)
	}
	return nil
}

// checkReviewRoots refuses the checkpoint when an open review has a head or
// base commit that is not one of the roots the checkpoint's pack includes: the
// review was created, or moved its head, during the build (doc/graft/PLAN7.md,
// REVIEW ROOTS RULE R2). The caller holds the repository row, which every
// review write that moves a review's commits takes first, so the reviews read
// here are the ones the checkpoint commits with.
func (s *Store) checkReviewRoots(ctx context.Context, tx *sql.Tx, repoID int64, roots []string) error {
	included := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		included[root] = struct{}{}
	}

	rows, err := tx.QueryContext(ctx, s.bind("SELECT body FROM graft_reviews WHERE repo_id = ? AND closed = ?"), repoID, false)
	if err != nil {
		return fmt.Errorf("read open reviews: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return fmt.Errorf("scan review: %w", err)
		}
		review, err := unmarshalReview(body)
		if err != nil {
			return err
		}
		for _, commit := range []string{review.GetHeadCommitId(), review.GetBaseCommitId()} {
			if commit == "" {
				continue
			}
			if _, ok := included[commit]; !ok {
				return fmt.Errorf("review %s commit %s: %w", review.GetId(), commit, corestore.ErrCheckpointRootsChanged)
			}
		}
	}

	return rows.Err()
}

// checkpointColumns are the columns read by scanCheckpoint, in order.
const checkpointColumns = "seq, pack_key, pack_bytes, created_at"

// scanCheckpoint reads a row of checkpointColumns.
func scanCheckpoint(repoID int64, scan func(...any) error) (corestore.Checkpoint, error) {
	var (
		seq        int64
		cp         corestore.Checkpoint
		createdRaw any
	)
	if err := scan(&seq, &cp.PackKey, &cp.PackBytes, &createdRaw); err != nil {
		return corestore.Checkpoint{}, err
	}

	created, err := scanTime(createdRaw)
	if err != nil {
		return corestore.Checkpoint{}, err
	}

	cp.RepoID = repoID
	cp.Seq = uint64(seq)
	cp.CreatedAt = created
	return cp, nil
}

// checkpointAt reads the checkpoint at seq; found is false when there is none.
func (s *Store) checkpointAt(ctx context.Context, q querier, repoID int64, seq uint64) (corestore.Checkpoint, bool, error) {
	cp, err := scanCheckpoint(repoID, q.QueryRowContext(ctx, s.bind(
		"SELECT "+checkpointColumns+" FROM graft_checkpoints WHERE repo_id = ? AND seq = ?"),
		repoID, int64(seq)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return corestore.Checkpoint{}, false, nil
	}
	if err != nil {
		return corestore.Checkpoint{}, false, fmt.Errorf("read checkpoint: %w", err)
	}
	return cp, true, nil
}

// newestCheckpoint reads the checkpoint with the greatest sequence; ok is false
// when there is none.
func (s *Store) newestCheckpoint(ctx context.Context, q querier, repoID int64) (corestore.Checkpoint, bool, error) {
	cp, err := scanCheckpoint(repoID, q.QueryRowContext(ctx, s.bind(
		"SELECT "+checkpointColumns+" FROM graft_checkpoints WHERE repo_id = ? ORDER BY seq DESC LIMIT 1"),
		repoID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return corestore.Checkpoint{}, false, nil
	}
	if err != nil {
		return corestore.Checkpoint{}, false, fmt.Errorf("read newest checkpoint: %w", err)
	}
	return cp, true, nil
}

// listCheckpoints reads every checkpoint of the repository, ascending by
// sequence.
func (s *Store) listCheckpoints(ctx context.Context, q querier, repoID int64) ([]corestore.Checkpoint, error) {
	rows, err := q.QueryContext(ctx, s.bind(
		"SELECT "+checkpointColumns+" FROM graft_checkpoints WHERE repo_id = ? ORDER BY seq"), repoID)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	defer rows.Close()

	var checkpoints []corestore.Checkpoint
	for rows.Next() {
		cp, err := scanCheckpoint(repoID, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan checkpoint: %w", err)
		}
		checkpoints = append(checkpoints, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	return checkpoints, nil
}

// requireRepository returns ErrRepositoryNotFound when the repository does not
// exist.
func (s *Store) requireRepository(ctx context.Context, q querier, repoID int64) error {
	var id int64
	err := q.QueryRowContext(ctx, s.bind("SELECT repo_id FROM graft_repositories WHERE repo_id = ?"), repoID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return corestore.ErrRepositoryNotFound
	}
	if err != nil {
		return fmt.Errorf("read repository: %w", err)
	}
	return nil
}

// NewestCheckpoint implements corestore.MetaStore.
func (s *Store) NewestCheckpoint(ctx context.Context, repoID int64) (corestore.Checkpoint, bool, error) {
	var (
		cp corestore.Checkpoint
		ok bool
	)
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		cp, ok, err = s.newestCheckpoint(ctx, tx, repoID)
		return err
	})
	if err != nil {
		return corestore.Checkpoint{}, false, err
	}
	return cp, ok, nil
}

// SyncBoundary implements corestore.MetaStore.
func (s *Store) SyncBoundary(ctx context.Context, repoID int64, age time.Duration) (uint64, error) {
	if age < 0 {
		return 0, fmt.Errorf("storage core: negative checkpoint age %s", age)
	}

	hostNow := time.Now()
	var boundary uint64
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		boundary = 0

		now, err := s.snapshotTime(ctx, tx, hostNow)
		if err != nil {
			return err
		}
		if err := s.requireRepository(ctx, tx, repoID); err != nil {
			return err
		}
		checkpoints, err := s.listCheckpoints(ctx, tx, repoID)
		if err != nil {
			return err
		}

		limit := now.Add(-age)
		for _, cp := range checkpoints {
			if !cp.CreatedAt.After(limit) && cp.Seq > boundary {
				boundary = cp.Seq
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return boundary, nil
}

// CheckpointRefs implements corestore.MetaStore.
func (s *Store) CheckpointRefs(ctx context.Context, repoID int64, seq uint64) (map[string]corestore.Target, error) {
	if seq > math.MaxInt64 {
		return nil, corestore.ErrCheckpointNotFound
	}

	var refs map[string]corestore.Target
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		refs = map[string]corestore.Target{}

		if _, found, err := s.checkpointAt(ctx, tx, repoID, seq); err != nil {
			return err
		} else if !found {
			return corestore.ErrCheckpointNotFound
		}

		return readReferences(ctx, s, tx,
			"SELECT name, target FROM graft_checkpoint_refs WHERE repo_id = ? AND seq = ? ORDER BY ordinal",
			[]any{repoID, int64(seq)}, refs)
	})
	if err != nil {
		return nil, err
	}
	return refs, nil
}

// DeleteCheckpoint implements corestore.MetaStore. It touches only the
// checkpoint rows and never the repository row (doc/graft/PLAN7.md, LOCK ORDER).
func (s *Store) DeleteCheckpoint(ctx context.Context, repoID int64, seq uint64) (bool, error) {
	if seq > math.MaxInt64 {
		return false, nil
	}

	var deleted bool
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.bind("DELETE FROM graft_checkpoints WHERE repo_id = ? AND seq = ?"), repoID, int64(seq))
		if err != nil {
			return fmt.Errorf("delete checkpoint: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete checkpoint: %w", err)
		}
		deleted = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}
