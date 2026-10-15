package superfan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// maxCursorFile bounds the cursor file that is read.
const maxCursorFile = 4096

// CursorStore persists the sequence of the last event whose digests were
// delivered, so that a restart resumes after it. The file is JSON:
// {"sequence": N}. Writes are atomic (temp file, fsync, rename).
type CursorStore struct {
	path string
}

// NewCursorStore returns a store for the file at path.
func NewCursorStore(path string) *CursorStore {
	return &CursorStore{path: path}
}

// Load returns the saved sequence, or 0 when there is no cursor yet.
func (c *CursorStore) Load() (uint64, error) {
	f, err := os.Open(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("superfan: open cursor: %w", err)
	}
	defer f.Close()
	data, err := readAllLimited(f, maxCursorFile)
	if err != nil {
		return 0, fmt.Errorf("superfan: read cursor: %w", err)
	}
	var doc struct {
		Sequence uint64 `json:"sequence"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return 0, fmt.Errorf("superfan: decode cursor: %w", err)
	}
	return doc.Sequence, nil
}

// Save atomically replaces the cursor with seq.
func (c *CursorStore) Save(seq uint64) error {
	data, err := json.Marshal(struct {
		Sequence uint64 `json:"sequence"`
	}{Sequence: seq})
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, ".cursor-*")
	if err != nil {
		return fmt.Errorf("superfan: save cursor: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("superfan: save cursor: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("superfan: save cursor: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("superfan: save cursor: %w", err)
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		return fmt.Errorf("superfan: save cursor: %w", err)
	}
	return nil
}

// readAllLimited reads at most limit bytes and fails when there are more.
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file too large")
	}
	return data, nil
}
