package derived

import (
	"errors"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// Blame attributes every line of a file at a revision to the commit that last
// changed it. The blame is computed once per last-modified commit of the file
// and then shared by every revision that has the same version of the file.
func (s *server) Blame(in *gitalypb.BlameRequest, stream gitalypb.DerivedDataService_BlameServer) error {
	ctx := stream.Context()

	if err := validateRevisionAndPath(ctx, s.deps.GetLocator(), in.GetRepository(), in.GetRevision(), in.GetPath()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	deriver, cancel, err := s.newDeriver(ctx, in.GetRepository())
	if err != nil {
		return err
	}
	defer cancel()

	commitID, err := resolveCommit(ctx, deriver, in.GetRevision())
	if err != nil {
		return err
	}

	path := string(in.GetPath())

	entry, err := deriver.TreeEntry(ctx, commitID, path)
	if err != nil {
		return structerr.NewInternal("%w", err)
	}
	if entry == nil {
		return structerr.NewNotFound("path not found in revision").
			WithMetadata("path", path).
			WithMetadata("revision", string(in.GetRevision()))
	}
	if entry.GetType() != gitalypb.TreeEntry_BLOB {
		return structerr.NewInvalidArgument("path is not a file").
			WithMetadata("path", path).
			WithMetadata("revision", string(in.GetRevision()))
	}

	// The blame is computed at the origin, which is the commit git blame attributes the
	// file to. Revisions sharing that origin share the cached blame.
	origin, err := deriver.BlameOrigin(ctx, commitID, path)
	if err != nil {
		return structerr.NewInternal("%w", err)
	}
	if origin == "" {
		return structerr.NewInternal("%w", errors.New("file exists but has no blame origin"))
	}

	lines, err := deriver.Blame(ctx, origin, path)
	if err != nil {
		return structerr.NewInternal("%w", err)
	}

	return sendBlame(stream, commitID, lines)
}

// sendBlame streams the lines in batches. The first message carries the resolved
// commit, and at least one message is always sent, even for an empty file.
func sendBlame(stream gitalypb.DerivedDataService_BlameServer, commitID git.ObjectID, lines []derived.BlameLine) error {
	sent := false
	for start := 0; start < len(lines) || !sent; start += blameBatchSize {
		end := min(start+blameBatchSize, len(lines))

		response := &gitalypb.BlameResponse{Lines: make([]*gitalypb.BlameResponse_Line, 0, end-start)}
		if !sent {
			response.CommitId = commitID.String()
			sent = true
		}

		for _, line := range lines[start:end] {
			response.Lines = append(response.Lines, &gitalypb.BlameResponse_Line{
				LineNumber:         line.LineNumber,
				CommitId:           line.CommitID,
				OriginalLineNumber: line.OriginalLineNumber,
				Content:            line.Content,
			})
		}

		if err := stream.Send(response); err != nil {
			return err
		}
	}

	return nil
}
