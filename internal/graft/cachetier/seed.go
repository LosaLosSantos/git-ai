package cachetier

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// seedHeader is the first line of a seed file. The whole file is gzip-compressed,
// and the header is the first line of the decompressed stream.
const seedHeader = "graft-cache-seed v1\n"

// errMalformedSeedRecord reports a record whose body does not decode.
var errMalformedSeedRecord = errors.New("malformed seed record")

// SeedReport summarizes what seeding did with one seed file.
type SeedReport struct {
	// Objects is the number of verified objects loaded into the cache.
	Objects int64 `json:"objects"`
	// Bytes is the data size of the loaded objects.
	Bytes int64 `json:"bytes"`
	// Corrupt counts the records that were skipped because they were malformed
	// or their content does not hash to their object ID. A damaged frame or a
	// truncated file ends the seed, because the start of the next record is
	// then unknown, and it is counted once.
	Corrupt int64 `json:"corrupt"`
	// Skipped counts the intact records that were not loaded, because the object
	// is larger than the in-memory budget and so could not be cached.
	Skipped int64 `json:"skipped"`
}

// SnapshotReport summarizes what a snapshot wrote.
type SnapshotReport struct {
	// Objects is the number of trees and blobs written.
	Objects int64 `json:"objects"`
	// Bytes is the data size of the written objects.
	Bytes int64 `json:"bytes"`
	// Missing counts the listed objects that the upstream reported as missing.
	// They are not written.
	Missing int64 `json:"missing"`
}

// seedRecord is one object of a seed file.
type seedRecord struct {
	storage      string
	relativePath string
	oid          string
	typ          gitalypb.ObjectType
	data         []byte
}

// seedWriter writes a seed file: the gzip-compressed header line followed by one
// length-prefixed record per object.
type seedWriter struct {
	gz  *gzip.Writer
	hdr []byte
}

func newSeedWriter(w io.Writer) (*seedWriter, error) {
	gz := gzip.NewWriter(w)
	if _, err := gz.Write([]byte(seedHeader)); err != nil {
		return nil, fmt.Errorf("write seed header: %w", err)
	}
	return &seedWriter{gz: gz}, nil
}

// write appends rec to the seed. The record's object must already be verified.
func (w *seedWriter) write(rec seedRecord) error {
	if !validOID(rec.oid) {
		return fmt.Errorf("seed record: invalid object ID %q", rec.oid)
	}
	if _, ok := typeName(rec.typ); !ok {
		return fmt.Errorf("seed record %s: unsupported type %v", rec.oid, rec.typ)
	}

	hdr := w.hdr[:0]
	hdr = binary.AppendUvarint(hdr, uint64(len(rec.storage)))
	hdr = append(hdr, rec.storage...)
	hdr = binary.AppendUvarint(hdr, uint64(len(rec.relativePath)))
	hdr = append(hdr, rec.relativePath...)
	hdr = binary.AppendUvarint(hdr, uint64(len(rec.oid)))
	hdr = append(hdr, rec.oid...)
	hdr = append(hdr, byte(rec.typ))
	hdr = binary.AppendUvarint(hdr, uint64(len(rec.data)))
	w.hdr = hdr

	frame := binary.AppendUvarint(nil, uint64(len(hdr)+len(rec.data)))
	for _, part := range [][]byte{frame, hdr, rec.data} {
		if _, err := w.gz.Write(part); err != nil {
			return fmt.Errorf("write seed record %s: %w", rec.oid, err)
		}
	}
	return nil
}

// close flushes the compressed stream. It does not close the underlying writer.
func (w *seedWriter) close() error {
	return w.gz.Close()
}

// maxSeedField bounds the storage name, relative path and object ID of a seed
// record. Longer fields only occur in damaged records.
const maxSeedField = 4096

// errSeedFrameOverrun reports a field that reaches past the end of its frame.
var errSeedFrameOverrun = fmt.Errorf("%w: field past the end of the frame", errMalformedSeedRecord)

// seedFrame reads the body of one record from the decompressed seed stream. It
// never reads past the frame, and it remembers whether a read failed because
// the stream itself ended or is damaged, as opposed to a malformed record.
type seedFrame struct {
	r *bufio.Reader
	// left is the number of body bytes not read yet.
	left int64
	// streamErr is the first error of the underlying stream. After it, the start
	// of the next record is unknown.
	streamErr error
}

// ReadByte implements io.ByteReader for binary.ReadUvarint.
func (f *seedFrame) ReadByte() (byte, error) {
	if f.left <= 0 {
		return 0, errSeedFrameOverrun
	}
	b, err := f.r.ReadByte()
	if err != nil {
		f.streamErr = err
		return 0, err
	}
	f.left--
	return b, nil
}

