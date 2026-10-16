package sdk

import (
	"context"
	"errors"
	"io"
	"math"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// maxSearchQueryBytes is the longest query SearchCode sends. The server refuses
// longer queries too.
const maxSearchQueryBytes = 1024

// SearchOptions configures Repo.SearchCode.
type SearchOptions struct {
	// Revision is the commit-ish to search at. Required.
	Revision string
	// Query is the text to search for: a fixed string, or a regular expression
	// when Regex is set. Required, at most 1 KiB.
	Query string
	// Regex interprets Query as a POSIX extended regular expression.
	Regex bool
	// CaseSensitive makes matching case sensitive. The default is case
	// insensitive.
	CaseSensitive bool
	// Paths limits the search to these path prefixes. Empty searches the whole
	// tree. Each path must be non-empty.
	Paths []string
	// MaxResults bounds the number of matching lines. Zero selects the server
	// default, and the server caps it at its own maximum.
	MaxResults int
	// ContextLines is the number of lines of context before and after each
	// match. The server caps it.
	ContextLines int
}

// SearchMatch is one line of a file that matches a SearchCode query. Path and
// the lines hold the exact bytes of the repository, which need not be valid
// UTF-8.
type SearchMatch struct {
	// Path is the file path relative to the repository root.
	Path string
	// Line is the 1-based line number of the match.
	Line int
	// Text is the matching line without its trailing newline. The server cuts
	// very long lines (4 KiB, at a character boundary for UTF-8 text), match
	// and context lines alike, so that minified or generated files cannot
	// make a search fail.
	Text string
	// Before holds the context lines before the match, in file order. Empty
	// unless context was requested.
	Before []string
	// After holds the context lines after the match, in file order. Empty
	// unless context was requested.
	After []string
}

// SearchCode searches the files at opts.Revision for lines that match
// opts.Query. It returns the matches sorted by path then line, the full object
// ID of the searched commit, and whether the results were cut short by the
// result limit or the server's time budget. Having no match is not an error.
//
// It fails with InvalidArgument for invalid options or an invalid regular
// expression, and with NotFound when the revision does not name a commit.
func (r *Repo) SearchCode(ctx context.Context, opts SearchOptions) (matches []SearchMatch, commitID string, truncated bool, err error) {
	req, err := opts.request(r.repository())
	if err != nil {
		return nil, "", false, err
	}

	stream, err := gitalypb.NewSearchServiceClient(r.client.conn).SearchCode(ctx, req)
	if err != nil {
		return nil, "", false, err
	}

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return matches, commitID, truncated, nil
		}
		if err != nil {
			return nil, "", false, err
		}

		if commitID == "" {
			commitID = msg.GetCommitId()
		}
		truncated = truncated || msg.GetTruncated()

		for _, m := range msg.GetMatches() {
			matches = append(matches, SearchMatch{
				Path:   string(m.GetPath()),
				Line:   int(m.GetLineNumber()),
				Text:   string(m.GetLine()),
				Before: byteStrings(m.GetPrecedingLines()),
				After:  byteStrings(m.GetFollowingLines()),
			})
		}
	}
}

// request validates opts and builds the SearchCode request for repo.
func (opts SearchOptions) request(repo *gitalypb.Repository) (*gitalypb.SearchCodeRequest, error) {
	if opts.Revision == "" {
		return nil, status.Error(codes.InvalidArgument, "revision is required")
	}
	if opts.Query == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}
	if len(opts.Query) > maxSearchQueryBytes {
		return nil, status.Errorf(codes.InvalidArgument, "query is longer than %d bytes", maxSearchQueryBytes)
	}
	if opts.MaxResults < 0 || opts.MaxResults > math.MaxUint32 {
		return nil, status.Errorf(codes.InvalidArgument, "max results %d out of range", opts.MaxResults)
	}
	if opts.ContextLines < 0 || opts.ContextLines > math.MaxUint32 {
		return nil, status.Errorf(codes.InvalidArgument, "context lines %d out of range", opts.ContextLines)
	}

	paths := make([][]byte, 0, len(opts.Paths))
	for _, p := range opts.Paths {
		if p == "" {
			return nil, status.Error(codes.InvalidArgument, "paths must not be empty")
		}
		paths = append(paths, []byte(p))
	}

	return &gitalypb.SearchCodeRequest{
		Repository:    repo,
		Revision:      []byte(opts.Revision),
		Query:         opts.Query,
		Regex:         opts.Regex,
		CaseSensitive: opts.CaseSensitive,
		Paths:         paths,
		MaxResults:    uint32(opts.MaxResults),
		ContextLines:  uint32(opts.ContextLines),
	}, nil
}

// byteStrings converts protobuf byte strings to Go strings. It returns nil for
// an empty slice.
func byteStrings(in [][]byte) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, b := range in {
		out[i] = string(b)
	}
	return out
}
