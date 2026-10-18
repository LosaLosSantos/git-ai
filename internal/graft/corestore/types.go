// Package corestore is the Graft storage core: the repository state lives in a
// strongly consistent database (a per-repository write-ahead log of changesets
// with a compare-and-swap per reference) plus content-addressed packs in object
// storage, and the git repositories of the Gitaly nodes are caches that any
// node can rebuild. See doc/graft/PLAN6.md (binding) and doc/graft/STORAGE.md.
//
// This package holds the types and interfaces shared by the implementation
// packages:
//
//   - sqlstore: the MetaStore on SQLite or Postgres.
//   - blobstore: the BlobStore on a gocloud bucket.
//   - gitops: the git operations (packs, references, watermark).
//   - voter: the write path in the reference-transaction hook.
//   - syncer: the catch-up and hydration of the local caches.
//   - gate: the gRPC interceptors of authoritative mode.
package corestore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Mode is the storage core mode, see config.GraftStorage.Mode.
type Mode string

const (
	// ModeOff disables the storage core.
	ModeOff Mode = "off"
	// ModeShadow records every reference transaction in the database and the
	// bucket without ever rejecting a write.
	ModeShadow Mode = "shadow"
	// ModeAuthoritative makes the database the source of truth.
	ModeAuthoritative Mode = "authoritative"
)

// ParseMode parses a configured mode. The empty string is ModeOff.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeOff:
		return ModeOff, nil
	case ModeShadow, ModeAuthoritative:
		return Mode(s), nil
	default:
		return "", fmt.Errorf("unknown storage mode %q (want off, shadow or authoritative)", s)
	}
}

// RepoKey identifies a repository on every node: the storage name and the
// relative path of the gitalypb.Repository.
type RepoKey struct {
	Storage      string
	RelativePath string
}

// String returns "storage:relative/path", for logs.
func (k RepoKey) String() string { return k.Storage + ":" + k.RelativePath }

// Target is the value of a reference: a hex object ID, or "ref:<name>" for a
// symbolic reference. The empty Target means that the reference is absent.
type Target string

// SymbolicPrefix prefixes the Target of a symbolic reference.
const SymbolicPrefix = "ref:"

// SymbolicTarget returns the Target of a symbolic reference to name.
func SymbolicTarget(name string) Target { return Target(SymbolicPrefix + name) }

// Absent reports whether the reference does not exist.
func (t Target) Absent() bool { return t == "" }

// IsSymbolic reports whether t is the value of a symbolic reference.
func (t Target) IsSymbolic() bool { return strings.HasPrefix(string(t), SymbolicPrefix) }

// SymbolicName returns the name a symbolic reference points to, or "" when
// t is not symbolic.
func (t Target) SymbolicName() string {
	if !t.IsSymbolic() {
		return ""
	}
	return string(t)[len(SymbolicPrefix):]
}

// Expect is the condition a RefUpdate puts on the current database value of
// its reference. See the CAS table in doc/graft/PLAN6.md (RULE 5).
type Expect int

const (
	// ExpectValue requires the current value to equal RefUpdate.Old; an empty
	// Old requires the reference to be absent.
	ExpectValue Expect = iota
	// ExpectAny updates the reference whatever its current value (forced).
	ExpectAny
)

// RefUpdate is one reference change of a changeset.
type RefUpdate struct {
	// Name is the fully qualified reference name ("refs/..." or "HEAD"). It may
	// hold bytes that are not valid UTF-8.
	Name string
	// Expect is the condition on the current value.
	Expect Expect
	// Old is the expected current value when Expect is ExpectValue.
	Old Target
	// New is the new value; empty deletes the reference.
	New Target
}

// AppliedUpdate is the effective change of one reference in a recorded
// changeset: Old is the actual previous database value.
type AppliedUpdate struct {
	Name string
	Old  Target
	New  Target
}

// Repository is a repository known to the database.
type Repository struct {
	// ID is never reused: a repository that is removed and created again gets
	// a new ID, which invalidates the caches of the old one.
	ID  int64
	Key RepoKey
	// ObjectFormat is "sha1" or "sha256".
	ObjectFormat string
	// HeadSeq is the sequence of the last changeset, 0 when there is none.
	HeadSeq uint64
	// LogSeq is the sequence of the last changelog entry, 0 when there is none.
	LogSeq    uint64
	CreatedAt time.Time
}

