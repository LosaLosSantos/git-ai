package cachetier

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"runtime"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// snapshotUpstream answers GetTree for a revision with a fixed root tree, the way
// the upstream does. The objects themselves are served by the embedded fake.
type snapshotUpstream struct {
	*fakeObjectService
	root    string
	entries []*gitalypb.GetTreeResponse_Entry
}

func (u *snapshotUpstream) GetTree(req *gitalypb.GetTreeRequest, stream gitalypb.ObjectService_GetTreeServer) error {
	return stream.Send(&gitalypb.GetTreeResponse{TreeOid: u.root, Entries: u.entries})
}

// snapshotFixture is a repository with a root tree, a subtree and two blobs in
// the upstream, and a submodule entry that a snapshot must skip.
type snapshotFixture struct {
	repo     *gitalypb.Repository
	upstream *fakeObjectService
	addr     string
	root     string
	subtree  string
	blobA    string
	blobB    string
}

func newSnapshotFixture(t *testing.T) snapshotFixture {
	t.Helper()

	repo := testRepo("@hashed/seed.git")
	upstream := newFakeObjectService()
	blobA := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("alpha\n"), false)
	blobB := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("beta\n"), false)
	subtree := upstream.put(repo, gitalypb.ObjectType_TREE, []byte("subtree listing"), false)
	root := upstream.put(repo, gitalypb.ObjectType_TREE, []byte("root listing"), false)
	submodule := gitObjectID("commit", []byte("submodule"), false)

	srv := &snapshotUpstream{
		fakeObjectService: upstream,
		root:              root,
		entries: []*gitalypb.GetTreeResponse_Entry{
			{Path: []byte("a.txt"), Oid: blobA, Type: gitalypb.ObjectType_BLOB, Mode: 0o100644},
			{Path: []byte("sub"), Oid: subtree, Type: gitalypb.ObjectType_TREE, Mode: 0o040000},
			{Path: []byte("sub/b.txt"), Oid: blobB, Type: gitalypb.ObjectType_BLOB, Mode: 0o100644},
			{Path: []byte("module"), Oid: submodule, Type: gitalypb.ObjectType_COMMIT, Mode: 0o160000},
		},
	}
	addr := startGRPC(t, func(s *grpc.Server) {
		gitalypb.RegisterObjectServiceServer(s, srv)
		gitalypb.RegisterRepositoryServiceServer(s, &fakeRepositoryService{})
	})

	return snapshotFixture{repo: repo, upstream: upstream, addr: addr, root: root, subtree: subtree, blobA: blobA, blobB: blobB}
}

func TestSeed_SnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	fx := newSnapshotFixture(t)

	var seed bytes.Buffer
	snapshot, err := WriteSnapshot(ctx, dialUpstream(t, fx.addr), "", fx.repo, "main", &seed)
	require.NoError(t, err)
	require.Equal(t, SnapshotReport{
		Objects: 4,
		Bytes:   int64(len("root listing") + len("subtree listing") + len("alpha\n") + len("beta\n")),
	}, snapshot, "the root tree, the subtree and both blobs; the submodule is skipped")

	tier := newTierOver(t, fx.addr, Config{MaxBytes: 1 << 20})
	loaded, err := tier.server.Seed(bytes.NewReader(seed.Bytes()))
	require.NoError(t, err)
	require.Equal(t, SeedReport{Objects: 4, Bytes: snapshot.Bytes}, loaded)

	before := fx.upstream.getObjectsCalls.Load()
	got, _ := getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{
		Repository: fx.repo,
		Oids:       []string{fx.root, fx.subtree, fx.blobA, fx.blobB},
	})
	require.Equal(t, before, fx.upstream.getObjectsCalls.Load(), "seeded objects are served without an upstream object read")
	require.Equal(t, []byte("alpha\n"), got[fx.blobA].data)
	require.Equal(t, gitalypb.ObjectType_BLOB, got[fx.blobA].typ)
	require.Equal(t, []byte("root listing"), got[fx.root].data)
	require.Equal(t, gitalypb.ObjectType_TREE, got[fx.root].typ)

	stats := tier.server.Stats()
	require.Equal(t, int64(4), stats.SeededObjects)
	require.Equal(t, snapshot.Bytes, stats.SeededBytes)
	require.Equal(t, int64(4), stats.ObjectHits)
	require.Zero(t, stats.ObjectMisses)
	require.Zero(t, stats.SeedCorrupt)
	require.Equal(t, 1.0, stats.ObjectHitRate)
}

