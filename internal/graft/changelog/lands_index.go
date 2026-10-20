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
	"sort"
	"sync"
	"sync/atomic"
)

const (
	// landsFileName is the name of the index of the land identities inside dirName.
	landsFileName = "changelog.lands"
	// maxIdleLandsIndexes bounds the number of indexes that are kept in memory while no lookup
	// uses them. A dropped index is loaded from its file again.
	maxIdleLandsIndexes = 64
	// maxIdleLandsBytes bounds the memory of the indexes that are kept while no lookup uses them,
	// as estimated by landsIndex.bytes. The least recently used ones beyond it are dropped, so an
	// index larger than the bound is dropped as soon as its lookup ends.
	maxIdleLandsBytes = 256 << 20
	// compactLandsMinSize is the size from which an index file is compacted once most of its
	// records are superseded by later ones.
	compactLandsMinSize = 1 << 20
	// compactIntentsMinSize is the size from which an intents file is compacted, see
	// compactIntents.
	compactIntentsMinSize = 64 << 10
)

// The index of the land identities of a repository (doc/graft/PLAN11.md, 4.2) maps a branch and an
// original commit to the last changelog entry that landed that commit on that branch. It is derived
// from the changelog: the log entries carry the lands (Entry.Lands), and the index is the result of
// reading them, kept in memory and in the file graft/changelog.lands so that a new process does not
// read the whole log again. A lookup also reads the land intents (see landIntent), which cover the
// lands whose update git committed before the changelog recorded it.
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
//
// In memory, the index also knows the log file it was built from: a log file that is another file
// (another inode) rebuilds it, and a lookup whose log ends with the entry of the mark, in the same
// file, reads nothing.

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

// describes reports whether head is the head of the log that the mark names the last entry of.
func (m landsMark) describes(head logHead) bool {
	return head.sequence == m.sequence && head.reference == m.reference && head.newOID == m.newOID
}

// landsIndex is the in-process view of the index of one repository.
type landsIndex struct {
	// mu serializes the lookups of this process in the repository and guards the fields below.
	mu sync.Mutex
	// users and lastUsed are guarded by landsIndexesMu: the callers that hold the index, and the
	// tick of its last release, which orders the eviction.
	users    int
	lastUsed uint64
	// bytes is the estimate of the memory of the index, see estimate. It is read by the eviction
	// without mu.
	bytes atomic.Int64

	// loaded is false until the index has been read from its file.
	loaded bool
	// mark is the log entry up to which records is complete; its sequence is zero when records
	// covers no entry.
	mark landsMark
	// logDevice and logInode identify the log file that records was read from.
	logDevice, logInode uint64
	// records maps the lands of the log entries up to mark to their last record.
	records map[landsIndexKey]indexRecord
	// confirmed holds the invocations of the land updates whose changelog entries the index read
	// (Entry.LandInvocation): their intents may go. An invocation leaves it with its intents.
	confirmed map[string]struct{}
	// recordLines is the number of record lines of the index file, superseded ones included.
	recordLines int
	// fileSize is the size of the index file when this process last read or wrote it. A file of
	// another size was written by another process and is read again.
	fileSize int64

	// intents are the land intents read from the intents file, by land, and intentsFile what is
	// known of that file.
	intents     map[landsIndexKey][]landIntent
	intentsFile intentsFile
}

// indexRecord is the last record of a land in an index.
type indexRecord struct {
	LandRecord
	// invocation identifies the update of the record, see Entry.LandInvocation.
	invocation string
}

// forget drops what the index knows, after a catch-up that failed: the next lookup reads the
// index and the intents again. The caller must hold i.mu.
func (i *landsIndex) forget() {
	i.loaded = false
	i.intents = nil
	i.intentsFile = intentsFile{}
}

var (
	landsIndexesMu sync.Mutex
	// landsIndexes holds the indexes by the path of their file. An index is kept after its last
	// user releases it, within maxIdleLandsIndexes and maxIdleLandsBytes.
	landsIndexes = map[string]*landsIndex{}
	// landsTick orders the releases of the indexes. It is guarded by landsIndexesMu.
	landsTick uint64
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
		landsTick++
		index.lastUsed = landsTick
		evictIdleLandsIndexes()
	}
}

// evictIdleLandsIndexes drops the least recently used idle indexes until at most
// maxIdleLandsIndexes of them, of at most maxIdleLandsBytes together, remain. The caller must hold
// landsIndexesMu.
func evictIdleLandsIndexes() {
	type idleIndex struct {
		path     string
		lastUsed uint64
		bytes    int64
	}

	var (
		idle  []idleIndex
		total int64
	)
	for path, index := range landsIndexes {
		if index.users == 0 {
			bytes := index.bytes.Load()
			idle = append(idle, idleIndex{path: path, lastUsed: index.lastUsed, bytes: bytes})
			total += bytes
		}
	}
	if len(idle) <= maxIdleLandsIndexes && total <= maxIdleLandsBytes {
		return
	}

	sort.Slice(idle, func(a, b int) bool { return idle[a].lastUsed < idle[b].lastUsed })
	for _, index := range idle {
		if len(idle) <= maxIdleLandsIndexes && total <= maxIdleLandsBytes {
			return
		}
		delete(landsIndexes, index.path)
		idle = idle[1:]
		total -= index.bytes
	}
}