// Changeset is one recorded changeset of the write-ahead log.
type Changeset struct {
	RepoID    int64
	Seq       uint64
	Operation string
	Actor     string
	NodeID    string
	// PackKey is the bucket key of the pack with the changeset's new objects,
	// empty when it brought no objects.
	PackKey   string
	PackBytes int64
	// Forced is true when shadow mode recorded the changeset despite a CAS
	// mismatch.
	Forced    bool
	CreatedAt time.Time
	// Updates are the effective reference changes, in order.
	Updates []AppliedUpdate
}

// Checkpoint is a consolidated state of a repository at a changeset sequence:
// one pack with every object reachable from the references at Seq and from the
// roots of the reviews open when it was built, plus the reference snapshot at
// Seq (MetaStore.CheckpointRefs). Hydration starts from the newest checkpoint
// and replays only the changesets after it (doc/graft/PLAN7.md).
type Checkpoint struct {
	RepoID    int64
	Seq       uint64
	PackKey   string
	PackBytes int64
	// CreatedAt is a database time (doc/graft/PLAN7.md, CLOCK RULE).
	CreatedAt time.Time
}

// RemovedRepository is the tombstone of a removed repository whose objects
// are still to be deleted from the bucket.
type RemovedRepository struct {
	RepoID int64
	Key    RepoKey
	// RemovedAt is a database time (doc/graft/PLAN7.md, CLOCK RULE).
	RemovedAt time.Time
}

// GCState is what garbage collection decides on for one repository, read in
// one database snapshot (doc/graft/PLAN7.md, PACK OWNERSHIP RULE 5).
type GCState struct {
	// Now is the database time of the snapshot: every row committed before it is
	// part of the snapshot.
	Now time.Time
	// Packs maps every changeset pack key to the greatest sequence of the
	// changesets that reference it (a legacy key can be shared by several).
	Packs map[string]uint64
	// Checkpoints are the checkpoints, ascending by sequence.
	Checkpoints []Checkpoint
	// Orphans maps the keys observed unreferenced to the database time they were
	// first observed.
	Orphans map[string]time.Time
}

// RefConflict is one reference whose current value did not satisfy its
// RefUpdate.
type RefConflict struct {
	Name     string
	Expected Target
	Actual   Target
}

// ErrRefConflict is returned by MetaStore.CommitChangeset when at least one
// reference does not satisfy its expectation; nothing was written.
type ErrRefConflict struct {
	Conflicts []RefConflict
}

func (e *ErrRefConflict) Error() string {
	if len(e.Conflicts) == 0 {
		return "storage core: reference conflict"
	}
	c := e.Conflicts[0]
	return fmt.Sprintf("storage core: reference %q is at %q but expected %q (%d conflicting references)",
		c.Name, c.Actual, c.Expected, len(e.Conflicts))
}

var (
	// ErrRepositoryNotFound is returned when the database does not know the repository.
	ErrRepositoryNotFound = errors.New("storage core: repository not found")
	// ErrRepositoryExists is returned when registering a repository that is already known.
	ErrRepositoryExists = errors.New("storage core: repository already exists")
	// ErrBlobNotFound is returned by BlobStore.Get for a missing key.
	ErrBlobNotFound = errors.New("storage core: blob not found")
	// ErrBlobExists is returned by BlobStore.Put for a key that exists already:
	// objects are never overwritten nor shared between writers.
	ErrBlobExists = errors.New("storage core: blob already exists")
	// ErrCheckpointNotFound is returned for a checkpoint sequence the
	// repository has no checkpoint at.
	ErrCheckpointNotFound = errors.New("storage core: checkpoint not found")
	// ErrCheckpointRootsChanged is returned by MetaStore.CreateCheckpoint when an
	// open review has a head or base commit that the checkpoint's pack does not
	// include: the review was created or moved during the build
	// (doc/graft/PLAN7.md, REVIEW ROOTS RULE R2).
	ErrCheckpointRootsChanged = errors.New("storage core: the open reviews changed during the checkpoint build")
	// ErrReviewRootsStale is returned by MetaStore.InsertReview and UpdateReview
	// when a checkpoint newer than the snapshot the review's commits were checked
	// at was recorded in between: the caller checks the commits again against a
	// new snapshot (doc/graft/PLAN7.md, REVIEW ROOTS RULE R1).
	ErrReviewRootsStale = errors.New("storage core: a checkpoint was recorded after the review's commits were checked")
	// ErrPackDeadline is returned when a pack upload, or the commit of the row
	// that references it, would end after the write deadline: garbage collection
	// may then already treat the pack as an orphan (doc/graft/PLAN7.md, PACK
	// OWNERSHIP RULE 4). The write fails like any storage failure.
	ErrPackDeadline = errors.New("storage core: the pack upload is older than the write deadline")
	// ErrBlobCorrupt is returned by the reader of BlobStore.Get when the content
	// does not hash to its key.
	ErrBlobCorrupt = errors.New("storage core: blob content does not match its key")
)

