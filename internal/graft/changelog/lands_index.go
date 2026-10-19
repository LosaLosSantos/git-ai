package changelog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	// landsFileName is the name of the index of the land identities inside dirName.
	landsFileName = "changelog.lands"
	// maxIdleLandsIndexes bounds the number of indexes that are kept in memory while no lookup
	// uses them. An index holds every land of its repository, so fewer of them are kept than of
	// the states of the logs (maxIdleStates). A dropped index is loaded from its file again.
	maxIdleLandsIndexes = 64
)

// The index of the land identities of a repository (doc/graft/PLAN11.md, 4.2) maps a branch and an
// original commit to the last changelog entry that landed that commit on that branch. It is derived
// from the changelog: the log entries carry the lands (Entry.Lands), and the index is the result of
// reading them, kept in memory and in the file graft/changelog.lands so that a new process does not
// read the whole log again.
//
// The file is a sequence of blocks of JSON lines. A block holds the records of the lands of some
// log entries, in log order, and ends with a mark: the sequence of the last log entry the block
// covers, the reference and new value of that entry, and the CRC-32 of the record lines of the
// block. The index is complete up to the sequence of the last mark.
//
// The file is never trusted beyond what it can prove. Reading it stops at the first line that is
// not a record or a mark, at a mark whose checksum does not match its records, and at a mark that
// does not follow the one before; the file is cut there, and the lookup reads the log from the last
// good mark on, as it does after the entries that other appends added since. The mark proves that
// it describes this log: the lookup reads the log entry at the mark's sequence first, and when that
// entry is not the one the mark names (the log was replaced, for example by a repository created
// again at the same path) the index is rebuilt from the whole log. The file is therefore written
// without fsync: what a crash leaves of it is either a good prefix or is cut off.

// landsIndexKey identifies a land in an index.
type landsIndexKey struct {
	reference string
	original  string
}

// landsMark identifies the log entry up to which an index is complete.
type landsMark struct {
	sequence  uint64
	reference string
	newOID    string
}

// matches reports whether entry is the log entry that the mark names.
func (m landsMark) matches(entry Entry) bool {
	return entry.Sequence == m.sequence && entry.Reference == m.reference && entry.NewOID == m.newOID
}

// landsIndex is the in-process view of the index of one repository.
type landsIndex struct {
	// mu serializes the lookups of this process in the repository and guards the fields below.
	mu sync.Mutex
	// users counts the callers that hold the index. It is guarded by landsIndexesMu.
	users int
	// loaded is false until the index has been read from its file.
	loaded bool
	// mark is the log entry up to which records is complete; its sequence is zero when records
	// covers no entry.
	mark landsMark
	// records maps the lands of the log entries up to mark to their last record.
	records map[landsIndexKey]LandRecord
	// fileSize is the size of the index file when this process last read or wrote it. A file of
	// another size was written by another process and is read again.
	fileSize int64
}

var (
	landsIndexesMu sync.Mutex
	// landsIndexes holds the indexes by the path of their file. An index is kept after its last
	// user releases it, up to maxIdleLandsIndexes idle ones.
	landsIndexes = map[string]*landsIndex{}
)

// acquireLandsIndex returns the index whose file is path, and the function that releases it.
func acquireLandsIndex(path string) (*landsIndex, func()) {
	landsIndexesMu.Lock()
	index, ok := landsIndexes[path]
	if !ok {
		index = &landsIndex{}
		landsIndexes[path] = index
	}
	index.users++
	landsIndexesMu.Unlock()

	return index, func() {
		landsIndexesMu.Lock()
		defer landsIndexesMu.Unlock()

		index.users--
		evictIdleLandsIndexes()
	}
}

// evictIdleLandsIndexes drops idle indexes until at most maxIdleLandsIndexes remain idle. The caller
// must hold landsIndexesMu.
func evictIdleLandsIndexes() {
	if len(landsIndexes) <= maxIdleLandsIndexes {
		return
	}

	idle := 0
	for _, index := range landsIndexes {
		if index.users == 0 {
			idle++
		}
	}

	for path, index := range landsIndexes {
		if idle <= maxIdleLandsIndexes {
			return
		}
		if index.users == 0 {
			delete(landsIndexes, path)
			idle--
		}
	}
}

