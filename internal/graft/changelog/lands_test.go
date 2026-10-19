package changelog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// landOID returns a SHA-1 object ID for n.
func landOID(n int) string { return fmt.Sprintf("%040x", n) }

// sha256OID returns a SHA-256 object ID for n.
func sha256OID(n int) string { return fmt.Sprintf("%064x", n) }

// appendLand records an update of reference to landed that lands original as landed.
func appendLand(t *testing.T, repoPath, reference, original, landed string) uint64 {
	t.Helper()

	entries, err := Append(repoPath, Meta{Operation: "land", Actor: "agent", LandReference: reference, Lands: []Land{{Original: original, Landed: landed}}},
		[]Update{{Reference: reference, OldOID: landOID(0), NewOID: landed}})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	return entries[0].Sequence
}

// appendPlain records an update of reference that lands nothing.
func appendPlain(t *testing.T, repoPath, reference, newOID string) uint64 {
	t.Helper()

	entries, err := Append(repoPath, Meta{Operation: "update_ref", Actor: "user"},
		[]Update{{Reference: reference, OldOID: landOID(0), NewOID: newOID}})
	require.NoError(t, err)
	return entries[0].Sequence
}

// lookup looks up a land and fails the test on an error.
func lookup(t *testing.T, repoPath, reference, original string) (LandRecord, bool) {
	t.Helper()

	record, found, err := LookupLand(repoPath, reference, original)
	require.NoError(t, err)
	return record, found
}

// forgetLandsIndex drops the in-memory index of the repository, as a new process starts without it.
func forgetLandsIndex(t *testing.T, repoPath string) {
	t.Helper()

	landsIndexesMu.Lock()
	defer landsIndexesMu.Unlock()
	delete(landsIndexes, landsPath(repoPath))
}

// forgetLog drops the in-memory state of the log of the repository.
func forgetLog(t *testing.T, repoPath string) {
	t.Helper()

	statesMu.Lock()
	defer statesMu.Unlock()
	delete(states, Path(repoPath))
}

func TestValidateLands(t *testing.T) {
	t.Parallel()

	land := Land{Original: landOID(1), Landed: landOID(2)}

	t.Run("a land on a branch", func(t *testing.T) {
		require.NoError(t, ValidateLands("refs/heads/main", []Land{land}))
	})

	t.Run("the largest batch with the longest branch name fits", func(t *testing.T) {
		// Every byte of the name is escaped by JSON into 6 bytes, the worst case.
		reference := "refs/heads/" + strings.Repeat("<", 4096-len("refs/heads/"))
		lands := make([]Land, MaxLandsPerUpdate)
		for i := range lands {
			lands[i] = Land{Original: sha256OID(2 * i), Landed: sha256OID(2*i + 1)}
		}

		require.NoError(t, ValidateLands(reference, lands))
	})

	t.Run("more lands than a batch has", func(t *testing.T) {
		lands := make([]Land, MaxLandsPerUpdate+1)
		for i := range lands {
			lands[i] = land
		}

		require.ErrorContains(t, ValidateLands("refs/heads/main", lands), "at most 100 per update")
	})

	t.Run("identities beyond the size bound", func(t *testing.T) {
		reference := "refs/heads/" + strings.Repeat("<", MaxLandsSize/6)

		require.ErrorContains(t, ValidateLands(reference, []Land{land}), fmt.Sprintf("at most %d", MaxLandsSize))
	})

	t.Run("not a branch", func(t *testing.T) {
		require.ErrorContains(t, ValidateLands("refs/tags/v1", []Land{land}), "not a branch")
	})

	t.Run("no lands", func(t *testing.T) {
		require.ErrorContains(t, ValidateLands("refs/heads/main", nil), "no lands")
	})

	for _, bad := range []Land{
		{Original: "", Landed: landOID(2)},
		{Original: landOID(1), Landed: "abc"},
		{Original: strings.ToUpper(landOID(0xabc)), Landed: landOID(2)},
		{Original: landOID(1), Landed: landOID(2) + "0"},
	} {
		t.Run(fmt.Sprintf("malformed %q -> %q", bad.Original, bad.Landed), func(t *testing.T) {
			require.ErrorContains(t, ValidateLands("refs/heads/main", []Land{bad}), "is not a pair of object IDs")
		})
	}
}