func TestSeed_SkipsCorruptRecords(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/corrupt.git")

	good1 := gitObjectID("blob", []byte("one"), false)
	bad := gitObjectID("blob", []byte("two"), false)
	good2 := gitObjectID("blob", []byte("three"), false)

	var seed bytes.Buffer
	w, err := newSeedWriter(&seed)
	require.NoError(t, err)
	record := func(oid string, data []byte) seedRecord {
		return seedRecord{storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(), oid: oid, typ: gitalypb.ObjectType_BLOB, data: data}
	}
	require.NoError(t, w.write(record(good1, []byte("one"))))
	// The content does not hash to the object ID.
	require.NoError(t, w.write(record(bad, []byte("TWO"))))
	// A frame whose body does not decode. The frame itself is intact, so the next record is still found.
	framed := binary.AppendUvarint(nil, 3)
	_, err = w.gz.Write(append(framed, "abc"...))
	require.NoError(t, err)
	require.NoError(t, w.write(record(good2, []byte("three"))))
	require.NoError(t, w.close())

	upstream := newFakeObjectService()
	tier := newTier(t, upstream, Config{MaxBytes: 1 << 20})

	report, err := tier.server.Seed(bytes.NewReader(seed.Bytes()))
	require.NoError(t, err)
	require.Equal(t, SeedReport{Objects: 2, Bytes: int64(len("one") + len("three")), Corrupt: 2}, report)

	before := upstream.getObjectsCalls.Load()
	got, _ := getObjects(t, ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{good1, good2}})
	require.Equal(t, []byte("one"), got[good1].data)
	require.Equal(t, []byte("three"), got[good2].data)
	require.Equal(t, before, upstream.getObjectsCalls.Load())

	stats := tier.server.Stats()
	require.Equal(t, int64(2), stats.SeedCorrupt)
	require.Equal(t, int64(2), stats.SeededObjects)
}

func TestSeed_TruncatedFileKeepsTheRecordsBeforeTheDamage(t *testing.T) {
	t.Parallel()

	repo := testRepo("@hashed/truncated.git")

	var seed bytes.Buffer
	w, err := newSeedWriter(&seed)
	require.NoError(t, err)
	for _, data := range []string{"first", "second", "third"} {
		require.NoError(t, w.write(seedRecord{
			storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(),
			oid: gitObjectID("blob", []byte(data), false), typ: gitalypb.ObjectType_BLOB, data: []byte(data),
		}))
	}
	require.NoError(t, w.close())

	// Cutting the end removes the gzip trailer and the end of the deflate stream.
	truncated := seed.Bytes()[:seed.Len()-20]

	tier := newTier(t, newFakeObjectService(), Config{MaxBytes: 1 << 20})
	report, err := tier.server.Seed(bytes.NewReader(truncated))
	require.NoError(t, err, "a damaged seed is not an error")
	require.Equal(t, int64(1), report.Corrupt)
	require.LessOrEqual(t, report.Objects, int64(3))
}

func TestSeed_RejectsFilesThatAreNotSeeds(t *testing.T) {
	t.Parallel()

	tier := newTier(t, newFakeObjectService(), Config{MaxBytes: 1 << 20})

	_, err := tier.server.Seed(strings.NewReader("not gzip at all"))
	require.Error(t, err)

	var notSeed bytes.Buffer
	gz := gzip.NewWriter(&notSeed)
	_, err = gz.Write([]byte("some other format\n"))
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	_, err = tier.server.Seed(&notSeed)
	require.ErrorContains(t, err, "not a graft-cache seed v1 file")
}

func TestSeed_ObjectsOverTheBudgetAreSkipped(t *testing.T) {
	t.Parallel()

	repo := testRepo("@hashed/budget.git")
	data := []byte("larger than the budget")

	var seed bytes.Buffer
	w, err := newSeedWriter(&seed)
	require.NoError(t, err)
	require.NoError(t, w.write(seedRecord{
		storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(),
		oid: gitObjectID("blob", data, false), typ: gitalypb.ObjectType_BLOB, data: data,
	}))
	require.NoError(t, w.close())

	tier := newTier(t, newFakeObjectService(), Config{MaxBytes: 64})
	report, err := tier.server.Seed(bytes.NewReader(seed.Bytes()))
	require.NoError(t, err)
	require.Equal(t, SeedReport{Skipped: 1}, report)
	require.Zero(t, tier.server.Stats().MemoryEntries)
}

