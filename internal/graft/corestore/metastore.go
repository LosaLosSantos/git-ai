package corestore

import (
	"context"
	"errors"

	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// ChangesetInput is a changeset to commit.
type ChangesetInput struct {
	// RepoID is the repository. RegisterRepository ignores it.
	RepoID int64
	// Updates are the reference changes, in order. A name appears at most once.
	Updates []RefUpdate
	// Operation, Actor and NodeID describe who made the changeset.
	Operation string
	Actor     string
	NodeID    string
	// PackKey is the bucket key of the already uploaded pack with the new
	// objects, empty when there are none. PackBytes is its size.
	PackKey   string
	PackBytes int64
	// Changelog, when not nil, appends one changelog entry per effective update
	// of a branch or tag (IsChangelogRef), in the order of Updates, in the same
	// transaction. The entries get the next changelog sequences, the actual
	// previous value as OldOID (the zero OID of the repository's object format
	// when the reference was absent) and the new value as NewOID (the zero OID
	// when deleted). A symbolic value is never a branch or tag value; such an
	// update is not logged. RecordedAt zero means the commit time.
	Changelog *changelog.Meta
	// Force records the changeset even when references do not satisfy their
	// expectations (shadow mode): the changeset is marked forced and the
	// conflicts are returned in CommitResult.Conflicts.
	Force bool
}

// CommitResult is the outcome of a committed changeset.
type CommitResult struct {
	// Seq is the sequence of the new changeset, 0 when NoOp.
	Seq uint64
	// NoOp is true when no update changed the database; nothing was written.
	NoOp bool
	// Forced and Conflicts report a changeset recorded with Force despite
	// conflicts.
	Forced    bool
	Conflicts []RefConflict
	// Updates are the effective updates that were recorded.
	Updates []AppliedUpdate
	// Changelog are the changelog entries that were appended.
	Changelog []changelog.Entry
}

// InitialState is what RegisterRepository writes together with the new
// repository, in the same transaction.
type InitialState struct {
	// Changeset, when not nil, is recorded as changeset 1 (its RepoID is
	// ignored; every update must expect the reference to be absent).
	Changeset *ChangesetInput
	// Changelog is imported changelog history. Sequences are kept as they are
	// and must be strictly increasing; entries appended by Changeset follow
	// the last one.
	Changelog []changelog.Entry
	// Reviews are imported reviews, keeping their IDs (decimal strings). The
	// next review ID is one more than the largest imported one.
	Reviews []*gitalypb.Review
}

var (
	// ErrReviewNotFound is returned for an unknown review ID.
	ErrReviewNotFound = errors.New("storage core: review not found")
	// ErrReviewVersionConflict is returned by UpdateReview when the review
	// changed since it was read.
	ErrReviewVersionConflict = errors.New("storage core: review changed concurrently")
)

// MetaStore is the strongly consistent database of the storage core. Every
// method is safe for concurrent use by any number of processes sharing the
// database. Implementations: sqlstore (SQLite and Postgres).
type MetaStore interface {
	// RegisterRepository records a new repository with its initial state in
	// one transaction. It returns ErrRepositoryExists when the key is taken.
	RegisterRepository(ctx context.Context, key RepoKey, objectFormat string, init InitialState) (Repository, CommitResult, error)
	// GetRepository returns ErrRepositoryNotFound for an unknown key.
	GetRepository(ctx context.Context, key RepoKey) (Repository, error)
	// DeleteRepository removes the repository and everything recorded for it.
	// It returns false when the repository was not known.
	DeleteRepository(ctx context.Context, key RepoKey) (bool, error)

	// CommitChangeset checks every update against the current references and
	// records the changeset, the reference changes and the changelog entries
	// in one transaction (see the CAS table of doc/graft/PLAN6.md). It returns
	// *ErrRefConflict, writing nothing, when an expectation fails and Force is
	// not set, and ErrRepositoryNotFound for an unknown repository.
	CommitChangeset(ctx context.Context, in ChangesetInput) (CommitResult, error)
	// ListChangesets returns the changesets with after < seq <= upTo in
	// ascending order, with their updates.
	ListChangesets(ctx context.Context, repoID int64, after, upTo uint64) ([]Changeset, error)
	// Refs returns every current reference and the head sequence they
	// correspond to, read in one consistent snapshot.
	Refs(ctx context.Context, repoID int64) (map[string]Target, uint64, error)
	// ReportApplied records how far a node's cache is (observability only).
	ReportApplied(ctx context.Context, repoID int64, nodeID string, seq uint64) error

	// ScanChangelog returns up to limit changelog entries with a sequence
	// greater than after, in ascending order. limit <= 0 means no limit.
	ScanChangelog(ctx context.Context, repoID int64, after uint64, limit int) ([]changelog.Entry, error)
	// ImportHistory brings history written outside the database (shadow mode
	// writes the file changelog and file reviews) into a tracked repository, in
	// one transaction (doc/graft/PLAN6.md, LOCK AND HISTORY RULE). The changelog
	// entries keep their sequences, which must be strictly increasing and
	// greater than the repository's log sequence, else nothing is written and an
	// error is returned; the log sequence becomes the last one. A review is
	// written when ReviewNeedsImport says so (its ID is new, or it is the
	// database review changed in the file since an earlier import; a replaced
	// review gets the next version) and skipped otherwise; the next review ID
	// becomes one more than the largest review ID. References are not touched. It returns the numbers of
	// entries and reviews written, and ErrRepositoryNotFound for an unknown
	// repository.
	ImportHistory(ctx context.Context, repoID int64, entries []changelog.Entry, reviews []*gitalypb.Review) (importedEntries, importedReviews int, err error)

	// InsertReview allocates the next review ID of the repository, calls build
	// with it and stores the returned review (version 1), atomically.
	InsertReview(ctx context.Context, repoID int64, build func(id string) (*gitalypb.Review, error)) (*gitalypb.Review, error)
	// GetReview returns the review and its version, ErrReviewNotFound if unknown.
	GetReview(ctx context.Context, repoID int64, id string) (*gitalypb.Review, int64, error)
	// ListReviews returns the reviews of the repository newest first
	// (descending numeric ID), keeping only those targeting targetBranch when
	// it is not empty and, unless includeClosed, only those whose state is
	// Review_STATE_OPEN_UNSPECIFIED; at most limit of them (limit <= 0 means
	// no limit). This is exactly the contract of review/store.Store.List.
	ListReviews(ctx context.Context, repoID int64, targetBranch string, includeClosed bool, limit int) ([]*gitalypb.Review, error)
	// UpdateReview stores r if the review is still at version, which becomes
	// version+1. It returns ErrReviewVersionConflict otherwise.
	UpdateReview(ctx context.Context, repoID int64, id string, version int64, r *gitalypb.Review) error

	// Close releases the database.
	Close() error
}
