// Package blobstore implements corestore.BlobStore on a gocloud bucket. Owned
// by WP52 (doc/graft/PLAN6.md).
//
// Conditional writes. Put writes with blob.WriterOptions{IfNotExist: true}.
// This was probed experimentally with gocloud.dev v0.46.0: the memblob and
// fileblob drivers support IfNotExist, and an existing key is reported by
// Close as gcerrors.FailedPrecondition. (The gocloud documentation names
// gcerrors.PreconditionFailed, which these drivers do not return.) Put treats
// FailedPrecondition as success, and it checks Exists first, so an existing
// key never reads the content. No plain-write fallback is implemented for a
// driver that reports gcerrors.Unimplemented: no registered driver does so
// (checked against gocloud.dev v0.46.0: s3blob, gcsblob, azureblob, memblob and
// fileblob all implement IfNotExist), and by the time Close reports it the
// reader is consumed, so a fallback would need a second copy of the content.
// Such an error is returned instead.
//
// Atomic objects. gocloud writers commit the object on Close and discard it
// when the writer's context is cancelled before Close (probed: a cancelled
// writer leaves no object). Put cancels before Close on every failure, so a
// reader never sees a partial object.
//
// Durability of file buckets. The fileblob driver renames its temporary file
// into place on Close without any fsync. A changeset row in the database
// (synchronous=FULL) can therefore outlive a pack whose data never reached the
// disk, and because Put skips existing keys, such a pack would never be
// repaired. For file buckets Put fsyncs the content before Close, and fsyncs
// the directories that hold the new entry (up to the bucket root) after Close.
// The mem and remote drivers commit their objects durably themselves.
//
// Integrity. Pack keys (corestore.ParsePackKey) are verified: Put hashes the
// content while writing and refuses content that does not match its key, and
// Get returns a reader that hashes the content and returns
// corestore.ErrBlobCorrupt instead of io.EOF on a mismatch or a short read.
// Content delivered before the end of the stream is not verified until the
// reader reaches its end, so callers must treat any read error as failure.
package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/s3blob"
	"gocloud.dev/gcerrors"

	_ "gocloud.dev/blob/azureblob" // register Azure driver
	_ "gocloud.dev/blob/gcsblob"   // register Google Cloud driver
	_ "gocloud.dev/blob/memblob"   // register in-memory driver
)

// Store is a bucket of the storage core.
type Store struct {
	bucket *blob.Bucket
	// root is the cleaned directory of a file bucket and empty for any other
	// driver. Put makes the writes of a file bucket durable from it.
	root string
}

var _ corestore.BlobStore = (*Store)(nil)

// fileContentType is the content type of file bucket writers. Setting it makes
// the driver open its file immediately instead of after sniffing the first 512
// bytes. File buckets store no metadata, so the type is not persisted.
const fileContentType = "application/octet-stream"

// syncHook, when set by a test, observes every fsync of a file bucket (isDir
// is true for a directory) and may fail it. Production code leaves it nil.
var syncHook func(path string, isDir bool) error

// Open opens the bucket at a gocloud URL: file:///abs/dir, mem://, s3://...,
// gs://..., azblob://... (drivers registered as in internal/backup/sink.go).
// As in internal/backup/sink.go, a scheme may carry a prefix before a "+" (the
// part after the last "+" selects the driver), and S3 endpoints default to
// request_checksum_calculation=when_required.
//
// Put is create-only: it writes with blob.WriterOptions{IfNotExist: true} and
// treats gcerrors.FailedPrecondition (the key exists) as success, because
// keys are content-addressed. Writes go to the final key only once the whole
// content was written (gocloud writers commit on Close), so a reader never
// sees a partial object. Get returns corestore.ErrBlobNotFound for
// gcerrors.NotFound, and for pack keys a reader that hashes the content and
// returns corestore.ErrBlobCorrupt instead of io.EOF on mismatch.
func Open(ctx context.Context, bucketURL string) (*Store, error) {
	u, err := url.Parse(bucketURL)
	if err != nil {
		return nil, fmt.Errorf("blobstore: parse bucket URL: %w", err)
	}

	scheme := u.Scheme
	if i := strings.LastIndex(scheme, "+"); i > 0 {
		scheme = scheme[i+1:]
	}
	u.Scheme = scheme

	var bucket *blob.Bucket
	var root string
	if scheme == fileblob.Scheme {
		if u.Path == "" {
			return nil, errors.New("blobstore: file bucket URL without a path")
		}
		root = filepath.Clean(u.Path)
		// fileblob creates missing directories with mode 0777, so the root
		// directory is created here with 0700 (see internal/backup/sink.go).
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, fmt.Errorf("blobstore: create bucket directory: %w", err)
		}
		bucket, err = fileblob.OpenBucket(root, &fileblob.Options{
			NoTempDir: true,
			Metadata:  fileblob.MetadataDontWrite,
		})
	} else {
		if scheme == s3blob.Scheme {
			bucketURL = withS3DefaultChecksumCalculation(u)
		} else {
			bucketURL = u.String()
		}
		bucket, err = blob.OpenBucket(ctx, bucketURL)
	}
	if err != nil {
		return nil, fmt.Errorf("blobstore: open bucket: %w", err)
	}

	return &Store{bucket: bucket, root: root}, nil
}