// packNonceSize is the number of random bytes that make a pack key unique.
const packNonceSize = 16

// NewPackKey returns a new owned bucket key for a pack of the repository whose
// bytes have the given SHA-256: "repos/<repo_id>/packs/<hex sha256>-<hex
// nonce>.pack", with a random nonce drawn for this one upload. Every upload uses a
// new key, so a pack object is never shared between two rows and a write never
// relies on an object it did not write (doc/graft/PLAN7.md, PACK OWNERSHIP RULE).
func NewPackKey(repoID int64, sum [32]byte) string {
	var nonce [packNonceSize]byte
	// crypto/rand.Read never fails: it aborts the program instead.
	_, _ = rand.Read(nonce[:])
	return ownedPackKey(repoID, sum, nonce)
}

func ownedPackKey(repoID int64, sum [32]byte, nonce [packNonceSize]byte) string {
	return fmt.Sprintf("repos/%d/packs/%x-%x.pack", repoID, sum, nonce)
}

func legacyPackKey(repoID int64, sum [32]byte) string {
	return fmt.Sprintf("repos/%d/packs/%x.pack", repoID, sum)
}

// ParsePackKey returns the repository ID and the SHA-256 encoded in an owned
// pack key (NewPackKey) or in a legacy one, "repos/<repo_id>/packs/<hex
// sha256>.pack", written by v0.1.0 and still referenced by its changesets. It
// returns false for anything else, including non-canonical spellings (uppercase
// hex, leading zeros).
func ParsePackKey(key string) (repoID int64, sum [32]byte, ok bool) {
	repoID, sum, _, ok = parsePackKey(key)
	return repoID, sum, ok
}

// IsOwnedPackKey reports whether key is an owned pack key, the only kind a
// writer may produce.
func IsOwnedPackKey(key string) bool {
	_, _, owned, ok := parsePackKey(key)
	return ok && owned
}

func parsePackKey(key string) (repoID int64, sum [32]byte, owned, ok bool) {
	rest, found := strings.CutPrefix(key, "repos/")
	if !found {
		return 0, sum, false, false
	}
	idPart, rest, found := strings.Cut(rest, "/packs/")
	if !found {
		return 0, sum, false, false
	}
	name, found := strings.CutSuffix(rest, ".pack")
	if !found {
		return 0, sum, false, false
	}
	hexSum, hexNonce, owned := strings.Cut(name, "-")
	if len(hexSum) != 2*len(sum) || (owned && len(hexNonce) != 2*packNonceSize) {
		return 0, sum, false, false
	}

	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		return 0, sum, false, false
	}
	if _, err := hex.Decode(sum[:], []byte(hexSum)); err != nil {
		return 0, [32]byte{}, false, false
	}

	// Only the canonical spelling is a pack key (lowercase hex, no leading zeros).
	canonical := legacyPackKey(id, sum)
	if owned {
		var nonce [packNonceSize]byte
		if _, err := hex.Decode(nonce[:], []byte(hexNonce)); err != nil {
			return 0, [32]byte{}, false, false
		}
		canonical = ownedPackKey(id, sum, nonce)
	}
	if canonical != key {
		return 0, [32]byte{}, false, false
	}
	return id, sum, owned, true
}

// IsChangelogRef reports whether updates of the reference are recorded in the
// Graft changelog: branches and tags, as changelog.ParseUpdates does.
func IsChangelogRef(name string) bool {
	return strings.HasPrefix(name, "refs/heads/") || strings.HasPrefix(name, "refs/tags/")
}
