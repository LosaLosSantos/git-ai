package changelog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// recordIntent records the intent of a land of original on reference as landed.
func recordIntent(t *testing.T, repoPath, reference, original, landed string) {
	t.Helper()
	require.NoError(t, RecordLandIntents(repoPath, reference, []Land{{Original: original, Landed: landed}}))
}

// intentsOf returns the intents in the intents file of the repository.
func intentsOf(t *testing.T, repoPath string) []landIntent {
	t.Helper()

	var intents []landIntent
	_, _, err := readIntents(intentsPath(repoPath), intentsFile{}, func(intent landIntent) {
		intents = append(intents, intent)
	})
	require.NoError(t, err)
	return intents
}

func TestLookupLandAnswersFromIntents(t *testing.T) {
	t.Parallel()

	t.Run("an intent without a log", func(t *testing.T) {
		// Git committed the first land of the repository, and the process stopped before the
		// changelog recorded it.
		repoPath := t.TempDir()
		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(2))

		require.Equal(t, []LandRecord{{Landed: landOID(2)}}, lookupAll(t, repoPath, "refs/heads/main", landOID(1)))
		require.Empty(t, lookupAll(t, repoPath, "refs/heads/other", landOID(1)))
	})

	t.Run("an intent that the changelog confirms", func(t *testing.T) {
		repoPath := t.TempDir()
		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
		seq := appendLand(t, repoPath, "refs/heads/main", landOID(1), landOID(2))

		require.Equal(t, []LandRecord{{Sequence: seq, Landed: landOID(2)}}, lookupAll(t, repoPath, "refs/heads/main", landOID(1)))
	})

	t.Run("an intent that a later land supersedes", func(t *testing.T) {
		// The update of the intent was not committed, and the change was landed again.
		repoPath := t.TempDir()
		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
		seq := appendLand(t, repoPath, "refs/heads/main", landOID(1), landOID(3))

		require.Equal(t, []LandRecord{{Sequence: seq, Landed: landOID(3)}}, lookupAll(t, repoPath, "refs/heads/main", landOID(1)))
	})

	t.Run("an intent after the recorded land", func(t *testing.T) {
		// The change landed, the branch was reset past it, and its new land was committed
		// without the changelog recording it: both are known, the record first.
		repoPath := t.TempDir()
		seq := appendLand(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(3))

		require.Equal(t, []LandRecord{{Sequence: seq, Landed: landOID(2)}, {Landed: landOID(3)}},
			lookupAll(t, repoPath, "refs/heads/main", landOID(1)))
	})

	t.Run("a branch whose name is not valid UTF-8", func(t *testing.T) {
		repoPath := t.TempDir()
		recordIntent(t, repoPath, "refs/heads/\xff", landOID(1), landOID(2))

		require.Equal(t, []LandRecord{{Landed: landOID(2)}}, lookupAll(t, repoPath, "refs/heads/\xff", landOID(1)))
		require.Empty(t, lookupAll(t, repoPath, "refs/heads/�", landOID(1)))
	})

	t.Run("intents recorded by another process", func(t *testing.T) {
		repoPath := t.TempDir()
		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
		require.Len(t, lookupAll(t, repoPath, "refs/heads/main", landOID(1)), 1)

		recordIntent(t, repoPath, "refs/heads/main", landOID(3), landOID(4))
		require.Equal(t, []LandRecord{{Landed: landOID(4)}}, lookupAll(t, repoPath, "refs/heads/main", landOID(3)))
	})
}