// allocatedBy returns the bytes allocated while f runs. Callers must not run in
// parallel with other tests, which would add their own allocations.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// rawSeedRecordHeader encodes the fields of a record that precede its data, the
// way seedWriter does, so that tests can write frames whose lengths lie.
func rawSeedRecordHeader(storage, relativePath, oid string, typ gitalypb.ObjectType, size uint64) []byte {
	var hdr []byte
	for _, field := range []string{storage, relativePath, oid} {
		hdr = binary.AppendUvarint(hdr, uint64(len(field)))
		hdr = append(hdr, field...)
	}
	hdr = append(hdr, byte(typ))
	return binary.AppendUvarint(hdr, size)
}

// A seed is compressed, so a small file can hold a huge record. Seeding must not
// buffer an object it cannot cache. These tests do not run in parallel because
// they measure the allocations of the whole process.
func TestSeed_RecordsOverTheBudgetAreNotBuffered(t *testing.T) {
	repo := testRepo("@hashed/seed-bomb.git")
	const bombSize = 64 << 20
	const allocLimit = 8 << 20

	zeros := make([]byte, bombSize)
	small := []byte("loaded after the large record")
	smallOID := gitObjectID("blob", small, false)

	for _, tc := range []struct {
		desc     string
		oid      string
		expected SeedReport
	}{
		{
			desc:     "intact record",
			oid:      gitObjectID("blob", zeros, false),
			expected: SeedReport{Objects: 1, Bytes: int64(len(small)), Skipped: 1},
		},
		{
			desc:     "content does not match the object ID",
			oid:      gitObjectID("blob", []byte("something else"), false),
			expected: SeedReport{Objects: 1, Bytes: int64(len(small)), Corrupt: 1},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var seed bytes.Buffer
			w, err := newSeedWriter(&seed)
			require.NoError(t, err)
			require.NoError(t, w.write(seedRecord{
				storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(),
				oid: tc.oid, typ: gitalypb.ObjectType_BLOB, data: zeros,
			}))
			require.NoError(t, w.write(seedRecord{
				storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(),
				oid: smallOID, typ: gitalypb.ObjectType_BLOB, data: small,
			}))
			require.NoError(t, w.close())
			require.Less(t, seed.Len(), bombSize/100, "the record compresses well")

			tier := newTier(t, newFakeObjectService(), Config{MaxBytes: 1 << 20})

			var report SeedReport
			allocated := allocatedBy(func() {
				report, err = tier.server.Seed(bytes.NewReader(seed.Bytes()))
			})
			require.NoError(t, err)
			require.Equal(t, tc.expected, report)
			require.Less(t, allocated, uint64(allocLimit), "the large record is streamed, not buffered")
			require.Equal(t, 1, tier.server.Stats().MemoryEntries)
		})
	}
}

func TestSeed_LyingLengthsDoNotAllocate(t *testing.T) {
	repo := testRepo("@hashed/seed-lies.git")
	oid := gitObjectID("blob", []byte("data"), false)
	const allocLimit = 8 << 20
	const huge = 1 << 40

	// objectFrame starts a well-formed record of an object of size bytes, without
	// its data.
	objectFrame := func(size uint64) []byte {
		hdr := rawSeedRecordHeader(repo.GetStorageName(), repo.GetRelativePath(), oid, gitalypb.ObjectType_BLOB, size)
		return append(binary.AppendUvarint(nil, uint64(len(hdr))+size), hdr...)
	}

	for _, tc := range []struct {
		desc  string
		frame []byte
	}{
		{
			desc:  "object over the budget and larger than the file",
			frame: objectFrame(huge),
		},
		{
			desc:  "object within the budget and larger than the file",
			frame: objectFrame(1 << 19),
		},
		{
			desc:  "field larger than the file",
			frame: binary.AppendUvarint(binary.AppendUvarint(nil, huge), huge-10),
		},
		{
			desc:  "frame larger than the file",
			frame: append(binary.AppendUvarint(nil, huge), "short"...),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var seed bytes.Buffer
			w, err := newSeedWriter(&seed)
			require.NoError(t, err)
			require.NoError(t, w.write(seedRecord{
				storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(),
				oid: oid, typ: gitalypb.ObjectType_BLOB, data: []byte("data"),
			}))
			_, err = w.gz.Write(tc.frame)
			require.NoError(t, err)
			require.NoError(t, w.close())

			tier := newTier(t, newFakeObjectService(), Config{MaxBytes: 1 << 30})

			var report SeedReport
			allocated := allocatedBy(func() {
				report, err = tier.server.Seed(bytes.NewReader(seed.Bytes()))
			})
			require.NoError(t, err)
			require.Equal(t, SeedReport{Objects: 1, Bytes: 4, Corrupt: 1}, report)
			require.Less(t, allocated, uint64(allocLimit))
		})
	}
}