// uvarint reads one uvarint from the frame.
func (f *seedFrame) uvarint() (uint64, error) {
	n, err := binary.ReadUvarint(f)
	if err != nil {
		if f.streamErr != nil {
			return 0, err
		}
		return 0, fmt.Errorf("%w: %w", errMalformedSeedRecord, err)
	}
	return n, nil
}

// field reads one uvarint length-prefixed field of at most maxSeedField bytes.
func (f *seedFrame) field() (string, error) {
	n, err := f.uvarint()
	if err != nil {
		return "", err
	}
	if n > maxSeedField || n > uint64(f.left) {
		return "", errMalformedSeedRecord
	}
	b := make([]byte, n)
	if err := f.readFull(b); err != nil {
		return "", err
	}
	return string(b), nil
}

// readFull fills b from the frame. The caller has checked that b fits in it.
func (f *seedFrame) readFull(b []byte) error {
	if _, err := io.ReadFull(f.r, b); err != nil {
		f.streamErr = err
		return err
	}
	f.left -= int64(len(b))
	return nil
}

// copyRest copies the unread rest of the frame to w.
func (f *seedFrame) copyRest(w io.Writer) error {
	n, err := io.CopyN(w, f.r, f.left)
	f.left -= n
	if err != nil {
		f.streamErr = err
		return err
	}
	return nil
}

// seedRecordHeader is the part of a record that precedes the object data.
type seedRecordHeader struct {
	storage      string
	relativePath string
	oid          string
	typ          gitalypb.ObjectType
	size         int64
}

// readHeader reads the fields of the record up to the object data, which must
// fill the rest of the frame exactly.
func (f *seedFrame) readHeader() (seedRecordHeader, error) {
	var hdr seedRecordHeader
	var err error

	if hdr.storage, err = f.field(); err != nil {
		return hdr, err
	}
	if hdr.relativePath, err = f.field(); err != nil {
		return hdr, err
	}
	if hdr.oid, err = f.field(); err != nil {
		return hdr, err
	}
	typ, err := f.ReadByte()
	if err != nil {
		if f.streamErr != nil {
			return hdr, err
		}
		return hdr, errMalformedSeedRecord
	}
	hdr.typ = gitalypb.ObjectType(typ)
	size, err := f.uvarint()
	if err != nil {
		return hdr, err
	}
	if size != uint64(f.left) {
		return hdr, errMalformedSeedRecord
	}
	hdr.size = f.left

	if _, ok := typeName(hdr.typ); !ok {
		return hdr, fmt.Errorf("%w: unsupported object type %v", errMalformedSeedRecord, hdr.typ)
	}
	if !validOID(hdr.oid) {
		return hdr, fmt.Errorf("%w: invalid object ID", errMalformedSeedRecord)
	}
	if hdr.storage == "" || hdr.relativePath == "" {
		return hdr, fmt.Errorf("%w: missing repository", errMalformedSeedRecord)
	}
	return hdr, nil
}

// newObjectHasher returns a hash that has consumed the Git object header of an
// object of typ and size, so that hashing its data yields its object ID. The
// length of oid selects the algorithm.
func newObjectHasher(oid string, typ gitalypb.ObjectType, size int64) (hash.Hash, bool) {
	name, ok := typeName(typ)
	if !ok {
		return nil, false
	}
	var h hash.Hash
	switch len(oid) {
	case sha1HexLen:
		h = sha1.New()
	case sha256HexLen:
		h = sha256.New()
	default:
		return nil, false
	}
	fmt.Fprintf(h, "%s %d\x00", name, size)
	return h, true
}

// Seed loads the objects of one seed file into the cache. Each object is verified
// against its object ID before it is loaded. A corrupt record is skipped and
// counted, and the seed goes on with the next record. The objects go into the
// in-memory LRU and, when a disk store is configured, onto disk too. Seeding is
// meant to run before the tier serves requests.
//
// A seed file is compressed, so its records can be far larger than the file.
// Seed therefore never buffers more than one object that fits the in-memory
// budget: the fields before the object data are bounded, and the data of an
// object over the budget is hashed as it streams past and then dropped.
//
// Seed returns an error only if the file is not a seed file at all. A damaged
// or truncated file keeps the objects it held before the damage, and the damage
// is counted in the report.
func (s *Server) Seed(r io.Reader) (SeedReport, error) {
	var report SeedReport

	gz, err := gzip.NewReader(r)
	if err != nil {
		return report, fmt.Errorf("open seed: %w", err)
	}
	defer func() { _ = gz.Close() }()

	br := bufio.NewReader(gz)
	header, err := br.ReadSlice('\n')
	if err != nil || string(header) != seedHeader {
		return report, errors.New("open seed: not a graft-cache seed v1 file")
	}

	for {
		frameLen, err := binary.ReadUvarint(br)
		if errors.Is(err, io.EOF) {
			return report, nil
		}
		if err != nil || frameLen > math.MaxInt64 {
			// The frame is damaged or the file is truncated, so the next record cannot be found.
			s.countSeedCorrupt(&report)
			return report, nil
		}

		frame := &seedFrame{r: br, left: int64(frameLen)}
		if !s.seedRecord(frame, &report) {
			return report, nil
		}
	}
}

