package search

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
)

// grepCommand returns the git grep invocation that lists matching lines of the given commit.
//
// Context lines are not requested from git grep: with --null, git separates the file name, the
// line number and the line with NUL bytes for match and context lines alike, so the two cannot be
// told apart in the output. Context is instead read from the blobs by contextReader.
func grepCommand(commitID, query string, regex, caseSensitive bool, paths []string) gitcmd.Command {
	flags := []gitcmd.Option{
		gitcmd.Flag{Name: "--null"},
		gitcmd.Flag{Name: "--line-number"},
		gitcmd.Flag{Name: "--full-name"},
		gitcmd.Flag{Name: "--no-color"},
		gitcmd.Flag{Name: "-I"},
	}

	if regex {
		flags = append(flags, gitcmd.Flag{Name: "--extended-regexp"})
	} else {
		flags = append(flags, gitcmd.Flag{Name: "--fixed-strings"})
	}

	if !caseSensitive {
		flags = append(flags, gitcmd.Flag{Name: "--ignore-case"})
	}

	flags = append(flags, gitcmd.ValueFlag{Name: "-e", Value: query})

	return gitcmd.Command{
		Name:        "grep",
		Flags:       flags,
		Args:        []string{commitID},
		PostSepArgs: paths,
	}
}

// grepRecord is one matching line reported by git grep.
type grepRecord struct {
	path       string
	lineNumber int
	text       []byte
}

// readGrepRecord reads one record in the form "<commit>:<path>\0<line number>\0<line>\n" as
// produced by `git grep --null --line-number`. It returns io.EOF once the output is exhausted.
func readGrepRecord(r *bufio.Reader) (grepRecord, error) {
	name, err := r.ReadString(0)
	if errors.Is(err, io.EOF) && name == "" {
		return grepRecord{}, io.EOF
	}
	if err != nil {
		return grepRecord{}, fmt.Errorf("reading file name: %w", unexpectedEOF(err))
	}

	// The commit ID is hexadecimal and therefore never contains a colon, so the first colon
	// separates it from the path. The path itself may contain any byte except NUL.
	_, filePath, ok := strings.Cut(strings.TrimSuffix(name, "\x00"), ":")
	if !ok {
		return grepRecord{}, fmt.Errorf("malformed grep record name %q", name)
	}

	lineField, err := r.ReadString(0)
	if err != nil {
		return grepRecord{}, fmt.Errorf("reading line number: %w", unexpectedEOF(err))
	}
	lineNumber, err := strconv.Atoi(strings.TrimSuffix(lineField, "\x00"))
	if err != nil {
		return grepRecord{}, fmt.Errorf("parsing line number %q: %w", lineField, err)
	}

	text, err := readCappedLine(r, maxLineBytes)
	if err != nil {
		return grepRecord{}, fmt.Errorf("reading line: %w", unexpectedEOF(err))
	}

	return grepRecord{
		path:       filePath,
		lineNumber: lineNumber,
		text:       text,
	}, nil
}

