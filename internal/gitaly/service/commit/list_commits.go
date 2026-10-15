package commit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gitpipe"
	"github.com/feder-cr/open_scion/internal/git/trailerparser"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/helper/chunk"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// listCommitsPageToken is the JSON-serializable struct embedded in the
// base64-encoded pagination cursor returned by ListCommits.
type listCommitsPageToken struct {
	// CommitID is the object ID of the last commit returned in the page.
	CommitID string `json:"commit_id"`
}

// encodeListCommitsPageToken encodes a commit OID into an opaque base64
// pagination token.
func encodeListCommitsPageToken(commitID string) (string, error) {
	jsonEncoded, err := json.Marshal(listCommitsPageToken{CommitID: commitID})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(jsonEncoded), nil
}

// decodeListCommitsPageToken decodes a pagination token. It supports both the
// new base64-encoded JSON format and the legacy raw commit SHA format for
// backward compatibility during rolling upgrades.
func decodeListCommitsPageToken(token string) string {
	decodedBytes, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		// Not valid base64 — treat as a legacy raw commit SHA.
		return token
	}

	var pt listCommitsPageToken
	if err := json.Unmarshal(decodedBytes, &pt); err != nil {
		// Valid base64 but not our JSON struct — treat as legacy.
		return token
	}

	if pt.CommitID == "" {
		// JSON decoded but had no commit_id — treat as legacy.
		return token
	}

	return pt.CommitID
}

func verifyListCommitsRequest(ctx context.Context, locator storage.Locator, request *gitalypb.ListCommitsRequest) error {
	if err := locator.ValidateRepository(ctx, request.GetRepository()); err != nil {
		return err
	}
	if len(request.GetRevisions()) == 0 {
		return errors.New("missing revisions")
	}
	for _, revision := range request.GetRevisions() {
		if err := git.ValidateRevision([]byte(revision), git.AllowPseudoRevision()); err != nil {
			return structerr.NewInvalidArgument("invalid revision: %w", err).WithMetadata("revision", revision)
		}
	}
	if request.GetFollow() {
		// Git can only follow renames of a single path.
		if len(request.GetPaths()) != 1 {
			return structerr.NewInvalidArgument("follow requires exactly one path")
		}

		// Git detects renames while walking the history in reverse chronological order, so
		// it stops following the path as soon as the order is inverted. Instead of returning
		// a truncated history we refuse the combination.
		if request.GetReverse() {
			return structerr.NewInvalidArgument("follow is incompatible with reverse")
		}

		// Git skips commits while walking the history, so as soon as it skips the commit
		// which renamed the path it loses track of the path and stops returning commits
		// altogether. As we cannot know where the rename happened without walking the
		// history ourselves we refuse the combination and expect callers to skip commits via
		// the pagination token instead.
		if request.GetSkip() > 0 {
			return structerr.NewInvalidArgument("follow is incompatible with skip")
		}
	}

	return nil
}

