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

// invocationOf returns the invocation that the tests give the update of the land of original.
func invocationOf(original string) string { return "inv-" + original }

// recordIntent records the intent of a land of original on reference as landed, by the update
// invocationOf(original).
func recordIntent(t *testing.T, repoPath, reference, original, landed string) {
	t.Helper()
	_, err := RecordLandIntents(repoPath, reference, invocationOf(original), []Land{{Original: original, Landed: landed}})
	require.NoError(t, err)
}

// confirmLand records the changelog entry of the update invocation, which landed original as
// landed on reference.
func confirmLand(t *testing.T, repoPath, reference, original, landed, invocation string) uint64 {
	t.Helper()

	entries, err := Append(repoPath, Meta{Operation: "land", LandReference: reference, LandInvocation: invocation, Lands: []Land{{Original: original, Landed: landed}}},
		[]Update{{Reference: reference, OldOID: landOID(0), NewOID: landed}})
	require.NoError(t, err)
	return entries[0].Sequence
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

	t.Run("an intent of the recorded commit", func(t *testing.T) {
		repoPath := t.TempDir()
		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
		seq := confirmLand(t, repoPath, "refs/heads/main", landOID(1), landOID(2), invocationOf(landOID(1)))

		require.Equal(t, []LandRecord{{Sequence: seq, Landed: landOID(2)}}, lookupAll(t, repoPath, "refs/heads/main", landOID(1)))
	})

	t.Run("an intent of another commit than the recorded one", func(t *testing.T) {
		// Review m1: land A of X is committed and its entry is pending; the branch is reset; land
		// B of X is committed with its intent; A's entry is written, and B's is lost. The intent
		// of B is not left out because a record came after it: it proves B by itself, when its
		// commit is on the branch.
		repoPath := t.TempDir()
		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(3))
		seq := confirmLand(t, repoPath, "refs/heads/main", landOID(1), landOID(2), "inv-a")

		require.Equal(t, []LandRecord{{Sequence: seq, Landed: landOID(2)}, {Landed: landOID(3)}},
			lookupAll(t, repoPath, "refs/heads/main", landOID(1)))
	})

	t.Run("an intent after the recorded land", func(t *testing.T) {
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

	t.Run("the entry carries the invocation of its update", func(t *testing.T) {
		repoPath := t.TempDir()
		confirmLand(t, repoPath, "refs/heads/main", landOID(1), landOID(2), "inv-x")
		appendPlain(t, repoPath, "refs/heads/main", landOID(9))

		entries, err := List(repoPath, 0, 10)
		require.NoError(t, err)
		require.Equal(t, "inv-x", entries[0].LandInvocation)
		require.Empty(t, entries[1].LandInvocation, "an entry without lands has no invocation")
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

	t.Run("an intent carries the invocation of its update", func(t *testing.T) {
		repoPath := t.TempDir()
		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(2))

		intents := intentsOf(t, repoPath)
		require.Len(t, intents, 1)
		require.Equal(t, invocationOf(landOID(1)), intents[0].Invocation)
	})

	t.Run("a reader that knows nothing reads the file once", func(t *testing.T) {
		repoPath := t.TempDir()
		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
		recordIntent(t, repoPath, "refs/heads/main", landOID(3), landOID(4))

		read := 0
		known, reset, err := readIntents(intentsPath(repoPath), intentsFile{}, func(landIntent) { read++ })
		require.NoError(t, err)
		require.False(t, reset)
		require.Equal(t, 2, read)
		require.Equal(t, fileSize(t, intentsPath(repoPath)), known.offset)
	})
}

// TestLandIntentsAreDurable replaces syncFile and syncDir, so it does not run in parallel.
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

		_, err := RecordLandIntents(repoPath, "refs/heads/main", "inv", []Land{{Original: landOID(1), Landed: landOID(2)}})
		require.ErrorContains(t, err, "injected")
	})

	t.Run("the directories are synced for a file created again", func(t *testing.T) {
		// Review m3: the repository is removed and created again at the same path. The intents
		// file of the new repository is another file, whose directory entries are synced.
		repoPath := t.TempDir()
		dirSyncs := 0
		original := syncDir
		syncDir = func(dir string) error {
			dirSyncs++
			return original(dir)
		}
		t.Cleanup(func() { syncDir = original })

		recordIntent(t, repoPath, "refs/heads/main", landOID(1), landOID(2))
		require.Positive(t, dirSyncs, "the write that created the file syncs the directories")

		dirSyncs = 0
		recordIntent(t, repoPath, "refs/heads/main", landOID(3), landOID(4))
		require.Zero(t, dirSyncs, "a later write to the same file does not")

		require.NoError(t, os.RemoveAll(filepath.Join(repoPath, dirName)))
		recordIntent(t, repoPath, "refs/heads/main", landOID(5), landOID(6))
		require.Positive(t, dirSyncs, "the write that created the file again syncs the directories")
	})
}