// landsPath returns the path of the index of the land identities of the repository whose git
// directory is repoPath.
func landsPath(repoPath string) string {
	return filepath.Join(repoPath, dirName, landsFileName)
}

// LookupLand returns the last land of original on reference that the changelog of the repository
// whose git directory is repoPath recorded (doc/graft/PLAN11.md): the sequence of its entry and the
// commit it landed as. found is false when the changelog recorded no land of original on reference.
// The lookup reads the durable entries of the log (see AppendGroup) that the index does not cover
// yet, and adds them to the index first.
func LookupLand(repoPath, reference, original string) (record LandRecord, found bool, err error) {
	index, release := acquireLandsIndex(landsPath(repoPath))
	defer release()

	index.mu.Lock()
	defer index.mu.Unlock()

	if err := index.catchUp(repoPath); err != nil {
		// What the failed catch-up did to the index is not known, so the next lookup reads it again.
		index.loaded = false
		return LandRecord{}, false, err
	}

	record, found = index.records[landsIndexKey{reference: reference, original: original}]
	return record, found, nil
}

// catchUp brings the index up to the last durable entry of the log. The caller must hold i.mu.
func (i *landsIndex) catchUp(repoPath string) error {
	last, err := LastSequence(repoPath)
	if err != nil {
		return err
	}
	if i.loaded && last == i.mark.sequence {
		return nil
	}

	path := landsPath(repoPath)
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
		return fmt.Errorf("opening land index: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat land index: %w", err)
	}
	if !i.loaded || info.Size() != i.fileSize {
		if err := i.load(f); err != nil {
			return err
		}
	}

	block, err := i.scanLog(repoPath)
	if errors.Is(err, errLandsIndexStale) {
		// The log is not the one the index was built from: build it again from the whole log.
		if err := truncateFile(f, 0); err != nil {
			return fmt.Errorf("truncating land index: %w", err)
		}
		i.reset(0)
		block, err = i.scanLog(repoPath)
	}
	if err != nil {
		return err
	}

	if block.mark.sequence == i.mark.sequence {
		i.loaded = true
		return nil
	}

	encoded, err := block.encode()
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(encoded, i.fileSize); err != nil {
		return fmt.Errorf("writing land index: %w", err)
	}

	for _, record := range block.records {
		i.records[landsIndexKey{reference: record.reference, original: record.original}] = LandRecord{Sequence: record.sequence, Landed: record.landed}
	}
	i.mark = block.mark
	i.fileSize += int64(len(encoded))
	i.loaded = true

	return nil
}

// errLandsIndexStale is returned by scanLog when the log entry at the mark of the index is not the
// one the mark names.
var errLandsIndexStale = errors.New("the land index does not describe the log")

// landsBlockRecord is a record of a block, see landsBlock.
type landsBlockRecord struct {
	sequence  uint64
	reference string
	original  string
	landed    string
}

// landsBlock is the records of the lands of the log entries after the mark of an index, and the
// mark of the last of those entries.
type landsBlock struct {
	records []landsBlockRecord
	mark    landsMark
}

// scanLog reads the log entries after the mark of the index and returns their lands as a block. It
// first checks that the log entry at the mark is the one the mark names, and returns
// errLandsIndexStale when it is not. The caller must hold i.mu.
func (i *landsIndex) scanLog(repoPath string) (landsBlock, error) {
	block := landsBlock{mark: i.mark}

	after := i.mark.sequence
	verified := after == 0
	if !verified {
		after--
	}

	if err := Scan(repoPath, after, func(entry Entry) (bool, error) {
		if !verified {
			if !i.mark.matches(entry) {
				return false, errLandsIndexStale
			}
			verified = true
			return true, nil
		}

		for _, land := range entry.Lands {
			block.records = append(block.records, landsBlockRecord{
				sequence:  entry.Sequence,
				reference: entry.Reference,
				original:  land.Original,
				landed:    land.Landed,
			})
		}
		block.mark = landsMark{sequence: entry.Sequence, reference: entry.Reference, newOID: entry.NewOID}

		return true, nil
	}); err != nil {
		return landsBlock{}, err
	}

	// The log ends before the mark: it was cut or replaced.
	if !verified {
		return landsBlock{}, errLandsIndexStale
	}

	return block, nil
}

