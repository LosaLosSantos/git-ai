package search

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/feder-cr/open_scion/internal/command"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"google.golang.org/protobuf/proto"
)

const (
	// maxQueryBytes bounds the size of a search query.
	maxQueryBytes = 1024
	// matchBatchSize is the largest number of matches sent in one SearchCodeResponse.
	matchBatchSize = 100
	// maxBatchBytes bounds the encoded size of the matches of one SearchCodeResponse, well
	// below the 4 MiB that gRPC clients accept by default. A single match larger than this,
	// which only a very long path can produce, is sent in a message of its own.
	maxBatchBytes = 1024 * 1024
	// maxLineBytes bounds the length of the matching line and of each context line of a
	// match. Longer lines, as found in minified or generated files, are cut to this length
	// (at a character boundary when the line is valid UTF-8); the search does not fail.
	maxLineBytes = 4 * 1024
)

// SearchCode streams the lines of files at a revision that match a query. Matches are emitted
// in path order, then line order, in batches of at most matchBatchSize matches and
// maxBatchBytes bytes. Lines longer than maxLineBytes, match and context lines alike, are cut
// to maxLineBytes. The search stops early, and reports truncated, once the result limit or the
// configured time budget is reached.
func (s *server) SearchCode(req *gitalypb.SearchCodeRequest, stream gitalypb.SearchService_SearchCodeServer) error {
	ctx := stream.Context()

	if err := s.deps.GetLocator().ValidateRepository(ctx, req.GetRepository()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	if err := validateSearchQuery(req.GetQuery()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	if err := validateSearchRevision(req.GetRevision()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	paths, err := validateSearchPaths(req.GetPaths())
	if err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	searchCfg := s.deps.GetCfg().Graft.Search

	timeout, err := searchCfg.TimeoutOrDefault()
	if err != nil {
		return structerr.NewInternal("%w", err)
	}

	limit := searchCfg.MaxResultsOrDefault()
	if requested := int(req.GetMaxResults()); requested > 0 && requested < limit {
		limit = requested
	}

	contextLines := int(req.GetContextLines())
	if maxContext := searchCfg.MaxContextLinesOrDefault(); contextLines > maxContext {
		contextLines = maxContext
	}

	repo := s.deps.GetRepositoryFactory().Build(req.GetRepository())

	revision := string(req.GetRevision())
	commitID, err := repo.ResolveRevision(ctx, git.Revision(revision+"^{commit}"))
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return structerr.NewNotFound("revision not found").WithMetadata("revision", revision)
		}
		return structerr.NewInternal("resolving revision: %w", err)
	}

	searchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sender := &matchSender{stream: stream, commitID: commitID.String()}

	var stderr bytes.Buffer
	cmd, err := repo.Exec(searchCtx,
		grepCommand(commitID.String(), req.GetQuery(), req.GetRegex(), req.GetCaseSensitive(), paths),
		gitcmd.WithSetupStdout(),
		gitcmd.WithStderr(&stderr),
	)
	if err != nil {
		if errors.Is(searchCtx.Err(), context.DeadlineExceeded) {
			// The time budget was already spent before git grep could start.
			return sender.send(nil, true)
		}
		return structerr.NewInternal("starting git grep: %w", err)
	}

	var (
		records   = bufio.NewReaderSize(cmd, 64*1024)
		files     = newContextReader(searchCtx, repo, commitID.String(), paths, contextLines)
		pending   []*gitalypb.SearchCodeResponse_Match
		pendingSz int
		count     int
		truncated bool
		readErr   error
	)

	for {
		record, err := readGrepRecord(records)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			readErr = err
			break
		}

		if count == limit {
			// There is at least one more match beyond the limit.
			truncated = true
			break
		}

		match := &gitalypb.SearchCodeResponse_Match{
			Path:       []byte(record.path),
			LineNumber: int32(record.lineNumber),
			Line:       record.text,
		}

		if contextLines > 0 {
			before, after, err := files.surrounding(record.path, record.lineNumber)
			if err != nil {
				readErr = err
				break
			}
			match.PrecedingLines = before
			match.FollowingLines = after
		}

		// Flush the batch first when this match would push it past the byte budget, so that
		// no response exceeds what a client accepts. A match larger than the budget on its
		// own is still sent, in a message of its own.
		size := proto.Size(match)
		if len(pending) > 0 && pendingSz+size > maxBatchBytes {
			if err := sender.send(pending, false); err != nil {
				cancel()
				_ = cmd.Wait()
				return err
			}
			pending, pendingSz = nil, 0
		}

		pending = append(pending, match)
		pendingSz += size
		count++

		if len(pending) == matchBatchSize {
			if err := sender.send(pending, false); err != nil {
				cancel()
				_ = cmd.Wait()
				return err
			}
			pending, pendingSz = nil, 0
		}
	}

	timedOut := errors.Is(searchCtx.Err(), context.DeadlineExceeded)
	if truncated || readErr != nil {
		// Stop git grep early: nothing more is needed, or its output can no longer be read.
		cancel()
	}

	waitErr := cmd.Wait()
	if err := ctx.Err(); err != nil {
		return structerr.NewCanceled("search canceled: %w", err)
	}

	switch {
	case truncated:
		// The result limit was reached; git grep was stopped on purpose.
	case timedOut && (waitErr != nil || readErr != nil):
		// The time budget ran out before the search completed: report what was found so far.
		truncated = true
	case readErr != nil:
		return structerr.NewInternal("reading git grep output: %w", readErr).
			WithMetadata("stderr", stderr.String())
	case waitErr != nil:
		status, ok := command.ExitStatus(waitErr)
		switch {
		case ok && status == 1:
			// git grep exits with 1 when nothing matched.
		case ok && status == 128 && req.GetRegex():
			return structerr.NewInvalidArgument("invalid regular expression").
				WithMetadata("stderr", stderr.String())
		default:
			return structerr.NewInternal("git grep: %w", waitErr).
				WithMetadata("stderr", stderr.String())
		}
	}

	return sender.send(pending, truncated)
}