func TestSeed_OversizedFieldIsCorruptAndSkipped(t *testing.T) {
	t.Parallel()

	repo := testRepo("@hashed/seed-field.git")
	data := []byte("after the bad record")
	oid := gitObjectID("blob", data, false)

	// The frame is intact, but its storage name is longer than any valid one.
	longStorage := strings.Repeat("s", maxSeedField+1)
	body := append(rawSeedRecordHeader(longStorage, repo.GetRelativePath(), oid, gitalypb.ObjectType_BLOB, 4), "data"...)

	var seed bytes.Buffer
	w, err := newSeedWriter(&seed)
	require.NoError(t, err)
	_, err = w.gz.Write(append(binary.AppendUvarint(nil, uint64(len(body))), body...))
	require.NoError(t, err)
	require.NoError(t, w.write(seedRecord{
		storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(),
		oid: oid, typ: gitalypb.ObjectType_BLOB, data: data,
	}))
	require.NoError(t, w.close())

	tier := newTier(t, newFakeObjectService(), Config{MaxBytes: 1 << 20})
	report, err := tier.server.Seed(bytes.NewReader(seed.Bytes()))
	require.NoError(t, err)
	require.Equal(t, SeedReport{Objects: 1, Bytes: int64(len(data)), Corrupt: 1}, report)
}

func TestSeed_DiskStoreKeepsSeededObjects(t *testing.T) {
	t.Parallel()

	repo := testRepo("@hashed/disk-seed.git")
	data := []byte("persisted by the seed")
	oid := gitObjectID("blob", data, false)

	var seed bytes.Buffer
	w, err := newSeedWriter(&seed)
	require.NoError(t, err)
	require.NoError(t, w.write(seedRecord{
		storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(),
		oid: oid, typ: gitalypb.ObjectType_BLOB, data: data,
	}))
	require.NoError(t, w.close())

	upstreamAddr := startGRPC(t, func(s *grpc.Server) {
		gitalypb.RegisterObjectServiceServer(s, newFakeObjectService())
		gitalypb.RegisterRepositoryServiceServer(s, &fakeRepositoryService{})
	})
	tier := newTierOver(t, upstreamAddr, Config{MaxBytes: 1 << 20, DiskDir: t.TempDir()})

	_, err = tier.server.Seed(bytes.NewReader(seed.Bytes()))
	require.NoError(t, err)

	key := objectKey{storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(), oid: oid}
	obj, err := tier.server.disk.read(key, noReadLimit)
	require.NoError(t, err)
	require.NotNil(t, obj)
	require.Equal(t, data, obj.data)
}

func TestSnapshot_CorruptUpstreamObjectFailsTheSnapshot(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	fx := newSnapshotFixture(t)
	fx.upstream.corruptOID(fx.blobA)

	var seed bytes.Buffer
	_, err := WriteSnapshot(ctx, dialUpstream(t, fx.addr), "", fx.repo, "main", &seed)
	require.ErrorContains(t, err, fx.blobA)
}

func TestSnapshot_ValidatesArguments(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	fx := newSnapshotFixture(t)
	conn := dialUpstream(t, fx.addr)

	_, err := WriteSnapshot(ctx, conn, "", &gitalypb.Repository{}, "main", &bytes.Buffer{})
	require.Error(t, err)

	_, err = WriteSnapshot(ctx, conn, "", fx.repo, "", &bytes.Buffer{})
	require.ErrorContains(t, err, "revision is required")
}

func TestStats_HitRates(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/rates.git")
	upstream := newFakeObjectService()
	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("hello"), false)

	tier := newTier(t, upstream, Config{MaxBytes: 1 << 20})
	require.Zero(t, tier.server.Stats().HitRate, "no lookups yet, and no NaN")

	req := &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}}
	getObjects(t, ctx, tier.client, req)
	getObjects(t, ctx, tier.client, req)

	stats := tier.server.Stats()
	require.Equal(t, 0.5, stats.ObjectHitRate)
	require.Equal(t, 0.5, stats.HitRate)
	require.Zero(t, stats.TreeHitRate)
}
