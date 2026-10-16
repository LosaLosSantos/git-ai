package derived

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// BlameLine attributes one line of a file to the commit that introduced it.
type BlameLine struct {
	// LineNumber is the 1-based line number in the blamed file.
	LineNumber int32 `json:"line_number"`
	// CommitID is the commit that introduced the line in its current form.
	CommitID string `json:"commit_id"`
	// OriginalLineNumber is the line's 1-based number in CommitID's version of the file.
	OriginalLineNumber int32 `json:"original_line_number"`
	// Content is the line without its trailing newline.
	Content []byte `json:"content"`
}

// porcelainHeader matches the header line that starts every blame group and
// every line of porcelain output: "<object id> <original line> <final line>",
// optionally followed by the number of lines in the group.
var porcelainHeader = regexp.MustCompile(`\A([0-9a-f]{40}|[0-9a-f]{64}) ([0-9]+) ([0-9]+)(?: [0-9]+)?\z`)

// ParsePorcelain parses the output of `git blame --porcelain`. Header lines
// that carry commit metadata, which Git only prints the first time a commit
// occurs, are skipped. The result is sorted by line number.
func ParsePorcelain(data []byte) ([]BlameLine, error) {
	var lines []BlameLine

	var (
		haveHeader bool
		header     BlameLine
	)

	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}

		if len(line) > 0 && line[0] == '\t' {
			if !haveHeader {
				return nil, errors.New("blame content line without a header")
			}

			header.Content = append([]byte{}, line[1:]...)
			lines = append(lines, header)
			haveHeader = false
			continue
		}

		matches := porcelainHeader.FindSubmatch(line)
		if matches == nil {
			continue
		}

		original, err := strconv.ParseInt(string(matches[2]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parsing original line number: %w", err)
		}
		final, err := strconv.ParseInt(string(matches[3]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parsing final line number: %w", err)
		}

		header = BlameLine{
			LineNumber:         int32(final),
			CommitID:           string(matches[1]),
			OriginalLineNumber: int32(original),
		}
		haveHeader = true
	}

	sort.SliceStable(lines, func(i, j int) bool {
		return lines[i].LineNumber < lines[j].LineNumber
	})

	return lines, nil
}

// Blame returns the blame of path at commit. commit is expected to be the blame
// origin of path (see BlameOrigin), so that all revisions sharing that origin
// share the cached result, and it is only correct for an origin: git blame at any
// commit equals git blame at its origin. The result is read from the store when
// present, and otherwise computed with git blame and stored.
func (d *Deriver) Blame(ctx context.Context, commit git.ObjectID, path string) ([]BlameLine, error) {
	path = normalizePath(path)

	return cachedJSON(ctx, d.store, d.logger, kindBlame, Key(kindBlame, commit, path), func(ctx context.Context) ([]BlameLine, error) {
		d.blameRuns.Add(1)
		return d.runBlame(ctx, commit, path)
	})
}

// runBlame runs git blame --porcelain for path at commit and parses its output.
func (d *Deriver) runBlame(ctx context.Context, commit git.ObjectID, path string) ([]BlameLine, error) {
	var stdout, stderr bytes.Buffer
	cmd, err := d.repo.Exec(ctx, gitcmd.Command{
		Name:        "blame",
		Flags:       []gitcmd.Option{gitcmd.Flag{Name: "--porcelain"}},
		Args:        []string{commit.String()},
		PostSepArgs: []string{path},
	}, gitcmd.WithStdout(&stdout), gitcmd.WithStderr(&stderr))
	if err != nil {
		return nil, fmt.Errorf("starting blame: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return nil, structerr.New("blaming file: %w", err).WithMetadata("stderr", stderr.String())
	}

	lines, err := ParsePorcelain(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("parsing blame: %w", err)
	}

	return lines, nil
}
