package derived

import (
	"bytes"
	"reflect"
	"testing"
)

const (
	fuzzBlameSHA1A = "1111111111111111111111111111111111111111"
	fuzzBlameSHA1B = "2222222222222222222222222222222222222222"
)

// FuzzParsePorcelain fuzzes the parser of the git blame --porcelain output. The
// parser must not panic, must be deterministic, and an accepted output must
// hold lines sorted by number, with valid commit IDs and no newline in content.
func FuzzParsePorcelain(f *testing.F) {
	for _, seed := range []string{
		"",
		fuzzBlameSHA1A + " 1 1 1\nauthor Jane\nauthor-mail <jane@example.com>\nsummary first\nfilename file\n\thello\n",
		fuzzBlameSHA1A + " 1 1 2\nauthor Jane\nfilename file\n\tone\n" +
			fuzzBlameSHA1A + " 2 2\n\ttwo\n" +
			fuzzBlameSHA1B + " 7 3 1\nauthor John\nfilename file\n\tthree\n",
		fuzzBlameSHA1B + " 3 3 1\n\t\n",
		"\tno header\n",
		fuzzBlameSHA1A + " 1 1 1\n\tcontent\r\n",
		fuzzBlameSHA1A + " 99999999999 1 1\n\tx\n",
		"zzzz 1 1 1\n\tx\n",
		fuzzBlameSHA1A + " 1 1 1\n\ttab\there\n",
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		lines, err := ParsePorcelain(data)
		again, errAgain := ParsePorcelain(data)

		if (err == nil) != (errAgain == nil) || (err != nil && err.Error() != errAgain.Error()) {
			t.Fatalf("non-deterministic error: %v != %v", err, errAgain)
		}

		if !reflect.DeepEqual(lines, again) {
			t.Fatal("non-deterministic result")
		}

		if err != nil {
			return
		}

		for i, line := range lines {
			if len(line.CommitID) != 40 && len(line.CommitID) != 64 {
				t.Fatalf("line %d has commit ID %q of length %d", i, line.CommitID, len(line.CommitID))
			}

			if bytes.IndexByte(line.Content, '\n') >= 0 {
				t.Fatalf("line %d content holds a newline: %q", i, line.Content)
			}

			if i > 0 && lines[i-1].LineNumber > line.LineNumber {
				t.Fatalf("lines not sorted by number at %d: %d > %d", i, lines[i-1].LineNumber, line.LineNumber)
			}
		}
	})
}