func TestLandIntentsFile(t *testing.T) {
	t.Parallel()

	t.Run("a torn line is cut off by the next intent", func(t *testing.T) {
		// A writer crashed in the middle of its line, before its fsync: its update was not
		// committed. The next writer cuts the line off, so that its own line stays whole.
		repoPath := t.TempDir()
		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
		appendToFile(t, intentsPath(repoPath), `{"ref":"cmVmcy9oZWFkcy9tYWlu","original":"`+landOID(5))

		_, err := LookupLand(repoPath, "refs/heads/main", landOID(5))
		require.NoError(t, err, "a reader ignores the torn line")

		recordIntent(t, repoPath, "refs/heads/main", landOID(3), landOID(4))
		require.Len(t, intentsOf(t, repoPath), 2)
		require.Equal(t, []LandRecord{{Landed: landOID(4)}}, lookupAll(t, repoPath, "refs/heads/main", landOID(3)))
		require.Empty(t, lookupAll(t, repoPath, "refs/heads/main", landOID(5)))
	})

	t.Run("a line that is not an intent is skipped", func(t *testing.T) {
		repoPath := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Dir(intentsPath(repoPath)), 0o755))
		require.NoError(t, os.WriteFile(intentsPath(repoPath), []byte("\x00\x00\n{\"ref\":\"\",\"original\":\"nope\",\"landed\":\"x\"}\n"), 0o644))
		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(2))

		require.Equal(t, []LandRecord{{Landed: landOID(2)}}, lookupAll(t, repoPath, "refs/heads/main", landOID(1)))
	})

	t.Run("an intent records the head of the log", func(t *testing.T) {
		repoPath := t.TempDir()
		seq := appendPlain(t, repoPath, "refs/heads/main", landOID(9))
		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(2))

		intents := intentsOf(t, repoPath)
		require.Len(t, intents, 1)
		require.Equal(t, seq, intents[0].Head)
	})
}

// TestLandIntentsAreDurable replaces syncFile, so it does not run in parallel.
func TestLandIntentsAreDurable(t *testing.T) {
	t.Run("the intents are fsync'd before they return", func(t *testing.T) {
		repoPath := t.TempDir()
		var synced []string
		replaceSyncFile(t, func(f *os.File) error {
			synced = append(synced, filepath.Base(f.Name()))
			return f.Sync()
		})

		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
		require.Contains(t, synced, intentsFileName)
	})

	t.Run("a failed fsync fails the intent", func(t *testing.T) {
		repoPath := t.TempDir()
		replaceSyncFile(t, func(f *os.File) error {
			if filepath.Base(f.Name()) == intentsFileName {
				return fmt.Errorf("injected")
			}
			return f.Sync()
		})

		require.ErrorContains(t, RecordLandIntents(repoPath, "refs/heads/main", []Land{{Original: landOID(1), Landed: landOID(2)}}), "injected")
	})
}

func TestLandIntentsCompaction(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()

	// Enough intents to compact the file: half of them confirmed by the changelog, a quarter
	// superseded by a later land, a quarter never committed and never landed again.
	const n = 2000
	lands := make([]Land, n)
	for i := range lands {
		lands[i] = Land{Original: landOID(10000 + i), Landed: landOID(20000 + i)}
	}
	require.NoError(t, RecordLandIntents(repoPath, "refs/heads/main", lands))
	require.Greater(t, fileSize(t, intentsPath(repoPath)), int64(compactIntentsMinSize))

	var confirmed []Land
	for i := 0; i < n/2; i++ {
		confirmed = append(confirmed, lands[i])
	}
	for i := n / 2; i < 3*n/4; i++ {
		confirmed = append(confirmed, Land{Original: lands[i].Original, Landed: landOID(30000 + i)})
	}
	updates := make([]Update, 0, len(confirmed))
	for i := range confirmed {
		updates = append(updates, Update{Reference: "refs/heads/main", OldOID: landOID(0), NewOID: landOID(40000 + i)})
	}
	// One entry per land, each carrying its own land.
	records := make([]Record, len(confirmed))
	for i, land := range confirmed {
		records[i] = Record{Meta: Meta{Operation: "land", LandReference: "refs/heads/main", Lands: []Land{land}}, Updates: updates[i : i+1]}
	}
	_, err := AppendGroup(repoPath, records)
	require.NoError(t, err)

	lookupAll(t, repoPath, "refs/heads/main", landOID(10000))

	kept := intentsOf(t, repoPath)
	require.Len(t, kept, n/4, "only the intents that the changelog neither confirms nor supersedes are kept")
	for _, intent := range kept {
		require.True(t, strings.Compare(intent.Original, landOID(10000+3*n/4)) >= 0)
	}

	// The kept intents, more than the size from which a file is compacted, are not rewritten by
	// every lookup.
	require.Greater(t, fileSize(t, intentsPath(repoPath)), int64(compactIntentsMinSize))
	compacted, err := os.Stat(intentsPath(repoPath))
	require.NoError(t, err)
	lookupAll(t, repoPath, "refs/heads/main", landOID(10000))
	again, err := os.Stat(intentsPath(repoPath))
	require.NoError(t, err)
	require.True(t, os.SameFile(compacted, again))

	// Every land is still answered as before the compaction, also by a new process.
	forgetLandsIndex(t, repoPath)
	for i, land := range lands {
		records := lookupAll(t, repoPath, "refs/heads/main", land.Original)
		require.Len(t, records, 1)
		switch {
		case i < n/2:
			require.Equal(t, land.Landed, records[0].Landed)
			require.NotZero(t, records[0].Sequence)
		case i < 3*n/4:
			require.Equal(t, landOID(30000+i), records[0].Landed)
		default:
			require.Equal(t, LandRecord{Landed: land.Landed}, records[0])
		}
	}
}