// withS3DefaultChecksumCalculation adds request_checksum_calculation=when_required
// to the S3 URL if a custom endpoint is configured and the parameter is not
// already specified. This keeps third-party S3 providers working that do not
// support the AWS SDK checksums. It mirrors internal/backup/sink.go.
func withS3DefaultChecksumCalculation(parsed *url.URL) string {
	query := parsed.Query()
	hasCustomEndpoint := query.Get("endpoint") != ""
	checksumNotSpecified := query.Get("request_checksum_calculation") == ""

	if hasCustomEndpoint && checksumNotSpecified {
		query.Set("request_checksum_calculation", "when_required")
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}

// Put implements corestore.BlobStore.
func (s *Store) Put(ctx context.Context, key string, r io.Reader) error {
	exists, err := s.bucket.Exists(ctx, key)
	if err != nil {
		return fmt.Errorf("blobstore: check %q: %w", key, err)
	}
	if exists {
		return nil
	}

	writeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	opts := &blob.WriterOptions{IfNotExist: true}
	var file *os.File
	if s.root != "" {
		opts.ContentType = fileContentType
		opts.BeforeWrite = func(asFunc func(any) bool) error {
			var f *os.File
			if !asFunc(&f) {
				return errors.New("blobstore: file bucket writer does not expose *os.File")
			}
			file = f
			return nil
		}
	}

	w, err := s.bucket.NewWriter(writeCtx, key, opts)
	if err != nil {
		return fmt.Errorf("blobstore: new writer %q: %w", key, err)
	}

	// abort discards the object: a writer whose context is cancelled before
	// Close commits nothing.
	abort := func(err error) error {
		cancel()
		_ = w.Close()
		return err
	}

	_, want, isPack := corestore.ParsePackKey(key)
	var dst io.Writer = w
	var hasher hash.Hash
	if isPack {
		hasher = sha256.New()
		dst = io.MultiWriter(w, hasher)
	}

	if _, err := io.Copy(dst, r); err != nil {
		return abort(fmt.Errorf("blobstore: write %q: %w", key, err))
	}

	if isPack && !bytes.Equal(hasher.Sum(nil), want[:]) {
		return abort(fmt.Errorf("blobstore: put %q: %w", key, corestore.ErrBlobCorrupt))
	}

	if file != nil {
		// Close renames the file into place. The content must reach the disk
		// first: a database row may reference this object after a power loss.
		if err := syncFile(file); err != nil {
			return abort(fmt.Errorf("blobstore: sync %q: %w", key, err))
		}
	}

	closeErr := w.Close()
	if closeErr != nil && gcerrors.Code(closeErr) != gcerrors.FailedPrecondition {
		return fmt.Errorf("blobstore: close writer %q: %w", key, closeErr)
	}

	// The rename (or, with FailedPrecondition, the key that another writer
	// created first, whose content is the same) is durable only once its
	// directory is synced.
	if file != nil {
		if err := s.syncDirs(filepath.Dir(file.Name())); err != nil {
			return fmt.Errorf("blobstore: sync directories of %q: %w", key, err)
		}
	}

	return nil
}

// syncFile fsyncs the temporary file of a file bucket writer.
func syncFile(f *os.File) error {
	if syncHook != nil {
		if err := syncHook(f.Name(), false); err != nil {
			return err
		}
	}
	return f.Sync()
}

// syncDirs fsyncs dir and, when dir is inside the bucket root, each of its
// ancestors up to the root: the directories that Put or the driver may have
// created, and the entry that the rename created.
func (s *Store) syncDirs(dir string) error {
	inside := dir == s.root || strings.HasPrefix(dir, s.root+string(filepath.Separator))
	for d := dir; ; d = filepath.Dir(d) {
		if err := syncDir(d); err != nil {
			return err
		}
		if !inside || d == s.root {
			return nil
		}
	}
}

// syncDir fsyncs a directory so that the entries created in it are durable.
func syncDir(dir string) error {
	if syncHook != nil {
		if err := syncHook(dir, true); err != nil {
			return err
		}
	}

	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return fmt.Errorf("sync directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close directory: %w", closeErr)
	}
	return nil
}

// Get implements corestore.BlobStore.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.bucket.NewReader(ctx, key, nil)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, corestore.ErrBlobNotFound
		}
		return nil, fmt.Errorf("blobstore: get %q: %w", key, err)
	}

	_, want, isPack := corestore.ParsePackKey(key)
	if !isPack {
		return rc, nil
	}

	return &verifyingReader{rc: rc, want: want, hasher: sha256.New()}, nil
}

// Exists implements corestore.BlobStore.
func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	exists, err := s.bucket.Exists(ctx, key)
	if err != nil {
		return false, fmt.Errorf("blobstore: exists %q: %w", key, err)
	}
	return exists, nil
}

// Close implements corestore.BlobStore.
func (s *Store) Close() error {
	if err := s.bucket.Close(); err != nil {
		return fmt.Errorf("blobstore: close bucket: %w", err)
	}
	return nil
}

// verifyingReader hashes the content of a pack and reports a mismatch with its
// key, instead of io.EOF, when the content ends.
type verifyingReader struct {
	rc     io.ReadCloser
	want   [sha256.Size]byte
	hasher hash.Hash
}

// Read implements io.Reader.
func (r *verifyingReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	// hash.Hash.Write never returns an error.
	_, _ = r.hasher.Write(p[:n])

	switch {
	case err == nil:
		return n, nil
	case err == io.EOF:
		if !bytes.Equal(r.hasher.Sum(nil), r.want[:]) {
			return n, corestore.ErrBlobCorrupt
		}
		return n, io.EOF
	case errors.Is(err, io.ErrUnexpectedEOF):
		return n, corestore.ErrBlobCorrupt
	default:
		return n, err
	}
}

// Close implements io.Closer.
func (r *verifyingReader) Close() error {
	return r.rc.Close()
}
