package cachetier

import (
	"bufio"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	sha1HexLen   = 40
	sha256HexLen = 64
)

// object is a fully loaded and verified Git object.
type object struct {
	typ  gitalypb.ObjectType
	data []byte
}

// errVerification is returned when fetched or stored object contents do not
// hash to the requested object ID.
var errVerification = errors.New("object content does not match its object ID")

// validOID reports whether oid is a full-length, lowercase hexadecimal SHA-1 or
// SHA-256 object ID. The length selects the hash algorithm, which is how the
// cache tier learns the object format without repository metadata.
func validOID(oid string) bool {
	if len(oid) != sha1HexLen && len(oid) != sha256HexLen {
		return false
	}
	for i := 0; i < len(oid); i++ {
		c := oid[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// typeName returns the Git name of an object type, or false if the type cannot
// be stored as a Git object.
func typeName(typ gitalypb.ObjectType) (string, bool) {
	switch typ {
	case gitalypb.ObjectType_COMMIT:
		return "commit", true
	case gitalypb.ObjectType_BLOB:
		return "blob", true
	case gitalypb.ObjectType_TREE:
		return "tree", true
	case gitalypb.ObjectType_TAG:
		return "tag", true
	default:
		return "", false
	}
}

func parseTypeName(name string) (gitalypb.ObjectType, bool) {
	switch name {
	case "commit":
		return gitalypb.ObjectType_COMMIT, true
	case "blob":
		return gitalypb.ObjectType_BLOB, true
	case "tree":
		return gitalypb.ObjectType_TREE, true
	case "tag":
		return gitalypb.ObjectType_TAG, true
	default:
		return gitalypb.ObjectType_UNKNOWN, false
	}
}

// verifyObject recomputes the Git object ID of obj from its type, size and
// content and compares it with oid. It returns errVerification on mismatch.
func verifyObject(oid string, obj *object) error {
	name, ok := typeName(obj.typ)
	if !ok {
		return fmt.Errorf("%w: unsupported object type %v", errVerification, obj.typ)
	}

	var h hash.Hash
	switch len(oid) {
	case sha1HexLen:
		h = sha1.New()
	case sha256HexLen:
		h = sha256.New()
	default:
		return fmt.Errorf("%w: invalid object ID length %d", errVerification, len(oid))
	}

	// The header is "<type> <size>\x00" as defined by Git's object model.
	fmt.Fprintf(h, "%s %d\x00", name, len(obj.data))
	h.Write(obj.data)

	if hex.EncodeToString(h.Sum(nil)) != oid {
		return errVerification
	}
	return nil
}

// tempFilePrefix names the temporary files of objects being written. They are
// not published objects, so the store neither counts nor evicts them.
const tempFilePrefix = ".tmp-"

// diskStore persists verified objects below a root directory. Objects are
// grouped by a digest of (storage, relative path) so that repositories never
// share files, and written atomically via a temporary file and rename.
//
// The store is bounded by maxBytes. When a write exceeds the bound, the oldest
// files by modification time are removed until the store is back under 90% of
// the bound. Reads do not refresh the age of a file, so eviction is by write age.
type diskStore struct {
	root     string
	maxBytes int64

	// mu serializes accounting and eviction. used approximates the bytes below
	// root. eviction recomputes it from a directory scan.
	mu   sync.Mutex
	used int64
}

// diskFile is a file below the disk store's root.
type diskFile struct {
	path    string
	size    int64
	modTime time.Time
}

func newDiskStore(root string, maxBytes int64) (*diskStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create disk store: %w", err)
	}
	d := &diskStore{root: root, maxBytes: maxBytes}

	files, err := d.scan()
	if err != nil {
		return nil, fmt.Errorf("scan disk store: %w", err)
	}
	for _, f := range files {
		d.used += f.size
	}
	return d, nil
}

// scan lists the regular files below the root.
func (d *diskStore) scan() ([]diskFile, error) {
	var files []diskFile
	err := filepath.WalkDir(d.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		// Temporary files belong to writes still in progress. Evicting one would make
		// that write's rename fail, so only published objects are candidates.
		if strings.HasPrefix(entry.Name(), tempFilePrefix) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		files = append(files, diskFile{path: path, size: info.Size(), modTime: info.ModTime()})
		return nil
	})
	return files, err
}

// evict removes the oldest files until the store is under 90% of its bound.
func (d *diskStore) evict() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	files, err := d.scan()
	if err != nil {
		return err
	}

	var total int64
	for _, f := range files {
		total += f.size
	}

	target := d.maxBytes / 10 * 9
	if total > target {
		sort.Slice(files, func(i, j int) bool {
			if !files[i].modTime.Equal(files[j].modTime) {
				return files[i].modTime.Before(files[j].modTime)
			}
			return files[i].path < files[j].path
		})
		for _, f := range files {
			if total <= target {
				break
			}
			if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("evict object: %w", err)
			}
			total -= f.size
		}
	}

	d.used = total
	return nil
}