func (s *server) ListCommits(
	request *gitalypb.ListCommitsRequest,
	stream gitalypb.CommitService_ListCommitsServer,
) error {
	ctx := stream.Context()
	if err := verifyListCommitsRequest(ctx, s.locator, request); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	repo := s.localRepoFactory.Build(request.GetRepository())

	objectReader, cancel, err := s.catfileCache.ObjectReader(ctx, repo)
	if err != nil {
		return structerr.NewInternal("creating object reader: %w", err)
	}
	defer cancel()

	revlistOptions := []gitpipe.RevlistOption{}

	switch request.GetOrder() {
	case gitalypb.ListCommitsRequest_NONE:
		// Nothing to do, we use default sorting.
	case gitalypb.ListCommitsRequest_TOPO:
		revlistOptions = append(revlistOptions, gitpipe.WithOrder(gitpipe.OrderTopo))
	case gitalypb.ListCommitsRequest_DATE:
		revlistOptions = append(revlistOptions, gitpipe.WithOrder(gitpipe.OrderDate))
	}

	if request.GetReverse() {
		revlistOptions = append(revlistOptions, gitpipe.WithReverse())
	}

	if request.GetMaxParents() > 0 {
		revlistOptions = append(revlistOptions, gitpipe.WithMaxParents(uint(request.GetMaxParents())))
	}

	if request.GetDisableWalk() {
		revlistOptions = append(revlistOptions, gitpipe.WithDisabledWalk())
	}

	if request.GetFirstParent() {
		revlistOptions = append(revlistOptions, gitpipe.WithFirstParent())
	}

	if request.GetBefore() != nil {
		revlistOptions = append(revlistOptions, gitpipe.WithBefore(request.GetBefore().AsTime()))
	}

	if request.GetAfter() != nil {
		revlistOptions = append(revlistOptions, gitpipe.WithAfter(request.GetAfter().AsTime()))
	}

	if len(request.GetAuthor()) != 0 {
		revlistOptions = append(revlistOptions, gitpipe.WithAuthor(request.GetAuthor()))
	}

	if request.GetIgnoreCase() {
		revlistOptions = append(revlistOptions, gitpipe.WithIgnoreCase(request.GetIgnoreCase()))
	}

	if request.GetSkip() > 0 {
		revlistOptions = append(revlistOptions, gitpipe.WithSkip(uint(request.GetSkip())))
	}

	if len(request.GetPaths()) > 0 {
		paths := make([]string, len(request.GetPaths()))
		for i, path := range request.GetPaths() {
			paths[i] = string(path)
		}
		revlistOptions = append(revlistOptions, gitpipe.WithPaths(paths...))
	}

	follow := request.GetFollow()
	if follow {
		revlistOptions = append(revlistOptions, gitpipe.WithFollow())
	}

	if fields := log.CustomFieldsFromContext(ctx); fields != nil {
		fields.RecordMetadata("list_commits.follow", follow)
	}

	if opts := gitcmd.ConvertGlobalOptions(request.GetGlobalOptions()); len(opts) > 0 {
		revlistOptions = append(revlistOptions, gitpipe.WithCmdOpts(opts...))
	}

	if len(request.GetCommitMessagePatterns()) > 0 {
		revlistOptions = append(revlistOptions, gitpipe.WithCommitMessagePatterns(request.GetCommitMessagePatterns()))
	}

	if request.GetTrailers() {
		revlistOptions = append(revlistOptions, gitpipe.WithTrailers())
	}

	token := request.GetPaginationParams().GetPageToken()

	// If we've got a pagination token, then we will only start to print commits as soon as
	// we've seen the token.
	if token != "" {
		decodedToken := decodeListCommitsPageToken(token)
		tokenSeen := false
		revlistOptions = append(revlistOptions, gitpipe.WithSkipRevlistResult(func(r *gitpipe.RevisionResult) bool {
			if !tokenSeen {
				tokenSeen = r.OID == git.ObjectID(decodedToken)
				// We also skip the token itself, thus we always return `false`
				// here.
				return true
			}

			return false
		}))
	}

	limit := request.GetPaginationParams().GetLimit()

	// Cap rev-list at limit+1 to avoid walking all of history. We request an
	// extra commit to enable the logic below to detect if pagination is necessary.
	//
	// We don't do this when a pagination token is provided because the limit would
	// be factored in while commits are being skipped via WithSkipRevlistResult.
	// We also don't do this if the history is requested in reverse, because
	// --max-count is applied before --reverse.
	if limit > 0 && token == "" && !request.GetReverse() {
		revlistOptions = append(revlistOptions, gitpipe.WithMaxCount(uint(limit)+1))
	}

	revlistIter := gitpipe.Revlist(ctx, repo, request.GetRevisions(), revlistOptions...)

	catfileObjectIter, err := gitpipe.CatfileObject(ctx, objectReader, revlistIter)
	if err != nil {
		return err
	}

	parser := catfile.NewParser()

	// Track state for pagination cursor detection.
	// We use the "limit + 1" pattern: read one extra commit beyond the limit
	// to detect if there are more results. The cursor is sent in the final
	// response only when hasMoreCommits is true.
	var commitCount int32
	var lastReturnedCommitID string
	var hasMoreCommits bool

	sender := &commitsSender{
		send: func(commits []*gitalypb.GitCommit, cursor *gitalypb.PaginationCursor) error {
			return stream.Send(&gitalypb.ListCommitsResponse{
				Commits:          commits,
				PaginationCursor: cursor,
			})
		},
	}
	chunker := chunk.New(sender)

	for catfileObjectIter.Next() {
		// We've already sent `limit` commits. If we get here, it means
		// there are more commits available, so we set hasMoreCommits and stop.
		if limit > 0 && commitCount >= limit {
			hasMoreCommits = true
			break
		}

		object := catfileObjectIter.Result()

		commit, err := parser.ParseCommit(object)
		if err != nil {
			return structerr.NewInternal("parsing commit: %w", err)
		}

		if request.GetTrailers() {
			commit.GitCommit.Trailers = trailerparser.Parse(object.Trailers)
		}

		// Track the last commit ID for the pagination cursor.
		lastReturnedCommitID = commit.GitCommit.GetId()
		commitCount++

		if err := chunker.Send(commit.GitCommit); err != nil {
			return structerr.NewInternal("sending commit: %w", err)
		}
	}

	if err := catfileObjectIter.Err(); err != nil {
		return structerr.NewInternal("iterating objects: %w", err)
	}

	// Set the pagination cursor before flushing so it's included in the final response.
	// The cursor points to the last commit returned, allowing the client to continue
	// pagination from there.
	if hasMoreCommits && lastReturnedCommitID != "" {
		encodedCursor, err := encodeListCommitsPageToken(lastReturnedCommitID)
		if err != nil {
			return structerr.NewInternal("encoding pagination cursor: %w", err)
		}
		sender.SetPaginationCursor(&gitalypb.PaginationCursor{NextCursor: encodedCursor})
	}

	if err := chunker.Flush(); err != nil {
		return structerr.NewInternal("flushing commits: %w", err)
	}

	return nil
}
