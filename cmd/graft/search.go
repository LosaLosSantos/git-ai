package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/feder-cr/open_scion/internal/graft/workspace"
	"github.com/feder-cr/open_scion/sdk"
)

// searchFlags holds the flags of the search command that options does not
// carry. newSearchCommand shares one value between its bind and run functions.
type searchFlags struct {
	regex        bool
	ignoreCase   bool
	paths        pathList
	contextLines int
}

// pathList is a repeatable string flag.
type pathList []string

func (p *pathList) String() string {
	if p == nil {
		return ""
	}
	return strings.Join(*p, ",")
}

func (p *pathList) Set(v string) error {
	*p = append(*p, v)
	return nil
}

// newSearchCommand returns the search command. Its flags live in one searchFlags
// value per command line, so the bind and run functions see the same flags.
func newSearchCommand() command {
	var f searchFlags
	return command{
		bind: func(fs *flag.FlagSet, o *options) { bindSearch(fs, o, &f) },
		run: func(ctx context.Context, out io.Writer, o *options, args []string) error {
			return cmdSearch(ctx, out, o, &f, args)
		},
	}
}

// bindSearch sets the flags of search. -C is the workspace directory of every
// command, so the context lines have the flag --context.
func bindSearch(fs *flag.FlagSet, o *options, f *searchFlags) {
	fs.BoolVar(&f.regex, "regex", false, "treat QUERY as a POSIX extended regular expression")
	fs.BoolVar(&f.ignoreCase, "i", false, "ignore case when matching (default: case sensitive)")
	fs.BoolVar(&f.ignoreCase, "ignore-case", false, "ignore case when matching (same as -i)")
	fs.Var(&f.paths, "path", "limit the search to this path prefix (repeatable)")
	fs.IntVar(&f.contextLines, "context", 0, "print this many lines of context before and after each match")
	fs.IntVar(&o.limit, "n", 0, "maximum number of matches (default: the server's limit)")
	fs.StringVar(&o.revision, "rev", "", "revision to search (default: the workspace branch)")
	fs.BoolVar(&o.jsonOut, "json", false, "print one JSON object per match "+
		"(path, line, text, before, after; when a path or a line is not valid UTF-8, the readable "+
		"field replaces the invalid bytes and path_b64, text_b64, before_b64 or after_b64 carries the exact bytes in base64)")
}

// searchMatchJSON is the --json form of one match. The readable fields are JSON
// strings, in which encoding/json replaces bytes that are not valid UTF-8 with
// U+FFFD. Paths and lines may hold any byte, so whenever a readable field is
// not valid UTF-8, the matching _b64 field carries its exact bytes in base64,
// as the changeset log does for resolved_paths. For before and after, the _b64
// form holds every line, in the same order, when any of them is not valid.
type searchMatchJSON struct {
	Path      string   `json:"path"`
	PathB64   []byte   `json:"path_b64,omitempty"`
	Line      int      `json:"line"`
	Text      string   `json:"text"`
	TextB64   []byte   `json:"text_b64,omitempty"`
	Before    []string `json:"before"`
	BeforeB64 [][]byte `json:"before_b64,omitempty"`
	After     []string `json:"after"`
	AfterB64  [][]byte `json:"after_b64,omitempty"`
}