func TestLandsIndexCompaction(t *testing.T) {
	t.Parallel()

	// The same commit lands again and again (its branch reset past it each time): every record
	// but the last is superseded.
	repoPath := t.TempDir()
	const n = 8000
	records := make([]Record, n)
	for i := range records {
		records[i] = Record{
			Meta:    Meta{Operation: "land", LandReference: "refs/heads/main", Lands: []Land{{Original: landOID(1), Landed: landOID(100 + i)}}},
			Updates: []Update{{Reference: "refs/heads/main", OldOID: landOID(0), NewOID: landOID(100 + i)}},
		}
	}
	entries, err := AppendGroup(repoPath, records)
	require.NoError(t, err)
	last := entries[n-1][0]

	record, found := lookup(t, repoPath, "refs/heads/main", landOID(1))
	require.True(t, found)
	require.Equal(t, LandRecord{Sequence: last.Sequence, Landed: landOID(100 + n - 1)}, record)

	lines := landsIndexLines(t, repoPath)
	require.Len(t, lines, 2, "the compacted file holds one record and its mark")
	require.Equal(t, last.Sequence, lines[1].Mark)

	forgetLandsIndex(t, repoPath)
	record, found = lookup(t, repoPath, "refs/heads/main", landOID(1))
	require.True(t, found)
	require.Equal(t, LandRecord{Sequence: last.Sequence, Landed: landOID(100 + n - 1)}, record)
}

func TestLandsIndexOfAReplacedLogFile(t *testing.T) {
	t.Parallel()

	// The repository is created again at the same path, and its log ends with the same entry as
	// the old one: the index in memory, whose mark still matches, must not answer for it.
	repoPath := t.TempDir()
	appendLand(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
	appendPlain(t, repoPath, "refs/heads/main", landOID(9))
	_, found := lookup(t, repoPath, "refs/heads/main", landOID(1))
	require.True(t, found)

	entries, err := List(repoPath, 0, 10)
	require.NoError(t, err)
	entries[0].Lands = nil
	var buf bytes.Buffer
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		require.NoError(t, err)
		buf.Write(line)
		buf.WriteByte('\n')
	}
	replacement := Path(repoPath) + ".new"
	require.NoError(t, os.WriteFile(replacement, buf.Bytes(), 0o644))
	require.NoError(t, os.Rename(replacement, Path(repoPath)))
	forgetLog(t, repoPath)

	_, found = lookup(t, repoPath, "refs/heads/main", landOID(1))
	require.False(t, found)
}

func TestLandsIndexesAreEvictedLeastRecentlyUsedFirst(t *testing.T) {
	// The test fills the shared cache of indexes, so it does not run in parallel.
	landsIndexesMu.Lock()
	saved := landsIndexes
	landsIndexes = map[string]*landsIndex{}
	landsIndexesMu.Unlock()
	t.Cleanup(func() {
		landsIndexesMu.Lock()
		landsIndexes = saved
		landsIndexesMu.Unlock()
	})

	repos := make([]string, maxIdleLandsIndexes+1)
	for i := range repos {
		repos[i] = t.TempDir()
		appendLand(t, repos[i], "refs/heads/main", landOID(1), landOID(2))
		lookup(t, repos[i], "refs/heads/main", landOID(1))
	}
	cached := func(repoPath string) bool {
		landsIndexesMu.Lock()
		defer landsIndexesMu.Unlock()
		_, ok := landsIndexes[landsPath(repoPath)]
		return ok
	}

	// The first repository was used least recently when the last one came in.
	require.False(t, cached(repos[0]))
	for _, repoPath := range repos[1:] {
		require.True(t, cached(repoPath))
	}

	// Using the second repository again keeps it; the third is now the least recently used.
	lookup(t, repos[1], "refs/heads/main", landOID(1))
	lookup(t, repos[0], "refs/heads/main", landOID(1))
	require.True(t, cached(repos[1]))
	require.False(t, cached(repos[2]))
}
