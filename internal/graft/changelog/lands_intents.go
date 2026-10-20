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
// prepared phase ends. Unlike the index it is not derived data. An intent carries the invocation
// of its update, and so does the changelog entry that the update writes once it is committed
// (Entry.LandInvocation): a compaction drops an intent once the entry of its own update is
// durable, and only then. The intent of an update that was never committed stays.

// landIntent is one line of the intents file.
type landIntent struct {
	// Reference is the branch, as bytes (base64 in JSON): branch names may hold bytes that are
	// not valid UTF-8.
	Reference []byte `json:"ref"`
	Original  string `json:"original"`
	Landed    string `json:"landed"`
	// Invocation identifies the update that recorded the intent, see Entry.LandInvocation.
	Invocation string `json:"invocation"`
}

// intentsPath returns the path of the intents file of the repository whose git directory is
// repoPath.
func intentsPath(repoPath string) string {
	return filepath.Join(repoPath, dirName, intentsFileName)
}

// intentsFileKey identifies an intents file: its path, and the file at that path.
type intentsFileKey struct {
	path          string
	device, inode uint64
}

var (
	// intentDirsSynced holds the intents files of this process whose directory entries are
	// durable, see RecordLandIntents. A file created again at the same path, as with a
	// repository created again, is another file.
	intentDirsSynced sync.Map
	// intentsCompactionSize holds, by path, the size of the intents file when RecordLandIntents
	// last asked for a compaction, see compactionDue.
	intentsCompactionSize sync.Map
)

// RecordLandIntents records durably, in the repository whose git directory is repoPath, that the
// update of reference about to be committed lands lands, as the update identified by invocation
// (see the comment above landIntent). It returns once the intents are fsync'd; an error must abort
// the update, which would otherwise be committed without its identity. compact reports that the
// file grew so that its compaction is due: the caller runs CompactLandIntents, outside the locks
// of the update.
func RecordLandIntents(repoPath, reference, invocation string, lands []Land) (compact bool, err error) {
	path := intentsPath(repoPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("creating changelog directory: %w", err)
	}

	unlock, err := lockFile(path + lockSuffix)
	if err != nil {
		return false, err
	}
	defer unlock()

	_, statErr := os.Stat(path)
	created := errors.Is(statErr, fs.ErrNotExist)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, fmt.Errorf("opening land intents: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat land intents: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("stat land intents: no file identity in %T", info.Sys())
	}

	// The bytes after the last newline are a torn line of a writer that crashed before its fsync,
	// whose update was therefore never committed. They are cut off, or the next line would be
	// joined to them.
	end, err := endOfLastLine(f)
	if err != nil {
		return false, err
	}

	var buf bytes.Buffer
	for _, land := range lands {
		line, err := json.Marshal(landIntent{Reference: []byte(reference), Original: land.Original, Landed: land.Landed, Invocation: invocation})
		if err != nil {
			return false, fmt.Errorf("encoding land intent: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	if err := truncateFile(f, end); err != nil {
		return false, fmt.Errorf("truncating land intents: %w", err)
	}
	if _, err := f.WriteAt(buf.Bytes(), end); err != nil {
		return false, fmt.Errorf("writing land intents: %w", err)
	}
	if err := syncFile(f); err != nil {
		return false, fmt.Errorf("syncing land intents: %w", err)
	}

	// The intents are durable only once the directory entries of their file are: when this write
	// created the file, and on the first write of this process to the file, which a writer that
	// created it may have crashed before syncing.
	key := intentsFileKey{path: path, device: uint64(stat.Dev), inode: stat.Ino}
	if _, synced := intentDirsSynced.Load(key); created || !synced {
		if err := syncLogDirs(repoPath, path); err != nil {
			return false, err
		}
		intentDirsSynced.Store(key, struct{}{})
	}

	return compactionDue(path, end+int64(buf.Len())), nil
}

// compactionDue reports whether an intents file of the given size is due for a compaction: from
// compactIntentsMinSize on, each time it doubled since the last time it was due, or since the last
// CompactLandIntents left it, in this process.
func compactionDue(path string, size int64) bool {
	if size < compactIntentsMinSize {
		return false
	}

	last, _ := intentsCompactionSize.Load(path)
	if lastSize, ok := last.(int64); ok && size < 2*lastSize {
		return false
	}

	intentsCompactionSize.Store(path, size)
	return true
}

// compactionsRunning holds the repositories whose intents CompactLandIntents compacts right now.
var compactionsRunning sync.Map

// CompactLandIntents brings the land index of the repository whose git directory is repoPath up to
// its changelog and intents, which compacts the intents file when enough of its intents are
// confirmed (see landsIndex.catchUpIntents). RecordLandIntents asks for it, so that the intents of
// a repository whose lands never look up an earlier land (fast-forwards only) are compacted too. A
// call while another one runs for the repository returns at once.
func CompactLandIntents(repoPath string) error {
	if _, running := compactionsRunning.LoadOrStore(repoPath, struct{}{}); running {
		return nil
	}
	defer compactionsRunning.Delete(repoPath)

	index, release := acquireLandsIndex(landsPath(repoPath))
	defer release()

	index.mu.Lock()
	defer index.mu.Unlock()

	if err := index.catchUp(repoPath); err != nil {
		index.forget()
		return err
	}

	// The next compaction is due once the file doubled from what this one left.
	intentsCompactionSize.Store(intentsPath(repoPath), index.intentsFile.offset)

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
// each intent, and returns what the reader knows of the file after the read. A reader that knows
// nothing of the file (the zero known) reads it from its start. When the file is not the one known
// (it was replaced) or is shorter (it was replaced or cut), nothing is read and reset is true: the
// intents read before do not describe it, and the caller drops them and reads it again knowing
// nothing. A missing file has no intents.
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
	if known != (intentsFile{}) && (current.device != known.device || current.inode != known.inode || info.Size() < known.offset) {
		return intentsFile{}, true, nil
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

	return current, false, nil
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