func TestWithLands(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	reference, lands := LandsFromContext(ctx)
	require.Empty(t, reference)
	require.Empty(t, lands)

	recorded := []Land{{Original: landOID(1), Landed: landOID(2)}}
	withLands, err := WithLands(ctx, "refs/heads/main", recorded)
	require.NoError(t, err)

	reference, lands = LandsFromContext(withLands)
	require.Equal(t, "refs/heads/main", reference)
	require.Equal(t, recorded, lands)

	unchanged, err := WithLands(ctx, "refs/tags/v1", recorded)
	require.Error(t, err)
	require.Equal(t, ctx, unchanged)
}

func TestAppendRecordsLandsOnTheirBranchOnly(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	lands := []Land{{Original: landOID(1), Landed: landOID(2)}, {Original: landOID(3), Landed: landOID(4)}}

	entries, err := Append(repoPath, Meta{Operation: "land", LandReference: "refs/heads/main", Lands: lands}, []Update{
		{Reference: "refs/heads/other", OldOID: landOID(0), NewOID: landOID(5)},
		{Reference: "refs/heads/main", OldOID: landOID(0), NewOID: landOID(4)},
	})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Empty(t, entries[0].Lands)
	require.Equal(t, lands, entries[1].Lands)

	// The lands are part of the entry in the log, and an entry without them has no field.
	scanned, err := List(repoPath, 0, 10)
	require.NoError(t, err)
	require.Equal(t, entries, scanned)

	content, err := os.ReadFile(Path(repoPath))
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimSuffix(content, []byte("\n")), []byte("\n"))
	require.Len(t, lines, 2)
	require.NotContains(t, string(lines[0]), `"lands"`)
	require.Contains(t, string(lines[1]), `"lands":[{"original":"`+landOID(1)+`","landed":"`+landOID(2)+`"}`)
}

