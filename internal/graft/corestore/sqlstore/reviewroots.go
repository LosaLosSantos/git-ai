package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The REVIEW ROOTS RULE (doc/graft/PLAN7.md) keeps the invariant "every open
// review written with requireRoots has the keep-around reference of its head
// and base commits" with two checks. Both run under the repository row lock,
// which review writes with requireRoots and every changeset take first (LOCK
// ORDER), so a review write and a deletion of one of its references are
// serialized: either the deletion is refused, or the review write finds the
// reference gone and fails.

// requireReviewRoots refuses a review write whose head or base commit has no
// keep-around reference with that commit as its value.
func (s *Store) requireReviewRoots(ctx context.Context, tx *sql.Tx, repoID int64, r *gitalypb.Review) error {
	commits := reviewCommits(r)
	if len(commits) == 0 {
		return nil
	}

	names := make([]string, 0, len(commits))
	for _, commit := range commits {
		names = append(names, corestore.KeepAroundRef(commit))
	}
	current, err := s.currentTargets(ctx, tx, repoID, names)
	if err != nil {
		return err
	}

	for _, commit := range commits {
		if current[corestore.KeepAroundRef(commit)] != corestore.Target(commit) {
			return fmt.Errorf("review commit %s: %w", commit, corestore.ErrReviewRootsMissing)
		}
	}
	return nil
}

// refuseReleasedRoots refuses a changeset that deletes or moves a keep-around
// reference whose value is the head or base commit of an open review.
func (s *Store) refuseReleasedRoots(ctx context.Context, tx *sql.Tx, repoID int64, updates []corestore.AppliedUpdate) error {
	released := map[string]string{}
	for _, u := range updates {
		if strings.HasPrefix(u.Name, corestore.KeepAroundPrefix) && !u.Old.Absent() && !u.Old.IsSymbolic() && u.Old != u.New {
			released[string(u.Old)] = u.Name
		}
	}
	if len(released) == 0 {
		return nil
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
		for _, commit := range reviewCommits(review) {
			if name, ok := released[commit]; ok {
				return fmt.Errorf("reference %q, review %s: %w", name, review.GetId(), corestore.ErrReviewRootInUse)
			}
		}
	}
	return rows.Err()
}

// reviewCommits returns the head and base commits of the review that are set.
func reviewCommits(r *gitalypb.Review) []string {
	var commits []string
	for _, commit := range []string{r.GetHeadCommitId(), r.GetBaseCommitId()} {
		if commit != "" {
			commits = append(commits, commit)
		}
	}
	return commits
}