func (d *diskStore) path(key objectKey) string {
	repoDigest := sha256.Sum256([]byte(key.storage + "\x00" + key.relativePath))
	return filepath.Join(d.root, hex.EncodeToString(repoDigest[:]), key.oid[:2], key.oid)
}

// errObjectOverLimit is returned by diskStore.read when the stored object is
// larger than the caller may load.
var errObjectOverLimit = errors.New("stored object exceeds the read limit")

// read returns the object stored for key. A missing file yields (nil, nil). A
// file that fails to parse or verify is removed and reported as missing, so a
// corrupted entry degrades into a cache miss. An object larger than limit bytes
// is not loaded: read returns errObjectOverLimit before it allocates the data.
func (d *diskStore) read(key objectKey, limit int64) (*object, error) {
	path := d.path(key)

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open object: %w", err)
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	header, err := reader.ReadString('\n')
	if err != nil {
		return d.discard(path)
	}
	fields := strings.Fields(header)
	if len(fields) != 2 {
		return d.discard(path)
	}
	typ, ok := parseTypeName(fields[0])
	if !ok {
		return d.discard(path)
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || size < 0 {
		return d.discard(path)
	}

	// The header is untrusted: a corrupted size must not drive the allocation below.
	// The content must fill the rest of the file exactly.
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat object: %w", err)
	}
	if size != info.Size()-int64(len(header)) {
		return d.discard(path)
	}
	if size > limit {
		return nil, errObjectOverLimit
	}

	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return d.discard(path)
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		return d.discard(path)
	}

	obj := &object{typ: typ, data: data}
	if err := verifyObject(key.oid, obj); err != nil {
		return d.discard(path)
	}
	return obj, nil
}

func (d *diskStore) discard(path string) (*object, error) {
	_ = os.Remove(path)
	return nil, nil
}

// write stores obj for key atomically. The caller must have verified obj.
func (d *diskStore) write(key objectKey, obj *object) (err error) {
	name, ok := typeName(obj.typ)
	if !ok {
		return fmt.Errorf("unsupported object type %v", obj.typ)
	}

	path := d.path(key)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create object directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, tempFilePrefix+"*")
	if err != nil {
		return fmt.Errorf("create temporary object: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	header := fmt.Sprintf("%s %d\n", name, len(obj.data))

	w := bufio.NewWriter(tmp)
	if _, err = w.WriteString(header); err != nil {
		return fmt.Errorf("write object header: %w", err)
	}
	if _, err = w.Write(obj.data); err != nil {
		return fmt.Errorf("write object data: %w", err)
	}
	if err = w.Flush(); err != nil {
		return fmt.Errorf("flush object: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync object: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close object: %w", err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("publish object: %w", err)
	}

	d.mu.Lock()
	d.used += int64(len(header) + len(obj.data))
	overBudget := d.used > d.maxBytes
	d.mu.Unlock()

	if overBudget {
		if err := d.evict(); err != nil {
			return fmt.Errorf("enforce disk budget: %w", err)
		}
	}
	return nil
}