func TestLandIntentsCompaction(t *testing.T) {
	t.Parallel()

	// recordAndConfirm records n intents in one update each, and the changelog entries of the
	// updates of the first confirmed of them. The entries of the next superseded intents' commits
	// are written by other updates, with other landed commits: they do not confirm the intents.
	recordAndConfirm := func(t *testing.T, repoPath string, n, confirmed, superseded int) []Land {
		lands := make([]Land, n)
		for i := range lands {
			lands[i] = Land{Original: landOID(10000 + i), Landed: landOID(20000 + i)}
			_, err := RecordLandIntents(repoPath, "refs/heads/main", invocationOf(lands[i].Original), lands[i:i+1])
			require.NoError(t, err)
		}

		var records []Record
		for i := 0; i < confirmed+superseded; i++ {
			land, invocation := lands[i], invocationOf(lands[i].Original)
			if i >= confirmed {
				land, invocation = Land{Original: lands[i].Original, Landed: landOID(30000 + i)}, "inv-other"
			}
			records = append(records, Record{
				Meta:    Meta{Operation: "land", LandReference: "refs/heads/main", LandInvocation: invocation, Lands: []Land{land}},
				Updates: []Update{{Reference: "refs/heads/main", OldOID: landOID(0), NewOID: landOID(40000 + i)}},
			})
		}
		_, err := AppendGroup(repoPath, records)
		require.NoError(t, err)

		return lands
	}

	t.Run("an intent goes once the entry of its own update is durable", func(t *testing.T) {
		repoPath := t.TempDir()
		const n = 2000
		lands := recordAndConfirm(t, repoPath, n, 3*n/4, n/8)
		require.Greater(t, fileSize(t, intentsPath(repoPath)), int64(compactIntentsMinSize))

		lookupAll(t, repoPath, "refs/heads/main", landOID(10000))

		kept := intentsOf(t, repoPath)
		require.Len(t, kept, n/4, "the intents whose own update has no entry stay, also those of a recorded commit")
		for _, intent := range kept {
			require.True(t, strings.Compare(intent.Original, landOID(10000+3*n/4)) >= 0)
		}

		// Every land is still answered as before the compaction, also by a new process.
		forgetLandsIndex(t, repoPath)
		for i, land := range lands {
			records := lookupAll(t, repoPath, "refs/heads/main", land.Original)
			switch {
			case i < 3*n/4:
				require.Len(t, records, 1)
				require.Equal(t, land.Landed, records[0].Landed)
				require.NotZero(t, records[0].Sequence)
			case i < 3*n/4+n/8:
				require.Equal(t, []LandRecord{{Sequence: records[0].Sequence, Landed: landOID(30000 + i)}, {Landed: land.Landed}}, records)
			default:
				require.Equal(t, []LandRecord{{Landed: land.Landed}}, records)
			}
		}
	})

	t.Run("intents that must stay are not rewritten again", func(t *testing.T) {
		// Review m4: after a compaction, also in a new process, the intents that stay are not
		// rewritten while too few of them may go.
		repoPath := t.TempDir()
		const n = 2000
		recordAndConfirm(t, repoPath, n, n/2, 0)
		lookupAll(t, repoPath, "refs/heads/main", landOID(10000))
		require.Len(t, intentsOf(t, repoPath), n/2)
		require.Greater(t, fileSize(t, intentsPath(repoPath)), int64(compactIntentsMinSize))

		compacted, err := os.Stat(intentsPath(repoPath))
		require.NoError(t, err)
		lookupAll(t, repoPath, "refs/heads/main", landOID(10000))
		forgetLandsIndex(t, repoPath)
		lookupAll(t, repoPath, "refs/heads/main", landOID(10000))
		again, err := os.Stat(intentsPath(repoPath))
		require.NoError(t, err)
		require.True(t, os.SameFile(compacted, again))
	})

	t.Run("the intents of lands that never look up are compacted too", func(t *testing.T) {
		// Review m4: fast-forward lands never look up an earlier land. The append asks for the
		// compaction once the file grew, and CompactLandIntents does it.
		repoPath := t.TempDir()
		const n = 1000
		var due bool
		for i := 0; i < n; i++ {
			land := Land{Original: landOID(10000 + i), Landed: landOID(10000 + i)}
			compact, err := RecordLandIntents(repoPath, "refs/heads/main", invocationOf(land.Original), []Land{land})
			require.NoError(t, err)
			confirmLand(t, repoPath, "refs/heads/main", land.Original, land.Landed, invocationOf(land.Original))
			if compact {
				due = true
				require.NoError(t, CompactLandIntents(repoPath))
			}
		}

		require.True(t, due, "the append asked for a compaction")
		require.Less(t, fileSize(t, intentsPath(repoPath)), int64(compactIntentsMinSize))
	})
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