// seedRecord reads the record of frame and loads its object. It returns false
// when the stream ended or is damaged inside the frame, so the next record
// cannot be found.
func (s *Server) seedRecord(frame *seedFrame, report *SeedReport) bool {
	// skipCorrupt drops the rest of a malformed record. Its frame is intact, so
	// the next record follows it.
	skipCorrupt := func() bool {
		s.countSeedCorrupt(report)
		if frame.streamErr != nil {
			return false
		}
		return frame.copyRest(io.Discard) == nil
	}

	hdr, err := frame.readHeader()
	if err != nil {
		return skipCorrupt()
	}

	// An object over the budget could not be cached anyway. Its data is not
	// buffered: it is hashed as it streams past, only to tell an intact record
	// from a corrupt one.
	if hdr.size > s.maxBytes-entryOverhead {
		h, ok := newObjectHasher(hdr.oid, hdr.typ, hdr.size)
		if !ok {
			return skipCorrupt()
		}
		if err := frame.copyRest(h); err != nil {
			s.countSeedCorrupt(report)
			return false
		}
		if hex.EncodeToString(h.Sum(nil)) != hdr.oid {
			s.countSeedCorrupt(report)
			return true
		}
		report.Skipped++
		return true
	}

	// The data fills the rest of the frame, and its capacity equals its length,
	// which keeps the memory accounting of the LRU exact.
	data := make([]byte, hdr.size)
	if err := frame.readFull(data); err != nil {
		s.countSeedCorrupt(report)
		return false
	}

	s.loadSeedRecord(seedRecord{
		storage:      hdr.storage,
		relativePath: hdr.relativePath,
		oid:          hdr.oid,
		typ:          hdr.typ,
		data:         data,
	}, report)
	return true
}

func (s *Server) countSeedCorrupt(report *SeedReport) {
	report.Corrupt++
	s.seedCorrupt.Add(1)
}

// loadSeedRecord verifies rec and stores its object. The object must fit the
// in-memory budget.
func (s *Server) loadSeedRecord(rec seedRecord, report *SeedReport) {
	obj := &object{typ: rec.typ, data: rec.data}
	if err := verifyObject(rec.oid, obj); err != nil {
		s.countSeedCorrupt(report)
		return
	}

	size := int64(len(obj.data))
	key := objectKey{storage: rec.storage, relativePath: rec.relativePath, oid: rec.oid}
	s.store(key, obj, true)

	report.Objects++
	report.Bytes += size
	s.seededObjects.Add(1)
	s.seededBytes.Add(size)
}

// snapshotBatchObjects bounds the object IDs of one GetObjects call of a
// snapshot. It must not exceed the upstream's MaxObjects, which defaults to the
// same value.
const snapshotBatchObjects = config.DefaultGraftMaxObjects

// WriteSnapshot writes a seed file to w with every tree and blob reachable from
// the root tree of revision in repo. The objects are read from upstream with
// GetTree (recursive) and batched GetObjects calls. token, if set, is sent
// upstream as the caller's credentials. Every object is verified against its
// object ID, and a mismatch fails the snapshot. Objects are buffered one at a
// time. The caller owns w and closes it after WriteSnapshot returns.
func WriteSnapshot(ctx context.Context, upstream *grpc.ClientConn, token string, repo *gitalypb.Repository, revision string, w io.Writer) (SnapshotReport, error) {
	var report SnapshotReport

	if err := validateRepository(repo); err != nil {
		return report, err
	}
	if revision == "" {
		return report, errors.New("revision is required")
	}
	if token != "" {
		md, err := gitalyauth.RPCCredentialsV2(token).GetRequestMetadata(ctx)
		if err != nil {
			return report, fmt.Errorf("sign upstream request: %w", err)
		}
		ctx = metadata.NewOutgoingContext(ctx, metadata.New(md))
	}

	client := gitalypb.NewObjectServiceClient(upstream)
	oids, err := listSnapshotObjects(ctx, client, repo, revision)
	if err != nil {
		return report, err
	}

	sw, err := newSeedWriter(w)
	if err != nil {
		return report, err
	}
	for start := 0; start < len(oids); start += snapshotBatchObjects {
		end := min(start+snapshotBatchObjects, len(oids))
		if err := snapshotObjects(ctx, client, repo, oids[start:end], sw, &report); err != nil {
			return report, err
		}
	}
	if err := sw.close(); err != nil {
		return report, fmt.Errorf("finish seed: %w", err)
	}
	return report, nil
}