// landsPath returns the path of the index of the land identities of the repository whose git
// directory is repoPath.
func landsPath(repoPath string) string {
	return filepath.Join(repoPath, dirName, landsFileName)
}

// LookupLand returns what the repository whose git directory is repoPath knows of the lands of
// original on reference (doc/graft/PLAN11.md): first the last land that the changelog recorded,
// with the sequence of its entry and the commit it landed as, then the land intents (see
// landIntent) of other landed commits, with a zero sequence. A land answers only when its landed
// commit is on the branch, which the caller checks: an intent whose commit is on the branch proves
// the land by itself, whatever the changelog recorded since, so no intent is left out but those of
// the recorded commit. The result is empty when nothing is known. The lookup reads the durable
// entries of the log (see AppendGroup) and the intents that the index does not cover yet, and adds
// them to the index first.
func LookupLand(repoPath, reference, original string) ([]LandRecord, error) {
	index, release := acquireLandsIndex(landsPath(repoPath))
	defer release()

	index.mu.Lock()
	defer index.mu.Unlock()

	if err := index.catchUp(repoPath); err != nil {
		// What the failed catch-up did to the index is not known, so the next lookup reads it again.
		index.forget()
		return nil, err
	}

	key := landsIndexKey{reference: reference, original: original}
	var lands []LandRecord
	recorded, found := index.records[key]
	if found {
		lands = append(lands, recorded.LandRecord)
	}
	seen := map[string]bool{recorded.Landed: found}
	for _, intent := range index.intents[key] {
		if seen[intent.Landed] {
			continue
		}
		seen[intent.Landed] = true
		lands = append(lands, LandRecord{Landed: intent.Landed})
	}

	return lands, nil
}

// catchUp brings the index up to the last durable entry of the log and the intents up to the end
// of their file, and compacts their files when they grew large. The caller must hold i.mu.
func (i *landsIndex) catchUp(repoPath string) error {
	if err := i.catchUpLog(repoPath); err != nil {
		return err
	}
	if err := i.catchUpIntents(repoPath); err != nil {
		return err
	}
	i.bytes.Store(i.estimate())

	return nil
}

// estimate returns an estimate of the memory of the index: its strings and the overhead of maps
// and slices. The caller must hold i.mu.
func (i *landsIndex) estimate() int64 {
	const overhead = 96
	var bytes int64
	for key, record := range i.records {
		bytes += int64(len(key.reference)+len(key.original)+len(record.Landed)+len(record.invocation)) + overhead
	}
	for invocation := range i.confirmed {
		bytes += int64(len(invocation)) + overhead
	}
	for key, intents := range i.intents {
		for _, intent := range intents {
			bytes += int64(len(key.reference)+len(key.original)+len(intent.Landed)+len(intent.Reference)) + overhead
		}
	}
	return bytes
}

