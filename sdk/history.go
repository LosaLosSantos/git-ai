package sdk

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// Commit is a commit as returned by PathHistory.
type Commit = gitalypb.GitCommit

// BlameLine is one line of a file attributed to the commit that last
// changed it.
type BlameLine struct {
	// LineNumber is the 1-based line number in the blamed revision.
	LineNumber int32
	// CommitID is the commit that introduced the line in its current form.
	CommitID string
	// OriginalLineNumber is the line number in CommitID's version of the file.
	OriginalLineNumber int32
	// Content is the line without its trailing newline.
	Content []byte
}

// BlameResult is the blame of a file at a revision.
type BlameResult struct {
	// CommitID is the commit that the revision resolved to.
	CommitID string
	// Lines are the attributed lines in line order.
	Lines []BlameLine
}

// Blame attributes every line of the file at path in rev to the commit that
// last changed it. Results come from derived data computed on the server and
// are reused by later queries.
func (r *Repo) Blame(ctx context.Context, rev, path string) (*BlameResult, error) {
	if rev == "" || path == "" {
		return nil, status.Error(codes.InvalidArgument, "revision and path are required")
	}

	stream, err := r.client.derived.Blame(ctx, &gitalypb.BlameRequest{
		Repository: r.repository(),
		Revision:   []byte(rev),
		Path:       []byte(path),
	})
	if err != nil {
		return nil, err
	}

	result := &BlameResult{}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}

		if msg.GetCommitId() != "" {
			result.CommitID = msg.GetCommitId()
		}
		for _, line := range msg.GetLines() {
			result.Lines = append(result.Lines, BlameLine{
				LineNumber:         line.GetLineNumber(),
				CommitID:           line.GetCommitId(),
				OriginalLineNumber: line.GetOriginalLineNumber(),
				Content:            line.GetContent(),
			})
		}
	}
}

// PathHistory returns the commits that changed the file or directory at path,
// starting at rev and following first parents, newest first. At most limit
// commits are returned; zero means the server default of 100.
func (r *Repo) PathHistory(ctx context.Context, rev, path string, limit uint32) ([]*Commit, error) {
	if rev == "" || path == "" {
		return nil, status.Error(codes.InvalidArgument, "revision and path are required")
	}

	stream, err := r.client.derived.PathHistory(ctx, &gitalypb.PathHistoryRequest{
		Repository: r.repository(),
		Revision:   []byte(rev),
		Path:       []byte(path),
		Limit:      limit,
	})
	if err != nil {
		return nil, err
	}

	var commits []*Commit
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return commits, nil
		}
		if err != nil {
			return nil, err
		}

		commits = append(commits, msg.GetCommits()...)
	}
}