// readCappedLine reads the rest of the current line, up to and including its newline, and
// returns it without the newline, cut by truncateLine to at most limit bytes. The part of an
// overlong line beyond the limit is discarded as it is read, so that a single huge line, as in
// a minified or generated file, never has to be held in memory. A line that is not terminated
// by a newline is an error.
func readCappedLine(r *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')

		data := chunk
		if err == nil {
			data = chunk[:len(chunk)-1]
		}
		// One byte beyond the limit is kept so that truncateLine can see where the
		// character at the limit starts.
		if room := limit + 1 - len(line); room > 0 {
			line = append(line, data[:min(room, len(data))]...)
		}

		switch {
		case err == nil:
			if line == nil {
				line = []byte{}
			}
			return truncateLine(line, limit), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

// truncateLine cuts line to at most limit bytes. When the cut would split a UTF-8 encoded
// character, it moves back to the start of that character, so that valid UTF-8 text stays
// valid. Lines of at most limit bytes are returned unchanged.
func truncateLine(line []byte, limit int) []byte {
	if len(line) <= limit {
		return line
	}

	cut := limit
	for i := 0; i < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(line[cut]); i++ {
		cut--
	}
	return line[:cut:cut]
}

func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// contextReader provides the lines surrounding a match. It loads the blobs of the commit lazily,
// one file at a time, because git grep does not report context in an unambiguous format.
type contextReader struct {
	ctx          context.Context
	repo         *localrepo.Repo
	commitID     string
	paths        []string
	contextLines int

	blobs      map[string]git.ObjectID
	haveBlobs  bool
	cachedPath string
	haveCache  bool
	lines      [][]byte
}

func newContextReader(ctx context.Context, repo *localrepo.Repo, commitID string, paths []string, contextLines int) *contextReader {
	return &contextReader{
		ctx:          ctx,
		repo:         repo,
		commitID:     commitID,
		paths:        paths,
		contextLines: contextLines,
	}
}

// surrounding returns the preceding and following lines of the given 1-based line of a file.
func (r *contextReader) surrounding(filePath string, lineNumber int) ([][]byte, [][]byte, error) {
	if r.contextLines == 0 {
		return nil, nil, nil
	}

	if !r.haveCache || r.cachedPath != filePath {
		if err := r.load(filePath); err != nil {
			return nil, nil, err
		}
	}

	if lineNumber < 1 || lineNumber > len(r.lines) {
		return nil, nil, nil
	}

	// Context lines are cut like match lines, so that the size of a match stays bounded.
	var before, after [][]byte
	for i := max(1, lineNumber-r.contextLines); i < lineNumber; i++ {
		before = append(before, truncateLine(r.lines[i-1], maxLineBytes))
	}
	for i := lineNumber + 1; i <= min(len(r.lines), lineNumber+r.contextLines); i++ {
		after = append(after, truncateLine(r.lines[i-1], maxLineBytes))
	}

	return before, after, nil
}

func (r *contextReader) load(filePath string) error {
	if !r.haveBlobs {
		if err := r.loadBlobIDs(); err != nil {
			return err
		}
	}

	r.cachedPath = filePath
	r.haveCache = true
	r.lines = nil

	oid, ok := r.blobs[filePath]
	if !ok {
		return nil
	}

	data, err := r.repo.ReadObject(r.ctx, oid)
	if err != nil {
		return fmt.Errorf("reading blob of %q: %w", filePath, err)
	}

	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	r.lines = lines

	return nil
}

// loadBlobIDs maps the paths of all blobs matched by the search paths to their object IDs.
//
// git grep matches the search paths as pathspecs, with glob patterns, but git ls-tree matches
// them as literal prefixes only. When a search path contains a glob character, the whole tree is
// listed instead, so that every file git grep may report has its blob.
func (r *contextReader) loadBlobIDs() error {
	paths := r.paths
	for _, p := range paths {
		if strings.ContainsAny(p, `*?[\`) {
			paths = nil
			break
		}
	}

	var stdout bytes.Buffer
	if err := r.repo.ExecAndWait(r.ctx,
		gitcmd.Command{
			Name:        "ls-tree",
			Flags:       []gitcmd.Option{gitcmd.Flag{Name: "-r"}, gitcmd.Flag{Name: "-z"}},
			Args:        []string{r.commitID},
			PostSepArgs: paths,
		},
		gitcmd.WithStdout(&stdout),
	); err != nil {
		return fmt.Errorf("listing blobs: %w", err)
	}

	r.blobs = make(map[string]git.ObjectID)
	for _, entry := range bytes.Split(stdout.Bytes(), []byte{0}) {
		meta, filePath, ok := bytes.Cut(entry, []byte("\t"))
		if !ok {
			continue
		}

		fields := strings.Fields(string(meta))
		if len(fields) != 3 || fields[1] != "blob" {
			continue
		}

		r.blobs[string(filePath)] = git.ObjectID(fields[2])
	}
	r.haveBlobs = true

	return nil
}
