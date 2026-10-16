package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// DefaultMaxBisectCommits bounds the first-parent range that one bisection
// searches, and the commits that are listed to count the reverts. A longer
// bisection range is refused instead of being listed in full; the revert history
// is cut to the newest commits instead (see CommitsSince).
const DefaultMaxBisectCommits = 10000

// CommitLister implements CommitSource and RevertHistory with the ListCommits
// RPC of the CommitService. It lists first-parent commits, so that merge commits
// appear as the single change they are on the branch.
type CommitLister struct {
	Commits    gitalypb.CommitServiceClient
	Repository *gitalypb.Repository
	// MaxCommits bounds the commits listed by one call. Zero selects
	// DefaultMaxBisectCommits.
	MaxCommits int
}

var (
	_ CommitSource  = (*CommitLister)(nil)
	_ RevertHistory = (*CommitLister)(nil)
)

// FirstParentRange implements CommitSource. It returns the commits reachable
// from tip along first parents and not from base, oldest first. A range of more
// than MaxCommits commits is an error.
func (l *CommitLister) FirstParentRange(ctx context.Context, base, tip string) ([]CommitInfo, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	if base == "" || tip == "" {
		return nil, errors.New("verify: base and tip are required")
	}
	limit := l.limit()
	commits, more, err := l.list(ctx, &gitalypb.ListCommitsRequest{
		Repository: l.Repository,
		// "^base" excludes everything reachable from the green baseline.
		Revisions:   []string{"^" + base, tip},
		FirstParent: true,
		// Topological order lists children before parents, so reversing the
		// list gives oldest first.
		Order: gitalypb.ListCommitsRequest_TOPO,
	}, limit)
	if err != nil {
		return nil, err
	}
	if more {
		return nil, fmt.Errorf("verify: more than %d commits in the listed range", limit)
	}
	return commits, nil
}

// CommitsSince implements RevertHistory. It returns the first-parent commits of
// branch whose committer date is at or after since, or unknown, oldest first.
//
// The window is applied here and not by the server: ListCommits with an After
// bound stops walking at the first commit older than it, so a tip with an old
// committer date (a fast-forward keeps the date of the original commit) would
// hide every revert below it from the cap. The newest MaxCommits first-parent
// commits are listed instead, and the server stops its walk there.
//
// Reaching that bound is not an error, so that a long history does not turn
// the cap, and with it every revert, off for good. The automatic reverts are
// created by the server and carry its date, so a revert of the window is among
// the newest commits unless more than MaxCommits commits landed after it. When
// the oldest listed commit is dated inside the window, that many commits did
// land within the window, so the window is not covered and CommitsSince returns
// an error, which makes the cap refuse until the burst leaves the window or the
// next land moves the bound. An undated commit at the bound proves nothing and
// is taken as the window being covered, so that it cannot keep the cap closed.
// The residual: more than MaxCommits commits with old committer dates, placed on
// the branch after a revert of the window, hide that revert.
func (l *CommitLister) CommitsSince(ctx context.Context, branch string, since time.Time) ([]CommitInfo, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	if branch == "" {
		return nil, errors.New("verify: branch is required")
	}
	limit := l.limit()
	commits, more, err := l.list(ctx, &gitalypb.ListCommitsRequest{
		Repository:  l.Repository,
		Revisions:   []string{"refs/heads/" + branch},
		FirstParent: true,
		// Newest first, so that the bound keeps the newest commits.
		Order: gitalypb.ListCommitsRequest_TOPO,
	}, limit)
	if err != nil {
		return nil, err
	}
	if more && len(commits) > 0 && !commits[0].CommittedAt.IsZero() && inWindow(commits[0], since) {
		return nil, fmt.Errorf("verify: more than %d commits since %s", limit, since.UTC().Format(time.RFC3339))
	}

	recent := commits[:0]
	for _, c := range commits {
		if inWindow(c, since) {
			recent = append(recent, c)
		}
	}
	return recent, nil
}

// inWindow reports whether c is committed at or after since. A commit whose
// date is unknown is taken as inside the window.
func inWindow(c CommitInfo, since time.Time) bool {
	return c.CommittedAt.IsZero() || !c.CommittedAt.Before(since)
}

func (l *CommitLister) check() error {
	if l.Commits == nil || l.Repository == nil {
		return errors.New("verify: commit lister is not configured")
	}
	return nil
}

func (l *CommitLister) limit() int {
	if l.MaxCommits <= 0 {
		return DefaultMaxBisectCommits
	}
	// The server takes the bound plus one as an int32.
	return min(l.MaxCommits, math.MaxInt32-1)
}

// list streams the commits of req in the server's order, children first, and
// returns the first limit of them oldest first. more reports that the server
// had more commits. The server is asked for one commit beyond limit, so that it
// stops its walk there, and the stream is cancelled when list returns, so that
// an early return does not leave it open.
func (l *CommitLister) list(ctx context.Context, req *gitalypb.ListCommitsRequest, limit int) (_ []CommitInfo, more bool, _ error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req.PaginationParams = &gitalypb.PaginationParameter{Limit: int32(limit + 1)}
	stream, err := l.Commits.ListCommits(ctx, req)
	if err != nil {
		return nil, false, fmt.Errorf("list commits: %w", err)
	}

	var commits []CommitInfo
receive:
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, fmt.Errorf("receive commits: %w", err)
		}
		for _, c := range resp.GetCommits() {
			if len(commits) >= limit {
				more = true
				break receive
			}
			commits = append(commits, commitInfo(c))
		}
	}

	for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
		commits[i], commits[j] = commits[j], commits[i]
	}
	return commits, more, nil
}

// commitInfo converts a commit of the server.
func commitInfo(c *gitalypb.GitCommit) CommitInfo {
	subject := string(c.GetSubject())
	info := CommitInfo{
		ID:      c.GetId(),
		Subject: subject,
		Message: commitMessage(subject, string(c.GetBody())),
		// The server keeps only the first MaxCommitOrTagMessageSize bytes of the
		// body and reports the full size in BodySize.
		Truncated: c.GetBodySize() > int64(len(c.GetBody())),
	}
	if committer := c.GetCommitter(); committer != nil {
		info.CommitterEmail = string(committer.GetEmail())
		if date := committer.GetDate(); date != nil {
			info.CommittedAt = date.AsTime()
		}
	}
	return info
}

// commitMessage rebuilds the full message from its subject and body. The body
// may already start with the subject, as the server can return the full message;
// then it is used as it is.
func commitMessage(subject, body string) string {
	switch {
	case body == "":
		return subject
	case strings.HasPrefix(body, subject):
		return body
	default:
		return subject + "\n\n" + body
	}
}