// listSnapshotObjects returns the root tree of revision followed by the distinct
// trees and blobs below it. Submodule entries are not objects of this
// repository and are left out.
func listSnapshotObjects(ctx context.Context, client gitalypb.ObjectServiceClient, repo *gitalypb.Repository, revision string) ([]string, error) {
	stream, err := client.GetTree(ctx, &gitalypb.GetTreeRequest{
		Repository: repo,
		Revision:   []byte(revision),
		Recursive:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("list tree of %s: %w", revision, err)
	}

	var oids []string
	seen := make(map[string]bool)
	add := func(oid string) {
		if !seen[oid] {
			seen[oid] = true
			oids = append(oids, oid)
		}
	}

	var root string
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list tree of %s: %w", revision, err)
		}

		if root == "" && msg.GetTreeOid() != "" {
			root = msg.GetTreeOid()
			if !validOID(root) {
				return nil, fmt.Errorf("list tree of %s: upstream returned invalid tree ID %q", revision, root)
			}
			add(root)
		}
		for _, e := range msg.GetEntries() {
			switch e.GetType() {
			case gitalypb.ObjectType_COMMIT:
				continue
			case gitalypb.ObjectType_TREE, gitalypb.ObjectType_BLOB:
				if !validOID(e.GetOid()) {
					return nil, fmt.Errorf("list tree of %s: upstream returned invalid object ID %q", revision, e.GetOid())
				}
				add(e.GetOid())
			default:
				return nil, fmt.Errorf("list tree of %s: entry %q has unexpected type %v", revision, e.GetPath(), e.GetType())
			}
		}
	}

	if root == "" {
		return nil, fmt.Errorf("list tree of %s: upstream did not report the root tree", revision)
	}
	return oids, nil
}

// snapshotObjects reads oids with one GetObjects call, verifies every object and
// writes it to sw. Missing objects are counted in report.
func snapshotObjects(ctx context.Context, client gitalypb.ObjectServiceClient, repo *gitalypb.Repository, oids []string, sw *seedWriter, report *SnapshotReport) error {
	stream, err := client.GetObjects(ctx, &gitalypb.GetObjectsRequest{Repository: repo, Oids: oids})
	if err != nil {
		return fmt.Errorf("read objects: %w", err)
	}

	type partial struct {
		oid  string
		typ  gitalypb.ObjectType
		size int64
		data []byte
	}
	var cur *partial

	finish := func() error {
		if cur == nil {
			return nil
		}
		p := cur
		cur = nil

		if int64(len(p.data)) != p.size {
			return fmt.Errorf("object %s: got %d of %d bytes", p.oid, len(p.data), p.size)
		}
		if err := verifyObject(p.oid, &object{typ: p.typ, data: p.data}); err != nil {
			return fmt.Errorf("object %s: %w", p.oid, err)
		}

		rec := seedRecord{
			storage:      repo.GetStorageName(),
			relativePath: repo.GetRelativePath(),
			oid:          p.oid,
			typ:          p.typ,
			data:         p.data,
		}
		if err := sw.write(rec); err != nil {
			return err
		}
		report.Objects++
		report.Bytes += p.size
		return nil
	}

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read objects: %w", err)
		}

		if cur != nil && msg.GetOid() != cur.oid {
			if err := finish(); err != nil {
				return err
			}
		}

		if msg.GetMissing() {
			report.Missing++
			continue
		}

		if cur == nil {
			if msg.GetOffset() != 0 {
				return fmt.Errorf("object %s starts at offset %d", msg.GetOid(), msg.GetOffset())
			}
			if _, ok := typeName(msg.GetType()); !ok {
				return fmt.Errorf("object %s has unsupported type %v", msg.GetOid(), msg.GetType())
			}
			if msg.GetSize() < 0 {
				return fmt.Errorf("object %s has negative size", msg.GetOid())
			}
			prealloc := min(msg.GetSize(), maxPreallocBytes)
			cur = &partial{oid: msg.GetOid(), typ: msg.GetType(), size: msg.GetSize(), data: make([]byte, 0, prealloc)}
		}

		if msg.GetOffset() != int64(len(cur.data)) {
			return fmt.Errorf("object %s: chunk at offset %d, expected %d", cur.oid, msg.GetOffset(), len(cur.data))
		}
		if int64(len(cur.data))+int64(len(msg.GetData())) > cur.size {
			return fmt.Errorf("object %s: more data than its declared size", cur.oid)
		}
		cur.data = append(cur.data, msg.GetData()...)
	}

	return finish()
}
