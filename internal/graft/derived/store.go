// Package derived computes and caches history data that Git would otherwise
// recompute on every request: the last commit that changed a path (lastmod),
// the blame of a file at that commit, and first-parent path histories. Every
// value is a pure function of immutable commits, so cached entries never need
// invalidation. See doc/graft/PLAN.md, "WP6".
package derived

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
)

// storeDirName is the directory below the repository's Git directory that holds
// the derived-data store.
const storeDirName = "graft/derived"

var kindRegexp = regexp.MustCompile(`\A[a-z][a-z-]*\z`)

// Store is a key-value store of derived values kept on disk inside a repository.
// Entries are written atomically (temporary file, rename), so concurrent readers
// see either no entry or a complete one; see Put for crashes. A key's valid value never changes, but an
// entry may be replaced when it turns out to be corrupt.
//
// A nil *Store is valid and behaves as an empty store that discards writes;
// it is what callers use when derived-data caching is disabled.
type Store struct {
	dir string
}

// NewStore returns a store rooted at <repoPath>/graft/derived. The directory is
// created lazily on the first write.
func NewStore(repoPath string) *Store {
	return &Store{dir: filepath.Join(repoPath, filepath.FromSlash(storeDirName))}
}

// Key returns the store key of the value of the given kind computed for commit
// and path: the hex encoded SHA-256 of the NUL-separated fields.
func Key(kind string, commit git.ObjectID, path string) string {
	hash := sha256.New()
	for _, field := range []string{kind, commit.String(), path} {
		// The separator makes the encoding unambiguous: no field contains NUL.
		_, _ = hash.Write([]byte(field))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// Get returns the value stored under kind and key. The boolean is false when no
// value is stored.
func (s *Store) Get(kind, key string) ([]byte, bool, error) {
	if s == nil {
		return nil, false, nil
	}

	path, err := s.entryPath(kind, key)
	if err != nil {
		return nil, false, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading derived entry: %w", err)
	}

	return data, true, nil
}

// Put stores data under kind and key, replacing any entry that is already
// stored. Callers only write when they have no valid value for the key, so an
// existing entry is replaced only when it is corrupt.
//
// Entries are not fsync'd. Writing a file and fsync'ing it costs milliseconds,
// and a walk stores several entries (see lastMod), so syncing would dominate the cost of
// a cold query. The cost of skipping it is bounded: after a crash an entry may
// be missing or empty, and readers reject such data (see decodeValue) and
// recompute it. The value is therefore never trusted without its full content.
func (s *Store) Put(kind, key string, data []byte) error {
	if s == nil {
		return nil
	}

	path, err := s.entryPath(kind, key)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, mode.Directory); err != nil {
		return fmt.Errorf("creating derived directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temporary derived entry: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		return errors.Join(fmt.Errorf("writing derived entry: %w", err), tmp.Close(), os.Remove(tmpName))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(fmt.Errorf("closing derived entry: %w", err), os.Remove(tmpName))
	}
	if err := os.Chmod(tmpName, mode.File); err != nil {
		return errors.Join(fmt.Errorf("chmod derived entry: %w", err), os.Remove(tmpName))
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.Join(fmt.Errorf("publishing derived entry: %w", err), os.Remove(tmpName))
	}

	return nil
}

// entryPath returns the file path of an entry. The layout is
// <dir>/<kind>/<first two hex characters of key>/<key>.
func (s *Store) entryPath(kind, key string) (string, error) {
	if !kindRegexp.MatchString(kind) {
		return "", fmt.Errorf("invalid derived kind %q", kind)
	}
	if len(key) != sha256.Size*2 {
		return "", fmt.Errorf("invalid derived key length %d", len(key))
	}
	for _, c := range key {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", fmt.Errorf("invalid derived key %q", key)
		}
	}
	return filepath.Join(s.dir, kind, key[:2], key), nil
}
