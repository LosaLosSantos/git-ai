package verify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// stateVersion is the version of the state file format.
const stateVersion = 1

// State is the persisted progress of the verifier.
type State struct {
	Version int `json:"version"`
	// Cursor is the sequence number of the last changeset covered by a
	// completed verification cycle. Changesets after it are still to do.
	Cursor uint64 `json:"cursor"`
}

// loadState reads the state file at path. A missing file is the initial
// state, with cursor zero.
func loadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{Version: stateVersion}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read state file: %w", err)
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("parse state file %s: %w", path, err)
	}
	if st.Version != stateVersion {
		return State{}, fmt.Errorf("state file %s has version %d, want %d", path, st.Version, stateVersion)
	}
	return st, nil
}

// saveState writes st to path atomically: the content goes to a temporary
// file in the same directory, which then replaces path. A crash leaves either
// the old or the new state, never a partial one.
func saveState(path string, st State) error {
	st.Version = stateVersion
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}