// catchUpLog brings the index up to the last durable entry of the log. The caller must hold i.mu.
func (i *landsIndex) catchUpLog(repoPath string) error {
	head, err := readLogHead(repoPath)
	if err != nil {
		return err
	}
	if head.sequence == 0 {
		// A log without entries recorded no land. The index, in memory or in its file, is of
		// another log if it holds anything, and is rebuilt once this log has entries.
		i.reset(0)
		return nil
	}

	sameLog := i.loaded && head.device == i.logDevice && head.inode == i.logInode
	if sameLog && i.mark.describes(head) {
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

	block, err := landsBlock{}, errLandsIndexStale
	switch {
	case i.loaded && !sameLog:
		// The log is another file than the one the index was read from: the repository was
		// created again. The index is rebuilt.
	case !i.loaded || info.Size() != i.fileSize:
		if err := i.load(f); err != nil {
			return err
		}
		block, err = i.scanLog(repoPath)
	default:
		block, err = i.scanLog(repoPath)
	}
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

	if block.mark.sequence != i.mark.sequence {
		encoded, err := block.encode()
		if err != nil {
			return err
		}
		if _, err := f.WriteAt(encoded, i.fileSize); err != nil {
			return fmt.Errorf("writing land index: %w", err)
		}

		for _, record := range block.records {
			i.add(record)
		}
		i.recordLines += len(block.records)
		i.mark = block.mark
		i.fileSize += int64(len(encoded))
	}

	i.logDevice, i.logInode = head.device, head.inode
	i.loaded = true

	if i.fileSize >= compactLandsMinSize && i.recordLines > 2*len(i.records) {
		return i.compact(path)
	}

	return nil
}

// compact rewrites the index file with only the records that are not superseded, as one block,
// and replaces the old file with it. The caller must hold i.mu and the lock file of the index; the
// rename makes the other processes read the new file.
func (i *landsIndex) compact(path string) error {
	block := landsBlock{mark: i.mark}
	for key, record := range i.records {
		block.records = append(block.records, landsBlockRecord{sequence: record.Sequence, reference: key.reference, original: key.original, landed: record.Landed, invocation: record.invocation})
	}
	sort.Slice(block.records, func(a, b int) bool { return block.records[a].sequence < block.records[b].sequence })

	encoded, err := block.encode()
	if err != nil {
		return err
	}

	tmp := path + ".compact"
	if err := os.WriteFile(tmp, encoded, 0o644); err != nil {
		return fmt.Errorf("compacting land index: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("compacting land index: %w", err)
	}

	i.recordLines = len(block.records)
	i.fileSize = int64(len(encoded))

	return nil
}

// catchUpIntents reads the land intents recorded since the last lookup, and compacts the intents
// file when it grew large. The caller must hold i.mu.
func (i *landsIndex) catchUpIntents(repoPath string) error {
	path := intentsPath(repoPath)

	add := func(intent landIntent) {
		key := landsIndexKey{reference: string(intent.Reference), original: intent.Original}
		i.intents[key] = append(i.intents[key], intent)
	}

	if i.intents == nil {
		i.intents = map[landsIndexKey][]landIntent{}
	}
	known, reset, err := readIntents(path, i.intentsFile, add)
	if err != nil {
		return err
	}
	if reset {
		// The file is not the one that was read before, and the intents read before are not
		// its: they are dropped, and the file is read from its start.
		i.intents = map[landsIndexKey][]landIntent{}
		if known, _, err = readIntents(path, intentsFile{}, add); err != nil {
			return err
		}
	}
	i.intentsFile = known

	// An intent may go once the changelog entry of its own update is durable. The file is
	// compacted from compactIntentsMinSize on, when at least half of its intents may go, so that
	// intents that must stay are not rewritten again and again.
	confirmed := func(intent landIntent) bool {
		_, ok := i.confirmed[intent.Invocation]
		return intent.Invocation != "" && ok
	}
	total, droppable := 0, 0
	for _, intents := range i.intents {
		for _, intent := range intents {
			total++
			if confirmed(intent) {
				droppable++
			}
		}
	}
	if known.offset < compactIntentsMinSize || 2*droppable < total {
		return nil
	}

	if err := compactIntents(repoPath, path, confirmed); err != nil {
		return err
	}

	// The invocations of the dropped intents are not needed any more.
	for _, intents := range i.intents {
		for _, intent := range intents {
			if confirmed(intent) {
				delete(i.confirmed, intent.Invocation)
			}
		}
	}

	i.intents = map[landsIndexKey][]landIntent{}
	known, _, err = readIntents(path, intentsFile{}, add)
	if err != nil {
		return err
	}
	i.intentsFile = known

	return nil
}

// add records a land in the index: as the last record of its original commit on its branch, and
// its invocation as confirmed. The caller must hold i.mu.
func (i *landsIndex) add(record landsBlockRecord) {
	i.records[landsIndexKey{reference: record.reference, original: record.original}] = indexRecord{
		LandRecord: LandRecord{Sequence: record.sequence, Landed: record.landed},
		invocation: record.invocation,
	}
	if record.invocation != "" {
		i.confirmed[record.invocation] = struct{}{}
	}
}

// errLandsIndexStale is returned by scanLog when the log entry at the mark of the index is not the
// one the mark names.
var errLandsIndexStale = errors.New("the land index does not describe the log")

// landsBlockRecord is a record of a block, see landsBlock.
type landsBlockRecord struct {
	sequence   uint64
	reference  string
	original   string
	landed     string
	invocation string
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
				sequence:   entry.Sequence,
				reference:  entry.Reference,
				original:   land.Original,
				landed:     land.Landed,
				invocation: entry.LandInvocation,
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
	// Invocation identifies the update of the record, see Entry.LandInvocation.
	Invocation string `json:"inv,omitempty"`
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
			Sequence:   record.sequence,
			Reference:  []byte(record.reference),
			Original:   record.original,
			Landed:     record.landed,
			Invocation: record.invocation,
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
	i.records = map[landsIndexKey]indexRecord{}
	i.confirmed = map[string]struct{}{}
	i.recordLines = 0
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
				sequence:   decoded.Sequence,
				reference:  string(decoded.Reference),
				original:   decoded.Original,
				landed:     decoded.Landed,
				invocation: decoded.Invocation,
			})
			crc.Write(line)
			continue
		}

		if decoded.Mark <= i.mark.sequence || decoded.CRC != crc.Sum32() ||
			(len(pending) > 0 && pending[len(pending)-1].sequence > decoded.Mark) {
			break
		}

		for _, record := range pending {
			i.add(record)
		}
		i.recordLines += len(pending)
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
