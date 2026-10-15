// Package store persists code reviews in the git directory of a repository.
//
// Layout: <git dir>/graft/reviews/<id>.json holds one review as protojson
// (without requirements and approved, which callers recompute on read),
// <git dir>/graft/reviews/next_id holds the next decimal id, and
// <git dir>/graft/reviews/.lock is the file lock serialising mutations across
// processes. Mutations also hold an in-process mutex per directory. Every
// write goes through a temporary file, an fsync and a rename.
package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ErrNotFound is returned when a review does not exist.
var ErrNotFound = errors.New("review not found")

const (
	reviewsDirRel = "graft/reviews"
	lockFileName  = ".lock"
	nextIDName    = "next_id"
	reviewSuffix  = ".json"
)

// Store reads and writes the reviews of one repository. It is cheap to create;
// Open does not touch the disk.
type Store struct {
	repoPath string
}

// Open returns a Store for the repository at repoPath (its git directory).
func Open(repoPath string) *Store {
	return &Store{repoPath: repoPath}
}

// dir returns the reviews directory.
func (s *Store) dir() string {
	return filepath.Join(s.repoPath, filepath.FromSlash(reviewsDirRel))
}

// Create stores a new review. It assigns the id, created_at and updated_at.
// requirements and approved are not stored. The returned review is a copy.
func (s *Store) Create(r *gitalypb.Review) (*gitalypb.Review, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	dir := s.dir()
	nextID, err := readNextID(dir)
	if err != nil {
		return nil, err
	}

	// The counter is advanced before the review is written: a crash in between
	// leaves a gap in the ids, never a reused id.
	if err := writeFileAtomic(dir, nextIDName, []byte(strconv.FormatUint(nextID+1, 10)+"\n")); err != nil {
		return nil, fmt.Errorf("advance next id: %w", err)
	}

	now := timestamppb.Now()
	stored := proto.Clone(r).(*gitalypb.Review)
	stored.Id = strconv.FormatUint(nextID, 10)
	stored.CreatedAt = now
	stored.UpdatedAt = now
	stored.Requirements = nil
	stored.Approved = false

	if err := s.writeReview(stored); err != nil {
		return nil, err
	}

	return proto.Clone(stored).(*gitalypb.Review), nil
}

// Get returns the review with the given id, or ErrNotFound.
func (s *Store) Get(id string) (*gitalypb.Review, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}

	data, err := os.ReadFile(filepath.Join(s.dir(), id+reviewSuffix))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read review %s: %w", id, err)
	}

	var r gitalypb.Review
	if err := (protojson.UnmarshalOptions{}).Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode review %s: %w", id, err)
	}

	return &r, nil
}

// List returns the reviews newest first. A non-empty targetBranch keeps only
// the reviews targeting that branch. Landed and closed reviews are kept only
// when includeClosed is set. A limit of zero or less means no limit.
func (s *Store) List(targetBranch string, includeClosed bool, limit int) ([]*gitalypb.Review, error) {
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list reviews: %w", err)
	}

	var ids []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, reviewSuffix) {
			continue
		}
		if id := strings.TrimSuffix(name, reviewSuffix); validID(id) {
			ids = append(ids, id)
		}
	}

	// Ids are decimal counters without leading zeros: a longer id is always
	// newer, and equal lengths compare lexically.
	sort.Slice(ids, func(i, j int) bool {
		if len(ids[i]) != len(ids[j]) {
			return len(ids[i]) > len(ids[j])
		}
		return ids[i] > ids[j]
	})

	var reviews []*gitalypb.Review
	for _, id := range ids {
		if limit > 0 && len(reviews) >= limit {
			break
		}

		r, err := s.Get(id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Removed between the listing and the read.
				continue
			}
			return nil, err
		}

		if targetBranch != "" && string(r.GetTargetBranch()) != targetBranch {
			continue
		}
		if !includeClosed && r.GetState() != gitalypb.Review_STATE_OPEN_UNSPECIFIED {
			continue
		}

		reviews = append(reviews, r)
	}

	return reviews, nil
}

// Update reads the review with the given id under the lock, passes it to fn
// and stores the result, with updated_at refreshed. When fn returns an error
// nothing is written and the error is returned unchanged. The returned review
// is a copy.
func (s *Store) Update(id string, fn func(*gitalypb.Review) error) (*gitalypb.Review, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	r, err := s.Get(id)
	if err != nil {
		return nil, err
	}

	if err := fn(r); err != nil {
		return nil, err
	}

	r.UpdatedAt = timestamppb.Now()
	if err := s.writeReview(r); err != nil {
		return nil, err
	}

	return proto.Clone(r).(*gitalypb.Review), nil
}

// writeReview stores r without its computed fields.
func (s *Store) writeReview(r *gitalypb.Review) error {
	stored := proto.Clone(r).(*gitalypb.Review)
	stored.Requirements = nil
	stored.Approved = false

	data, err := protojson.Marshal(stored)
	if err != nil {
		return fmt.Errorf("encode review %s: %w", stored.GetId(), err)
	}

	if err := writeFileAtomic(s.dir(), stored.GetId()+reviewSuffix, data); err != nil {
		return fmt.Errorf("write review %s: %w", stored.GetId(), err)
	}

	return nil
}

// lock takes the in-process and the file lock of the reviews directory. The
// returned function releases both.
func (s *Store) lock() (func(), error) {
	dir := s.dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create reviews directory: %w", err)
	}

	mu := mutexFor(dir)
	mu.Lock()

	unlockFile, err := lockFile(filepath.Join(dir, lockFileName))
	if err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("lock reviews: %w", err)
	}

	return func() {
		unlockFile()
		mu.Unlock()
	}, nil
}

var (
	mutexesMu sync.Mutex
	mutexes   = map[string]*sync.Mutex{}
)

// mutexFor returns the in-process mutex of a reviews directory.
func mutexFor(dir string) *sync.Mutex {
	mutexesMu.Lock()
	defer mutexesMu.Unlock()

	mu, ok := mutexes[dir]
	if !ok {
		mu = &sync.Mutex{}
		mutexes[dir] = mu
	}
	return mu
}

// readNextID returns the next id, 1 when the counter does not exist yet.
func readNextID(dir string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(dir, nextIDName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 1, nil
		}
		return 0, fmt.Errorf("read next id: %w", err)
	}

	id, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("corrupt next id %q", strings.TrimSpace(string(data)))
	}

	return id, nil
}

// writeFileAtomic replaces dir/name with data: it writes a temporary file in
// dir, fsyncs it, renames it over the target and fsyncs dir.
func writeFileAtomic(dir, name string, data []byte) (returnErr error) {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}

	defer func() {
		if returnErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}

	return syncDir(dir)
}

// validID reports whether id is a review id: a decimal counter without a
// leading zero. Anything else cannot name a file in the reviews directory.
func validID(id string) bool {
	if id == "" || id == "0" || id[0] == '0' || len(id) > 20 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