// matchSender sends SearchCodeResponse messages. The commit ID is only set on the first message.
type matchSender struct {
	stream   gitalypb.SearchService_SearchCodeServer
	commitID string
	sent     bool
}

func (m *matchSender) send(matches []*gitalypb.SearchCodeResponse_Match, truncated bool) error {
	response := &gitalypb.SearchCodeResponse{
		Matches:   matches,
		Truncated: truncated,
	}
	if !m.sent {
		response.CommitId = m.commitID
		m.sent = true
	}

	if err := m.stream.Send(response); err != nil {
		return structerr.NewInternal("sending search results: %w", err)
	}
	return nil
}

func validateSearchQuery(query string) error {
	if query == "" {
		return errors.New("empty query")
	}
	if len(query) > maxQueryBytes {
		return fmt.Errorf("query exceeds %d bytes", maxQueryBytes)
	}
	if strings.ContainsRune(query, 0) {
		return errors.New("query contains NUL byte")
	}
	// git grep splits a -e pattern at newlines into alternative patterns, so a
	// multi-line query would match lines that contain any one of its lines.
	// Matches are single lines, so such a query can never match as written.
	if strings.ContainsAny(query, "\r\n") {
		return errors.New("query contains a line break")
	}
	return nil
}

func validateSearchRevision(revision []byte) error {
	if err := git.ValidateRevision(revision); err != nil {
		return fmt.Errorf("invalid revision: %w", err)
	}
	if bytes.Contains(revision, []byte("..")) {
		return errors.New("revision must name a single commit")
	}
	return nil
}

func validateSearchPaths(rawPaths [][]byte) ([]string, error) {
	paths := make([]string, 0, len(rawPaths))

	for _, rawPath := range rawPaths {
		p := string(rawPath)

		switch {
		case p == "":
			return nil, errors.New("empty path")
		case strings.ContainsRune(p, 0):
			return nil, fmt.Errorf("path %q contains NUL byte", p)
		case strings.HasPrefix(p, "/"):
			return nil, fmt.Errorf("path %q must be relative", p)
		case strings.HasPrefix(p, ":"):
			// A leading colon would be interpreted by git as pathspec magic.
			return nil, fmt.Errorf("path %q must not start with a colon", p)
		case path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/.."):
			return nil, fmt.Errorf("path %q is not clean", p)
		}

		paths = append(paths, p)
	}

	return paths, nil
}