// cmdSearch searches the files of the workspace's repository, at the workspace
// branch or at --rev, for QUERY. Matches print as path:line:text, like grep.
// Context lines print as path-line-text and groups are separated by "--". A
// search without matches prints nothing and succeeds.
func cmdSearch(ctx context.Context, out io.Writer, o *options, f *searchFlags, args []string) error {
	if len(args) != 1 {
		return usagef("search requires QUERY")
	}
	query := args[0]
	if query == "" {
		return usagef("search requires a non-empty QUERY")
	}
	if o.limit < 0 {
		return usagef("-n must not be negative")
	}
	if f.contextLines < 0 {
		return usagef("--context must not be negative")
	}
	paths := make([]string, 0, len(f.paths))
	for _, raw := range f.paths {
		p, err := workspace.ValidatePath(raw)
		if err != nil {
			return usagef("--path: %v", err)
		}
		paths = append(paths, p)
	}

	backend, meta, err := remoteFor(ctx, o)
	if err != nil {
		return err
	}
	sb, ok := backend.(*sdkBackend)
	if !ok {
		return errors.New("search: the backend cannot search code")
	}

	matches, _, truncated, err := sb.repo.SearchCode(ctx, sdk.SearchOptions{
		Revision:      pick(o.revision, meta.Branch),
		Query:         query,
		Regex:         f.regex,
		CaseSensitive: !f.ignoreCase,
		Paths:         paths,
		MaxResults:    o.limit,
		ContextLines:  f.contextLines,
	})
	if err != nil {
		return mapError(err)
	}

	if o.jsonOut {
		if err := writeSearchJSON(out, matches); err != nil {
			return err
		}
	} else if err := writeSearchText(out, matches, f.contextLines > 0); err != nil {
		return err
	}

	if truncated && o.stderr != nil {
		fmt.Fprintln(o.stderr, "graft search: results truncated; raise -n or narrow the query")
	}
	return nil
}

// writeSearchJSON prints one JSON object per match, with the exact bytes of
// fields that are not valid UTF-8 in their _b64 form.
func writeSearchJSON(out io.Writer, matches []sdk.SearchMatch) error {
	enc := json.NewEncoder(out)
	for _, m := range matches {
		if err := enc.Encode(searchMatchJSON{
			Path:      m.Path,
			PathB64:   exactBytes(m.Path),
			Line:      m.Line,
			Text:      m.Text,
			TextB64:   exactBytes(m.Text),
			Before:    nonNilStrings(m.Before),
			BeforeB64: exactLines(m.Before),
			After:     nonNilStrings(m.After),
			AfterB64:  exactLines(m.After),
		}); err != nil {
			return err
		}
	}
	return nil
}

// exactBytes returns the bytes of s when s is not valid UTF-8, and nil
// otherwise, so that the _b64 field only appears when the readable one is
// lossy.
func exactBytes(s string) []byte {
	if utf8.ValidString(s) {
		return nil
	}
	return []byte(s)
}

// exactLines returns the bytes of every line when any line is not valid UTF-8,
// and nil otherwise. All lines are kept so that the indexes match the readable
// form.
func exactLines(lines []string) [][]byte {
	for _, l := range lines {
		if !utf8.ValidString(l) {
			out := make([][]byte, len(lines))
			for i, line := range lines {
				out[i] = []byte(line)
			}
			return out
		}
	}
	return nil
}

// writeSearchText prints matches like grep. The paths and the text come from
// the repository, so they go through displayText. Context lines already shown
// are not repeated, and a match is never printed as context.
func writeSearchText(out io.Writer, matches []sdk.SearchMatch, withContext bool) error {
	var (
		path    string
		printed int
		started bool
	)
	for i, m := range matches {
		if m.Path != path {
			printed = 0
		}
		first := m.Line - len(m.Before)

		if withContext && started && (m.Path != path || first > printed+1) {
			if _, err := io.WriteString(out, "--\n"); err != nil {
				return err
			}
		}
		for j, line := range m.Before {
			n := first + j
			if n <= printed {
				continue
			}
			if _, err := fmt.Fprintf(out, "%s-%d-%s\n", displayText(m.Path), n, displayText(line)); err != nil {
				return err
			}
		}

		if _, err := fmt.Fprintf(out, "%s:%d:%s\n", displayText(m.Path), m.Line, displayText(m.Text)); err != nil {
			return err
		}
		path, printed, started = m.Path, m.Line, true

		for j, line := range m.After {
			n := m.Line + 1 + j
			if i+1 < len(matches) && matches[i+1].Path == m.Path && n >= matches[i+1].Line {
				break
			}
			if _, err := fmt.Fprintf(out, "%s-%d-%s\n", displayText(m.Path), n, displayText(line)); err != nil {
				return err
			}
			printed = n
		}
	}
	return nil
}

// nonNilStrings returns s, or an empty slice for nil, so that JSON shows [].
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