// landsLine is one line of the index file: a record when Mark is zero, a mark otherwise. Reference
// names may hold bytes that are not valid UTF-8, so they are encoded as bytes (base64 in JSON).
type landsLine struct {
	Sequence  uint64 `json:"seq,omitempty"`
	Reference []byte `json:"ref"`
	Original  string `json:"original,omitempty"`
	Landed    string `json:"landed,omitempty"`
	// Mark is the sequence of the log entry that the mark names; Reference and NewOID are its
	// reference and new value, CRC the CRC-32 (IEEE) of the record lines of the block.
	Mark   uint64 `json:"mark,omitempty"`
	NewOID string `json:"new_oid,omitempty"`
	CRC    uint32 `json:"crc,omitempty"`
}

// encode returns the lines of the block, its records and then its mark.
func (b landsBlock) encode() ([]byte, error) {
	var buf bytes.Buffer
	for _, record := range b.records {
		line, err := json.Marshal(landsLine{
			Sequence:  record.sequence,
			Reference: []byte(record.reference),
			Original:  record.original,
			Landed:    record.landed,
		})
		if err != nil {
			return nil, fmt.Errorf("encoding land index record: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	mark, err := json.Marshal(landsLine{
		Reference: []byte(b.mark.reference),
		Mark:      b.mark.sequence,
		NewOID:    b.mark.newOID,
		CRC:       crc32.ChecksumIEEE(buf.Bytes()),
	})
	if err != nil {
		return nil, fmt.Errorf("encoding land index mark: %w", err)
	}
	buf.Write(mark)
	buf.WriteByte('\n')

	return buf.Bytes(), nil
}

// reset empties the in-memory index of a file of the given size.
func (i *landsIndex) reset(fileSize int64) {
	i.mark = landsMark{}
	i.records = map[landsIndexKey]LandRecord{}
	i.fileSize = fileSize
	i.loaded = false
}

// load reads the index from its file f up to its last good mark, and cuts the file there. The
// caller must hold i.mu and the lock file of the index.
func (i *landsIndex) load(f *os.File) error {
	i.reset(0)

	var (
		pending []landsBlockRecord
		crc     = crc32.NewIEEE()
		offset  int64
	)

	r := bufio.NewReader(f)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("reading land index: %w", err)
	}

	for {
		line, err := r.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("reading land index: %w", err)
		}
		// A line without its newline is a torn tail.
		if len(line) == 0 || line[len(line)-1] != '\n' {
			break
		}
		offset += int64(len(line))

		var decoded landsLine
		if json.Unmarshal(line, &decoded) != nil {
			break
		}

		if decoded.Mark == 0 {
			if decoded.Sequence <= i.mark.sequence || (len(pending) > 0 && decoded.Sequence < pending[len(pending)-1].sequence) {
				break
			}
			pending = append(pending, landsBlockRecord{
				sequence:  decoded.Sequence,
				reference: string(decoded.Reference),
				original:  decoded.Original,
				landed:    decoded.Landed,
			})
			crc.Write(line)
			continue
		}

		if decoded.Mark <= i.mark.sequence || decoded.CRC != crc.Sum32() ||
			(len(pending) > 0 && pending[len(pending)-1].sequence > decoded.Mark) {
			break
		}

		for _, record := range pending {
			i.records[landsIndexKey{reference: record.reference, original: record.original}] = LandRecord{Sequence: record.sequence, Landed: record.landed}
		}
		i.mark = landsMark{sequence: decoded.Mark, reference: string(decoded.Reference), newOID: decoded.NewOID}
		i.fileSize = offset
		pending = nil
		crc.Reset()
	}

	if err := truncateFile(f, i.fileSize); err != nil {
		return fmt.Errorf("truncating land index: %w", err)
	}

	return nil
}
