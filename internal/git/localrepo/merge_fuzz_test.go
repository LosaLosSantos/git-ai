package localrepo

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gittest"
)

// FuzzParseMergeTreeError fuzzes the parser of the git-merge-tree(1) conflict
// output. The parser must never panic, whatever the output, and must be
// deterministic: parsing the same output twice gives the same result. A
// conflict error that is accepted must also be reproduced by an independent
// walk of the output: every conflicting file and every information message
// comes from the input, and nothing is dropped or invented.
func FuzzParseMergeTreeError(f *testing.F) {
	emptyTree := gittest.DefaultObjectHash.EmptyTreeOID.String()

	// Seeds: the well-formed and malformed outputs of merge_test.go.
	seeds := []struct {
		output string
		names  bool
	}{
		{
			output: strings.Join([]string{
				emptyTree,
				fmt.Sprintf("100644 %s 1\ta", emptyTree),
				fmt.Sprintf("100644 %s 2\ta", emptyTree),
				fmt.Sprintf("100644 %s 3\ta", emptyTree),
				"",
				"1",
				"a",
				"Auto-merging",
				"Auto-merging a\n",
				"",
			}, "\x00"),
		},
		{
			output: strings.Join([]string{
				emptyTree,
				"",
				"1",
				"/foo",
				"CONFLICT(directory rename unclear split)",
				"CONFLICT (directory rename split): Unclear where to rename /foo to.",
				"",
			}, "\x00"),
		},
		{
			output: strings.Join([]string{
				emptyTree,
				"a",
				"b",
				"",
				"1",
				"a",
				"Auto-merging a\n",
				"",
			}, "\x00"),
			names: true,
		},
		{output: emptyTree + "\x00\x00"},
		{output: ""},
		{output: "\x00\x00"},
		{output: "not an oid\x00\x00"},
		{output: emptyTree + "\x00\x00" + "1\x00a\x00Auto-merging a\n\x00"},
		// Path counts at and around the limits of int. Adding the count to the
		// position of the field used to overflow and pass the bounds check.
		{output: emptyTree + "\x00\x00" + "9223372036854775807\x00a\x00b\x00c\x00"},
		{output: emptyTree + "\x00\x00" + "9223372036854775806\x00a\x00b\x00c\x00"},
		{output: emptyTree + "\x00\x00" + "-3\x00a\x00b\x00c\x00"},
		{output: emptyTree + "\x00\x00" + "2\x00a\x00b\x00c\x00", names: true},
	}

	for _, seed := range seeds {
		f.Add(seed.output, seed.names)
	}

	f.Fuzz(func(t *testing.T, output string, conflictingFileNamesOnly bool) {
		cfg := mergeTreeConfig{conflictingFileNamesOnly: conflictingFileNamesOnly}

		oid1, err1 := parseMergeTreeError(gittest.DefaultObjectHash, cfg, output)
		oid2, err2 := parseMergeTreeError(gittest.DefaultObjectHash, cfg, output)

		if oid1 != oid2 {
			t.Fatalf("non-deterministic object ID: %q != %q", oid1, oid2)
		}

		if !reflect.DeepEqual(err1, err2) {
			t.Fatalf("non-deterministic error: %v != %v", err1, err2)
		}

		if conflictErr, ok := err1.(*MergeTreeConflictError); ok {
			checkAcceptedMergeTreeError(t, output, cfg, string(oid1), conflictErr)
		}
	})
}

// checkAcceptedMergeTreeError checks the invariants of an accepted conflict
// output against an independent walk of the same output.
func checkAcceptedMergeTreeError(t *testing.T, output string, cfg mergeTreeConfig, oid string, conflictErr *MergeTreeConflictError) {
	t.Helper()

	oidAndConflictsBuf, infoMsg, ok := strings.Cut(output, "\x00\x00")
	if !ok {
		t.Fatalf("accepted output without a separator: %q", output)
	}

	oidAndConflicts := strings.Split(oidAndConflictsBuf, "\x00")
	if !strings.EqualFold(oid, oidAndConflicts[0]) {
		t.Fatalf("object ID %q is not the first field %q", oid, oidAndConflicts[0])
	}

	// One conflicting file per line after the object ID. The section is absent
	// when there is no line at all.
	conflicts := oidAndConflicts[1:]
	if len(conflicts) != len(conflictErr.ConflictingFileInfo) {
		t.Fatalf("got %d conflicting files for %d lines", len(conflictErr.ConflictingFileInfo), len(conflicts))
	}

	for i, info := range conflictErr.ConflictingFileInfo {
		line := conflicts[i]

		if cfg.conflictingFileNamesOnly {
			if info.FileName != line {
				t.Fatalf("file name %q is not the line %q", info.FileName, line)
			}

			continue
		}

		// The line is "<mode> <object> <stage>\t<filename>".
		if !strings.HasSuffix(line, "\t"+info.FileName) {
			t.Fatalf("file name %q is not in the line %q", info.FileName, line)
		}
	}

	// Walk the information messages with the same grammar as the output, and
	// refuse any count that does not fit in the remaining fields.
	fields := strings.Split(infoMsg, "\x00")
	fields = fields[:len(fields)-1]

	i := 0
	for k, msg := range conflictErr.ConflictInfoMessage {
		if i >= len(fields) {
			t.Fatalf("message %d has no path count in %d fields", k, len(fields))
		}

		numOfPaths, err := strconv.Atoi(fields[i])
		if err != nil || numOfPaths < 0 || numOfPaths > len(fields)-i-3 {
			t.Fatalf("message %d does not match the fields at %d: %v", k, i, err)
		}

		if want := fields[i+1 : i+numOfPaths+1]; !reflect.DeepEqual(msg.Paths, want) {
			t.Fatalf("message %d paths %q, want %q", k, msg.Paths, want)
		}

		if want := fields[i+numOfPaths+1]; msg.Type != want {
			t.Fatalf("message %d type %q, want %q", k, msg.Type, want)
		}

		if want := fields[i+numOfPaths+2]; msg.Message != want {
			t.Fatalf("message %d message %q, want %q", k, msg.Message, want)
		}

		i += numOfPaths + 3
	}

	if i != len(fields) {
		t.Fatalf("messages cover %d of %d fields", i, len(fields))
	}
}
