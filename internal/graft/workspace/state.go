package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	stateFileName = "state.json"
	lockFileName  = "lock"
	stateVersion  = 1
)

// ErrLocked is returned when another graft process holds the workspace lock.
var ErrLocked = errors.New("workspace is locked by another graft process")

// lockWait is how long a command waits for another command of the same
// workspace to finish before it fails with ErrLocked.
var lockWait = 10 * time.Second

// lockPoll is the interval at which a waiting command retries the lock.
const lockPoll = 20 * time.Millisecond

// lock takes the workspace lock, waiting up to lockWait for a running command
// to release it, so that concurrent commands never lose updates to the state
// file. The lock is an advisory operating system lock on a file in the
// metadata directory. The system releases it when the process exits, however
// it exits, so an interrupted command never leaves a stale lock behind. The
// returned function releases it.
func lock(root string) (func(), error) {
	return lockWithin(root, lockWait)
}

// lockWithin is lock with an explicit maximum wait.
func lockWithin(root string, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Join(root, metaDirName), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath(root), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		locked, err := tryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock workspace: %w", err)
		}
		if locked {
			break
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("%w: another graft command is running in %s", ErrLocked, root)
		}
		time.Sleep(lockPoll)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

// FileState is what the workspace last wrote to disk for a tracked file. The
// size and mtime allow status to skip re-hashing unchanged files.
type FileState struct {
	OID     string `json:"oid"`
	Mode    uint32 `json:"mode"`
	Size    int64  `json:"size"`
	MTimeNS int64  `json:"mtime_ns"`
}

// Pending is a commit that was created on a temporary branch but not landed.
type Pending struct {
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	// Before is the tracking state that the pending commit replaced, for each
	// tracked path it changed. Abort restores it.
	Before map[string]FileState `json:"before,omitempty"`
	// Added lists the paths that the pending commit started tracking. Abort stops
	// tracking them.
	Added []string `json:"added,omitempty"`
	// Landed is the commit that the pending commit landed as, recorded as soon
	// as the server accepted the land. It is set while the workspace refresh
	// after the land has not succeeded yet. The commit then cannot be aborted,
	// and a retried land only refreshes the workspace.
	Landed string `json:"landed,omitempty"`
}

// State is the content of .graft/state.json.
type State struct {
	Version      int                  `json:"version"`
	Address      string               `json:"address,omitempty"`
	Storage      string               `json:"storage"`
	Repo         string               `json:"repo"`
	Branch       string               `json:"branch"`
	Base         string               `json:"base"`
	ObjectFormat string               `json:"object_format"`
	Sparse       []string             `json:"sparse"`
	Files        map[string]FileState `json:"files"`
	Pending      *Pending             `json:"pending,omitempty"`
	// SavedNS is the time, in Unix nanoseconds, at which the state was last
	// written. Files modified at or after that time, within racyWindow, cannot
	// be told apart by size and mtime alone and are always re-hashed.
	SavedNS int64 `json:"saved_ns,omitempty"`
}

// racyWindow is the margin around the state's save time within which a file's
// mtime is not trusted. It covers coarse file system timestamps.
const racyWindow = 2 * time.Second

// Meta is the remote identity recorded when the workspace was cloned.
type Meta struct {
	Address string
	Storage string
	Repo    string
	Branch  string
}

// Summary describes the current state of a workspace.
type Summary struct {
	Base    string
	Branch  string
	Files   int
	Sparse  []string
	Pending *Pending
}

func (s State) sha256Format() bool { return s.ObjectFormat == "sha256" }

func statePath(root string) string {
	return filepath.Join(root, metaDirName, stateFileName)
}

// lockPath returns the lock file that serializes the commands of one workspace.
func lockPath(root string) string {
	return filepath.Join(root, metaDirName, lockFileName)
}

// ReadMeta reads the remote identity of the workspace at root.
func ReadMeta(root string) (Meta, error) {
	st, err := loadState(root)
	if err != nil {
		return Meta{}, err
	}
	return Meta{Address: st.Address, Storage: st.Storage, Repo: st.Repo, Branch: st.Branch}, nil
}

func loadState(root string) (State, error) {
	data, err := os.ReadFile(statePath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, ErrNotWorkspace
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("parse %s: %w", statePath(root), err)
	}
	if st.Files == nil {
		st.Files = map[string]FileState{}
	}
	for p := range st.Files {
		if _, err := ValidatePath(p); err != nil {
			return State{}, fmt.Errorf("state: tracked file: %w", err)
		}
	}
	for _, p := range st.Sparse {
		if _, err := ValidatePath(p); err != nil {
			return State{}, fmt.Errorf("state: sparse prefix: %w", err)
		}
	}
	return st, nil
}

// save writes the state atomically and records the save time.
func (s *State) save(root string) error {
	dir := filepath.Join(root, metaDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s.SavedNS = time.Now().UnixNano()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(dir, statePath(root), append(data, '\n'), 0o600)
}

// objectPath returns the cache location of a blob. Only valid object IDs map
// to a path, so the cache cannot be used to address files outside its tree.
func objectPath(root, oid string) (string, error) {
	if !validOID(oid) {
		return "", fmt.Errorf("invalid object ID %q", oid)
	}
	return filepath.Join(root, metaDirName, "objects", oid[:2], oid[2:]), nil
}

// readCachedObject returns a cached blob or ok=false when it is not cached.
func readCachedObject(root, oid string) (data []byte, ok bool, err error) {
	path, err := objectPath(root, oid)
	if err != nil {
		return nil, false, err
	}
	data, err = os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func writeCachedObject(root, oid string, data []byte) error {
	path, err := objectPath(root, oid)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Dir(path), path, data, 0o644)
}

// writeFileAtomic writes data to a temporary file in tmpDir and renames it to
// path, so readers never observe a partial file. tmpDir must be on the same file
// system as path. The temporary file is removed when a step fails.
func writeFileAtomic(tmpDir, path string, data []byte, perm fs.FileMode) error {
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, ".graft-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
