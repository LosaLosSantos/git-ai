package changelog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// intentsFileName is the name of the file of the land intents inside dirName.
const intentsFileName = "changelog.intents"

// The land intents of a repository (doc/graft/PLAN11.md, 4.2) close the window between git
// committing the update of a land and the changelog recording it. The changelog entry of an update,
// which carries its lands, is written in the committed phase of the reference transaction, after
// git committed the references: a crash, a full disk or a lock that times out in between leaves the
// branch with the land and the changelog without its identity. So, in the prepared phase, before
// git may commit, the hook records the lands of the update as intents and fsyncs them. A lookup
// answers from the confirmed records of the index and from the intents; the land path counts an
// intent only when its landed commit is on the branch, so the intent of an update that was not
// committed answers nothing.
//
// The file is JSON lines, appended under the lock file of the intents and fsync'd before the
// prepared phase ends. Unlike the index it is not derived data. An intent is dropped by a
// compaction once the changelog confirms it (a record of its land with its landed commit) or
// supersedes it (a later record of the same original commit on the branch); the intent of an
// update that was never committed and never landed again stays.

// landIntent is one line of the intents file.
type landIntent struct {
	// Reference is the branch, as bytes (base64 in JSON): branch names may hold bytes that are
	// not valid UTF-8.
	Reference []byte `json:"ref"`
	Original  string `json:"original"`
	Landed    string `json:"landed"`
	// Head is the sequence of the last durable changelog entry when the intent was recorded. The
	// entry of the land, if any, comes after it, so a record of the same commit at a later
	// sequence confirms or supersedes the intent.
	Head uint64 `json:"head"`
}

// intentsPath returns the path of the intents file of the repository whose git directory is
// repoPath.
func intentsPath(repoPath string) string {
	return filepath.Join(repoPath, dirName, intentsFileName)
}

// intentDirsSynced holds the intents files of this process whose directory entries are durable,
// see RecordLandIntents.
var intentDirsSynced sync.Map

// RecordLandIntents records durably, in the repository whose git directory is repoPath, that the
// update of reference about to be committed lands lands (see the comment above landIntent). It
// returns once the intents are fsync'd; an error must abort the update, which would otherwise be
// committed without its identity.
func RecordLandIntents(repoPath, reference string, lands []Land) error {
	head, err := readLogHead(repoPath)
	if err != nil {
		return err
	}

	path := intentsPath(repoPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating changelog directory: %w", err)
	}

	unlock, err := lockFile(path + lockSuffix)
	if err != nil {
		return err
	}
	defer unlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("opening land intents: %w", err)
	}
	defer f.Close()

	// The bytes after the last newline are a torn line of a writer that crashed before its fsync,
	// whose update was therefore never committed. They are cut off, or the next line would be
	// joined to them.
	end, err := endOfLastLine(f)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	for _, land := range lands {
		line, err := json.Marshal(landIntent{Reference: []byte(reference), Original: land.Original, Landed: land.Landed, Head: head.sequence})
		if err != nil {
			return fmt.Errorf("encoding land intent: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	if err := truncateFile(f, end); err != nil {
		return fmt.Errorf("truncating land intents: %w", err)
	}
	if _, err := f.WriteAt(buf.Bytes(), end); err != nil {
		return fmt.Errorf("writing land intents: %w", err)
	}
	if err := syncFile(f); err != nil {
		return fmt.Errorf("syncing land intents: %w", err)
	}

	// The intents are durable only once the directory entries of their file are, which a writer
	// that created the file may have crashed before syncing: the first write of this process
	// syncs them.
	if _, synced := intentDirsSynced.Load(path); !synced {
		if err := syncLogDirs(repoPath, path); err != nil {
			return err
		}
		intentDirsSynced.Store(path, struct{}{})
	}

	return nil
}

// endOfLastLine returns the offset just after the last newline of f, 0 when it has none.
func endOfLastLine(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat land intents: %w", err)
	}

	nl, err := lastNewlineBefore(f, info.Size())
	if err != nil {
		return 0, err
	}

	return nl + 1, nil
}

// intentsFile is what a reader of an intents file knows of it.
type intentsFile struct {
	// device and inode identify the file. A compaction replaces it.
	device, inode uint64
	// offset is the end of the complete lines read so far.
	offset int64
}

// readIntents reads the complete lines of the intents file at path after known, calling add for
// each intent. It returns what the reader knows of the file after the read. When the file is not
// the one known (it was replaced) or is shorter (it was replaced or cut), it is read from the
// start and reset is true: the intents read before do not describe it any more. A missing file has
// no intents.
func readIntents(path string, known intentsFile, add func(landIntent)) (intentsFile, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return intentsFile{}, known != (intentsFile{}), nil
		}
		return known, false, fmt.Errorf("opening land intents: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return known, false, fmt.Errorf("stat land intents: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return known, false, fmt.Errorf("stat land intents: no file identity in %T", info.Sys())
	}

	current := intentsFile{device: uint64(stat.Dev), inode: stat.Ino, offset: known.offset}
	reset := current.device != known.device || current.inode != known.inode || info.Size() < known.offset
	if reset {
		current.offset = 0
	}

	r := bufio.NewReader(io.NewSectionReader(f, current.offset, info.Size()-current.offset))
	for {
		line, err := r.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return known, false, fmt.Errorf("reading land intents: %w", err)
		}
		// A line without its newline is still being written, or is the torn line of a writer that
		// crashed: it is read once it is complete, or cut off by the next writer.
		if len(line) == 0 || line[len(line)-1] != '\n' {
			break
		}
		current.offset += int64(len(line))

		// A line that does not decode is not an intent. Only a writer that crashed before its
		// fsync leaves one, and its update was not committed.
		var intent landIntent
		if json.Unmarshal(line, &intent) != nil || !isObjectID(intent.Original) || !isObjectID(intent.Landed) {
			continue
		}
		add(intent)
	}

	return current, reset, nil
}

// compactIntents rewrites the intents file at path without the intents that drop says may go,
// under the lock file of the intents, so that no intent recorded meanwhile is lost: the file is
// read whole under the lock. The new file is fsync'd and renamed over the old one, and the
// directory is fsync'd, before the lock is released.
func compactIntents(repoPath, path string, drop func(landIntent) bool) error {
	unlock, err := lockFile(path + lockSuffix)
	if err != nil {
		return err
	}
	defer unlock()

	var (
		kept      bytes.Buffer
		encodeErr error
	)
	if _, _, err := readIntents(path, intentsFile{}, func(intent landIntent) {
		if drop(intent) || encodeErr != nil {
			return
		}
		line, err := json.Marshal(intent)
		if err != nil {
			encodeErr = fmt.Errorf("encoding land intent: %w", err)
			return
		}
		kept.Write(line)
		kept.WriteByte('\n')
	}); err != nil {
		return err
	}
	if encodeErr != nil {
		return encodeErr
	}

	tmp := path + ".compact"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("compacting land intents: %w", err)
	}
	if _, err := f.Write(kept.Bytes()); err != nil {
		f.Close()
		return fmt.Errorf("compacting land intents: %w", err)
	}
	if err := syncFile(f); err != nil {
		f.Close()
		return fmt.Errorf("compacting land intents: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("compacting land intents: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("compacting land intents: %w", err)
	}

	return syncLogDirs(repoPath, path)
}