func TestLookupLand(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()

	t.Run("without a log", func(t *testing.T) {
		_, found := lookup(t, repoPath, "refs/heads/main", landOID(1))
		require.False(t, found)
		require.NoDirExists(t, filepath.Join(repoPath, dirName), "a lookup writes nothing without a log")
	})

	first := appendLand(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
	appendPlain(t, repoPath, "refs/heads/main", landOID(9))
	other := appendLand(t, repoPath, "refs/heads/other", landOID(1), landOID(3))

	t.Run("a land on its branch", func(t *testing.T) {
		record, found := lookup(t, repoPath, "refs/heads/main", landOID(1))
		require.True(t, found)
		require.Equal(t, LandRecord{Sequence: first, Landed: landOID(2)}, record)
	})

	t.Run("the same commit on another branch is another land", func(t *testing.T) {
		record, found := lookup(t, repoPath, "refs/heads/other", landOID(1))
		require.True(t, found)
		require.Equal(t, LandRecord{Sequence: other, Landed: landOID(3)}, record)
	})

	t.Run("a commit that did not land", func(t *testing.T) {
		_, found := lookup(t, repoPath, "refs/heads/main", landOID(2))
		require.False(t, found)
	})

	t.Run("a later land of the commit replaces the record", func(t *testing.T) {
		again := appendLand(t, repoPath, "refs/heads/main", landOID(1), landOID(7))

		record, found := lookup(t, repoPath, "refs/heads/main", landOID(1))
		require.True(t, found)
		require.Equal(t, LandRecord{Sequence: again, Landed: landOID(7)}, record)
	})

	t.Run("a batch records every member", func(t *testing.T) {
		lands := []Land{{Original: landOID(11), Landed: landOID(12)}, {Original: landOID(13), Landed: landOID(14)}}
		entries, err := Append(repoPath, Meta{Operation: OperationLandBatch, LandReference: "refs/heads/main", Lands: lands},
			[]Update{{Reference: "refs/heads/main", OldOID: landOID(7), NewOID: landOID(14)}})
		require.NoError(t, err)

		for _, land := range lands {
			record, found := lookup(t, repoPath, "refs/heads/main", land.Original)
			require.True(t, found)
			require.Equal(t, LandRecord{Sequence: entries[0].Sequence, Landed: land.Landed}, record)
		}
	})
}

func TestLookupLandOnANonUTF8Branch(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	reference := "refs/heads/\xff\xfe"
	seq := appendLand(t, repoPath, reference, landOID(1), landOID(2))

	record, found := lookup(t, repoPath, reference, landOID(1))
	require.True(t, found)
	require.Equal(t, LandRecord{Sequence: seq, Landed: landOID(2)}, record)

	// The file keeps the name exactly: a new process finds the land in the file, with a log whose
	// entry no longer has it.
	rewriteLog(t, repoPath, func(entries []Entry) []Entry {
		entries[0].Lands = nil
		return entries
	})
	forgetLandsIndex(t, repoPath)

	record, found = lookup(t, repoPath, reference, landOID(1))
	require.True(t, found)
	require.Equal(t, LandRecord{Sequence: seq, Landed: landOID(2)}, record)
}

// landsIndexLines returns the decoded lines of the index file of the repository.
func landsIndexLines(t *testing.T, repoPath string) []landsLine {
	t.Helper()

	content, err := os.ReadFile(landsPath(repoPath))
	require.NoError(t, err)

	var lines []landsLine
	for _, raw := range bytes.SplitAfter(content, []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var line landsLine
		require.NoError(t, json.Unmarshal(raw, &line), "line %q", raw)
		lines = append(lines, line)
	}
	return lines
}

func TestLandsIndexFile(t *testing.T) {
	t.Parallel()

	t.Run("blocks of records and marks", func(t *testing.T) {
		repoPath := t.TempDir()
		first := appendLand(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
		lookup(t, repoPath, "refs/heads/main", landOID(1))

		plain := appendPlain(t, repoPath, "refs/heads/main", landOID(3))
		second := appendLand(t, repoPath, "refs/heads/main", landOID(4), landOID(5))
		lookup(t, repoPath, "refs/heads/main", landOID(1))

		lines := landsIndexLines(t, repoPath)
		require.Len(t, lines, 4)
		require.Equal(t, landsLine{Sequence: first, Reference: []byte("refs/heads/main"), Original: landOID(1), Landed: landOID(2)}, lines[0])
		require.Equal(t, first, lines[1].Mark)
		require.Equal(t, landOID(2), lines[1].NewOID)
		require.Equal(t, landsLine{Sequence: second, Reference: []byte("refs/heads/main"), Original: landOID(4), Landed: landOID(5)}, lines[2])
		require.Equal(t, second, lines[3].Mark)
		require.Greater(t, second, plain)
	})

	t.Run("a lookup without new entries writes nothing", func(t *testing.T) {
		repoPath := t.TempDir()
		appendLand(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
		lookup(t, repoPath, "refs/heads/main", landOID(1))

		before, err := os.ReadFile(landsPath(repoPath))
		require.NoError(t, err)

		lookup(t, repoPath, "refs/heads/main", landOID(1))
		forgetLandsIndex(t, repoPath)
		lookup(t, repoPath, "refs/heads/main", landOID(1))

		after, err := os.ReadFile(landsPath(repoPath))
		require.NoError(t, err)
		require.Equal(t, before, after)
	})

	t.Run("a new process reads the file, not the log", func(t *testing.T) {
		repoPath := t.TempDir()
		seq := appendLand(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
		lookup(t, repoPath, "refs/heads/main", landOID(1))

		// The entry is cut from the log while keeping its sequence: only the file knows the land.
		rewriteLog(t, repoPath, func(entries []Entry) []Entry {
			entries[0].Lands = nil
			return entries
		})

		forgetLandsIndex(t, repoPath)
		record, found := lookup(t, repoPath, "refs/heads/main", landOID(1))
		require.True(t, found)
		require.Equal(t, LandRecord{Sequence: seq, Landed: landOID(2)}, record)
	})
}

// rewriteLog replaces the entries of the log of the repository with what edit makes of them, and
// drops the in-memory state of the log.
func rewriteLog(t *testing.T, repoPath string, edit func([]Entry) []Entry) {
	t.Helper()

	entries, err := List(repoPath, 0, 1<<20)
	require.NoError(t, err)

	var buf bytes.Buffer
	for _, entry := range edit(entries) {
		line, err := json.Marshal(entry)
		require.NoError(t, err)
		buf.Write(line)
		buf.WriteByte('\n')
	}

	require.NoError(t, os.WriteFile(Path(repoPath), buf.Bytes(), 0o644))
	forgetLog(t, repoPath)
}

func TestLandsIndexRecovers(t *testing.T) {
	t.Parallel()

	// setUp records two lands, each read into the index in a block of its own, and a third land
	// that the index does not cover yet: the log append of the third is durable, and the process
	// stopped before a lookup brought the index up to it.
	setUp := func(t *testing.T) (string, []uint64) {
		repoPath := t.TempDir()
		var seqs []uint64
		for i := 1; i <= 3; i++ {
			seqs = append(seqs, appendLand(t, repoPath, "refs/heads/main", landOID(10*i), landOID(10*i+1)))
			if i < 3 {
				lookup(t, repoPath, "refs/heads/main", landOID(10))
			}
		}
		forgetLandsIndex(t, repoPath)
		return repoPath, seqs
	}

	// requireAll checks that every land of setUp is found with its record.
	requireAll := func(t *testing.T, repoPath string, seqs []uint64) {
		t.Helper()
		for i, seq := range seqs {
			record, found := lookup(t, repoPath, "refs/heads/main", landOID(10*(i+1)))
			require.True(t, found, "land %d", i+1)
			require.Equal(t, LandRecord{Sequence: seq, Landed: landOID(10*(i+1) + 1)}, record)
		}
		require.Equal(t, seqs[len(seqs)-1], landsIndexLines(t, repoPath)[len(landsIndexLines(t, repoPath))-1].Mark)
	}

	t.Run("an index behind the log catches up", func(t *testing.T) {
		repoPath, seqs := setUp(t)
		requireAll(t, repoPath, seqs)
	})

	t.Run("a torn tail is cut off", func(t *testing.T) {
		repoPath, seqs := setUp(t)
		appendToFile(t, landsPath(repoPath), `{"seq":3,"ref":"cmVmcy9oZWFkcy9tYWlu","original":"`)
		requireAll(t, repoPath, seqs)
	})

	t.Run("records without their mark are cut off", func(t *testing.T) {
		repoPath, seqs := setUp(t)
		bogus, err := json.Marshal(landsLine{Sequence: seqs[2], Reference: []byte("refs/heads/main"), Original: landOID(30), Landed: landOID(99)})
		require.NoError(t, err)
		appendToFile(t, landsPath(repoPath), string(bogus)+"\n")

		requireAll(t, repoPath, seqs)
	})

	t.Run("a block whose checksum does not match is cut off with what follows", func(t *testing.T) {
		repoPath, seqs := setUp(t)

		// The landed commit of the first record is changed: its line still decodes, but its mark
		// does not match it any more.
		content, err := os.ReadFile(landsPath(repoPath))
		require.NoError(t, err)
		corrupted := bytes.Replace(content, []byte(landOID(11)), []byte(landOID(66)), 1)
		require.NotEqual(t, content, corrupted)
		require.NoError(t, os.WriteFile(landsPath(repoPath), corrupted, 0o644))

		requireAll(t, repoPath, seqs)
	})

	t.Run("a line that is not JSON is cut off with what follows", func(t *testing.T) {
		repoPath, seqs := setUp(t)

		content, err := os.ReadFile(landsPath(repoPath))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(landsPath(repoPath), append([]byte("\x00\x00\x00\n"), content...), 0o644))

		requireAll(t, repoPath, seqs)
	})

	t.Run("a missing file is rebuilt from the log", func(t *testing.T) {
		repoPath, seqs := setUp(t)
		require.NoError(t, os.Remove(landsPath(repoPath)))
		requireAll(t, repoPath, seqs)
	})

	t.Run("an index of another log is rebuilt", func(t *testing.T) {
		repoPath, seqs := setUp(t)

		// The repository is created again at the same path: its log has other entries at the same
		// sequences, and the commit of the first land landed nowhere.
		rewriteLog(t, repoPath, func(entries []Entry) []Entry {
			for i := range entries {
				entries[i].NewOID = landOID(500 + i)
				entries[i].Lands = nil
			}
			entries[2].Lands = []Land{{Original: landOID(30), Landed: landOID(502)}}
			return entries
		})

		_, found := lookup(t, repoPath, "refs/heads/main", landOID(10))
		require.False(t, found)
		record, found := lookup(t, repoPath, "refs/heads/main", landOID(30))
		require.True(t, found)
		require.Equal(t, LandRecord{Sequence: seqs[2], Landed: landOID(502)}, record)
	})

	t.Run("an index past the end of the log is rebuilt", func(t *testing.T) {
		repoPath, seqs := setUp(t)
		lookup(t, repoPath, "refs/heads/main", landOID(10))

		// The log is replaced by a shorter one, which ends before the mark of the index.
		rewriteLog(t, repoPath, func(entries []Entry) []Entry { return entries[:1] })
		forgetLandsIndex(t, repoPath)

		_, found := lookup(t, repoPath, "refs/heads/main", landOID(30))
		require.False(t, found)
		record, found := lookup(t, repoPath, "refs/heads/main", landOID(10))
		require.True(t, found)
		require.Equal(t, LandRecord{Sequence: seqs[0], Landed: landOID(11)}, record)
	})

	t.Run("an index of a log that is gone answers nothing", func(t *testing.T) {
		repoPath, seqs := setUp(t)
		requireAll(t, repoPath, seqs)

		// The repository is created again at the same path, without a log yet: the index of
		// this process and the file are of the old log.
		require.NoError(t, os.Remove(Path(repoPath)))
		forgetLog(t, repoPath)
		_, found := lookup(t, repoPath, "refs/heads/main", landOID(10))
		require.False(t, found)

		seq := appendLand(t, repoPath, "refs/heads/main", landOID(50), landOID(51))
		_, found = lookup(t, repoPath, "refs/heads/main", landOID(10))
		require.False(t, found)
		record, found := lookup(t, repoPath, "refs/heads/main", landOID(50))
		require.True(t, found)
		require.Equal(t, LandRecord{Sequence: seq, Landed: landOID(51)}, record)
	})

	t.Run("the index of another process is read again", func(t *testing.T) {
		repoPath, seqs := setUp(t)
		requireAll(t, repoPath, seqs)

		// Another process brings the index up to a new land; this process sees the file grow.
		fourth := appendLand(t, repoPath, "refs/heads/main", landOID(40), landOID(41))
		index, release := acquireLandsIndex(landsPath(repoPath))
		index.mu.Lock()
		stale := index.fileSize
		index.mu.Unlock()
		release()

		forgetLandsIndex(t, repoPath)
		lookup(t, repoPath, "refs/heads/main", landOID(40))
		landsIndexesMu.Lock()
		landsIndexes[landsPath(repoPath)] = index
		landsIndexesMu.Unlock()
		require.Less(t, stale, fileSize(t, landsPath(repoPath)))

		record, found := lookup(t, repoPath, "refs/heads/main", landOID(40))
		require.True(t, found)
		require.Equal(t, LandRecord{Sequence: fourth, Landed: landOID(41)}, record)

		// The block of the other process is not written twice.
		marks := 0
		for _, line := range landsIndexLines(t, repoPath) {
			if line.Mark == fourth {
				marks++
			}
		}
		require.Equal(t, 1, marks)
	})
}

// appendToFile appends s to the file at path.
func appendToFile(t *testing.T, path, s string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(s)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// fileSize returns the size of the file at path.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}
