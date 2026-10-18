package corestore

import (
	"context"
	"io"
	"path"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
)

// BlobStore is the object storage of the storage core. Objects are immutable
// and every pack object belongs to exactly one database row: each upload uses a
// new owned key (NewPackKey), and Put never overwrites nor shares an object
// (doc/graft/PLAN7.md, PACK OWNERSHIP RULE). Implementations: blobstore (any
// gocloud bucket URL).
type BlobStore interface {
	// Put stores the content under key, which must not exist: an existing key is
	// ErrBlobExists and the stored object is left unchanged. Of concurrent Puts
	// of one key exactly one succeeds. For a pack key (see ParsePackKey) the
	// content must hash to the key's SHA-256, else ErrBlobCorrupt. A Put whose
	// context ends before it commits stores nothing. After any error the caller
	// must not reference the key: an object a failed Put may have left (its
	// commit succeeded but making it durable failed) is an orphan that garbage
	// collection reclaims, never an object another writer relies on.
	Put(ctx context.Context, key string, r io.Reader) error
	// Get returns the content, or ErrBlobNotFound. For a pack key (see
	// ParsePackKey) the reader returns ErrBlobCorrupt instead of io.EOF when
	// the content does not hash to the key.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Exists reports whether the key exists.
	Exists(ctx context.Context, key string) (bool, error)
	// List calls fn for every object whose key starts with prefix, in no
	// defined order. Directories are not objects. An error returned by fn stops
	// the iteration and is returned as it is.
	List(ctx context.Context, prefix string, fn func(BlobInfo) error) error
	// Delete removes the object. A key that does not exist is success, so a
	// collection that is run again never fails on an object it deleted before.
	Delete(ctx context.Context, key string) error
	// Close releases the bucket.
	Close() error
}

// BlobInfo describes a stored object. It has no modification time on purpose:
// garbage collection never decides on bucket clocks (doc/graft/PLAN7.md, CLOCK
// RULE).
type BlobInfo struct {
	Key  string
	Size int64
}

// Watermark is how far the local cache of a repository is: every changeset
// of repository RepoID up to and including Seq is applied locally.
type Watermark struct {
	RepoID int64  `json:"repo_id"`
	Seq    uint64 `json:"seq"`
}

// LocalApplier is told by the write path that a changeset made on this node
// is now committed in the local repository too, so the node's watermark can
// advance without downloading its own pack. Implemented by syncer.Syncer.
// It must never block on a lock that a catch-up holds.
type LocalApplier interface {
	NoteCommitted(ctx context.Context, repoPath string, repoID int64, seq uint64)
}

// Freshener brings the local cache of a repository up to date with the
// database before a request is served. Implemented by syncer.Syncer. Both
// methods are no-ops returning nil errors (and ok=false) unless the mode is
// authoritative.
type Freshener interface {
	// EnsureFresh catches up or hydrates the local repository, see
	// doc/graft/PLAN6.md, "Read path and catch-up".
	EnsureFresh(ctx context.Context, repo storage.Repository) error
	// Applied returns the local watermark of the repository; ok is false when
	// the local repository has none.
	Applied(ctx context.Context, repo storage.Repository) (w Watermark, ok bool, err error)
}

// Core is the opened storage core. A nil *Core means the storage core is off.
type Core struct {
	Mode   Mode
	NodeID string
	Meta   MetaStore
	Blobs  BlobStore
	// SyncTimeout bounds EnsureFresh, ShadowTimeout bounds the shadow work of
	// one reference transaction.
	SyncTimeout   time.Duration
	ShadowTimeout time.Duration
}

// Enabled reports whether the storage core is on (shadow or authoritative).
func (c *Core) Enabled() bool { return c != nil && c.Mode != ModeOff && c.Mode != "" }

// Authoritative reports whether the database is the source of truth.
func (c *Core) Authoritative() bool { return c != nil && c.Mode == ModeAuthoritative }

// Shadow reports whether the storage core runs in shadow mode.
func (c *Core) Shadow() bool { return c != nil && c.Mode == ModeShadow }

// Close releases the database and the bucket.
func (c *Core) Close() error {
	if c == nil {
		return nil
	}
	var firstErr error
	if c.Meta != nil {
		firstErr = c.Meta.Close()
	}
	if c.Blobs != nil {
		if err := c.Blobs.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// KeyOf returns the RepoKey of a gitaly repository. The relative path is
// normalized the way the storage locator resolves it (filepath.Join of the
// storage root and the relative path), so that two spellings of one directory
// ("a//b.git", "./a/b.git", "a/b.git/") are one key and one database row.
func KeyOf(repo storage.Repository) RepoKey {
	return RepoKey{Storage: repo.GetStorageName(), RelativePath: NormalizeRelativePath(repo.GetRelativePath())}
}

// NormalizeRelativePath cleans a repository's relative path: slashes are
// collapsed, "." elements and trailing slashes removed, ".." resolved, and the
// result has no leading slash. Paths that escape the storage root are rejected
// by the locator before any storage core call.
func NormalizeRelativePath(relativePath string) string {
	return strings.TrimPrefix(path.Clean("/"+relativePath), "/")
}

// NopFreshener is the Freshener of a node whose storage core is off.
type NopFreshener struct{}

// EnsureFresh does nothing.
func (NopFreshener) EnsureFresh(context.Context, storage.Repository) error { return nil }

// Applied reports no watermark.
func (NopFreshener) Applied(context.Context, storage.Repository) (Watermark, bool, error) {
	return Watermark{}, false, nil
}
