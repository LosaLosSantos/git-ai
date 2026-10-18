// Package dbreview stores the reviews of a repository in the storage core's
// database. It is the authoritative-mode counterpart of review/store.Store and
// has exactly the same semantics (see doc/graft/PLAN6.md, "Changelog and
// reviews in the database"). Reviews are rows of graft_reviews; the version
// column provides the optimistic concurrency that the file lock provides for
// the files store.
package dbreview

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// maxUpdateAttempts bounds the optimistic retries of Update.
	maxUpdateAttempts = 10
	// minBackoff and maxBackoff bound the wait between two attempts of
	// Update: the limit doubles from minBackoff on every attempt, capped at
	// maxBackoff, and the wait is between half the limit and the limit.
	minBackoff = 4 * time.Millisecond
	maxBackoff = 400 * time.Millisecond
)

// Store is the review store of one repository in the storage core database.
// The store.ReviewStore methods have no context parameter, so the context given
// to New is used for every database call.
type Store struct {
	ctx    context.Context
	meta   corestore.MetaStore
	repoID int64
	// requireRoots makes Create and Update require the keep-around references of
	// the review's commits (doc/graft/PLAN7.md, REVIEW ROOTS RULE).
	requireRoots bool
}

var _ store.ReviewStore = (*Store)(nil)

// New returns the review store of the repository repoID of meta. ctx bounds
// the database calls of the store.
func New(ctx context.Context, meta corestore.MetaStore, repoID int64) store.ReviewStore {
	return &Store{ctx: ctx, meta: meta, repoID: repoID}
}

// WithRequiredRoots returns a store whose Create and Update write a review only
// when the keep-around references of its head and base commits exist in the
// database, else they fail with corestore.ErrReviewRootsMissing and write
// nothing. It is the store of the writes that may record new commits.
func (s *Store) WithRequiredRoots() store.ReviewStore {
	required := *s
	required.requireRoots = true
	return &required
}

// Create stores a new review. It assigns the id, created_at and updated_at.
// requirements and approved are not stored. The returned review is a copy.
func (s *Store) Create(r *gitalypb.Review) (*gitalypb.Review, error) {
	stored, err := s.meta.InsertReview(s.ctx, s.repoID, func(id string) (*gitalypb.Review, error) {
		now := timestamppb.Now()
		review := proto.Clone(r).(*gitalypb.Review)
		review.Id = id
		review.CreatedAt = now
		review.UpdatedAt = now
		review.Requirements = nil
		review.Approved = false
		return review, nil
	}, s.requireRoots)
	if err != nil {
		return nil, mapErr(err)
	}

	return proto.Clone(stored).(*gitalypb.Review), nil
}

// Get returns the review with the given id, or store.ErrNotFound.
func (s *Store) Get(id string) (*gitalypb.Review, error) {
	r, _, err := s.meta.GetReview(s.ctx, s.repoID, id)
	if err != nil {
		return nil, mapErr(err)
	}

	return r, nil
}

// List returns the reviews newest first, with the filters of store.Store.List.
// A limit of zero or less means no limit.
func (s *Store) List(targetBranch string, includeClosed bool, limit int) ([]*gitalypb.Review, error) {
	reviews, err := s.meta.ListReviews(s.ctx, s.repoID, targetBranch, includeClosed, limit)
	if err != nil {
		return nil, mapErr(err)
	}

	return reviews, nil
}

// Update reads the review with the given id, passes it to fn and stores the
// result, with updated_at refreshed. When fn returns an error nothing is
// written and the error is returned unchanged. A concurrent change of the
// review makes Update read it again and call fn again, up to ten attempts. The
// returned review is a copy.
func (s *Store) Update(id string, fn func(*gitalypb.Review) error) (*gitalypb.Review, error) {
	for attempt := 0; attempt < maxUpdateAttempts; attempt++ {
		r, version, err := s.meta.GetReview(s.ctx, s.repoID, id)
		if err != nil {
			return nil, mapErr(err)
		}

		if err := fn(r); err != nil {
			return nil, err
		}

		r.UpdatedAt = timestamppb.Now()

		// Like the files store, the computed fields are not stored.
		stored := proto.Clone(r).(*gitalypb.Review)
		stored.Requirements = nil
		stored.Approved = false

		err = s.meta.UpdateReview(s.ctx, s.repoID, id, version, stored, s.requireRoots)
		if err == nil {
			return proto.Clone(r).(*gitalypb.Review), nil
		}
		if !errors.Is(err, corestore.ErrReviewVersionConflict) {
			return nil, mapErr(err)
		}

		if attempt+1 < maxUpdateAttempts {
			if err := s.backoff(attempt); err != nil {
				return nil, err
			}
		}
	}

	return nil, fmt.Errorf("update review %s: %w", id, corestore.ErrReviewVersionConflict)
}

// backoff waits a jittered delay that grows with the attempt before Update
// reads the review again. Without it the writer that just won keeps winning:
// its next update starts while the loser reads the review again, so a burst of
// updates from one node could starve the others of their ten attempts.
func (s *Store) backoff(attempt int) error {
	limit := minBackoff << attempt
	if limit > maxBackoff || limit <= 0 {
		limit = maxBackoff
	}
	delay := limit/2 + time.Duration(rand.Int63n(int64(limit/2)+1))

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

// mapErr translates the storage core errors into the errors of the review
// store.
func mapErr(err error) error {
	if errors.Is(err, corestore.ErrReviewNotFound) {
		return store.ErrNotFound
	}
	return err
}
