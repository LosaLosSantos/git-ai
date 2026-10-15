package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// fakeCommitStream returns the scripted pages, then err, then io.EOF.
type fakeCommitStream struct {
	grpc.ClientStream

	pages []*gitalypb.ListCommitsResponse
	err   error
}

func (s *fakeCommitStream) Recv() (*gitalypb.ListCommitsResponse, error) {
	if len(s.pages) > 0 {
		page := s.pages[0]
		s.pages = s.pages[1:]
		return page, nil
	}
	if s.err != nil {
		err := s.err
		s.err = nil
		return nil, err
	}
	return nil, io.EOF
}

// fakeCommitService answers ListCommits with stream and records the request.
type fakeCommitService struct {
	gitalypb.CommitServiceClient

	stream grpc.ServerStreamingClient[gitalypb.ListCommitsResponse]
	err    error
	got    *gitalypb.ListCommitsRequest
	// ctx is the context of the last call, to check that the stream is
	// cancelled once the lister returns.
	ctx context.Context
}

func (f *fakeCommitService) ListCommits(ctx context.Context, in *gitalypb.ListCommitsRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[gitalypb.ListCommitsResponse], error) {
	f.got = in
	f.ctx = ctx
	if f.err != nil {
		return nil, f.err
	}
	return f.stream, nil
}

func gitCommit(id, subject, body string) *gitalypb.GitCommit {
	return &gitalypb.GitCommit{Id: id, Subject: []byte(subject), Body: []byte(body)}
}

func TestCommitLister_FirstParentRange(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "x.git"}
	const base, tip = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"

	t.Run("requests the first-parent range topologically and returns it oldest first", func(t *testing.T) {
		// The server answers children before parents, in two pages.
		svc := &fakeCommitService{stream: &fakeCommitStream{pages: []*gitalypb.ListCommitsResponse{
			{Commits: []*gitalypb.GitCommit{gitCommit(tip, "third", "")}},
			{Commits: []*gitalypb.GitCommit{gitCommit("bbb", "second", "details\n\nGraft-Resolved-By: x\n")}},
			{Commits: []*gitalypb.GitCommit{gitCommit("aaa", "first", "")}},
		}}}
		l := &CommitLister{Commits: svc, Repository: repo}

		commits, err := l.FirstParentRange(ctx, base, tip)
		require.NoError(t, err)

		require.Equal(t, repo, svc.got.GetRepository())
		require.Equal(t, []string{"^" + base, tip}, svc.got.GetRevisions())
		require.True(t, svc.got.GetFirstParent())
		require.Equal(t, gitalypb.ListCommitsRequest_TOPO, svc.got.GetOrder())

		require.Equal(t, []CommitInfo{
			{ID: "aaa", Subject: "first", Message: "first"},
			{ID: "bbb", Subject: "second", Message: "second\n\ndetails\n\nGraft-Resolved-By: x\n"},
			{ID: tip, Subject: "third", Message: "third"},
		}, commits)
	})

	t.Run("body that already starts with the subject is the full message", func(t *testing.T) {
		svc := &fakeCommitService{stream: &fakeCommitStream{pages: []*gitalypb.ListCommitsResponse{
			{Commits: []*gitalypb.GitCommit{gitCommit("aaa", "first", "first\n\nGraft-Auto-Revert: bbb\n")}},
		}}}
		commits, err := (&CommitLister{Commits: svc, Repository: repo}).FirstParentRange(ctx, base, tip)
		require.NoError(t, err)
		require.Equal(t, []CommitInfo{{ID: "aaa", Subject: "first", Message: "first\n\nGraft-Auto-Revert: bbb\n"}}, commits)
	})

	t.Run("committer is reported with its date", func(t *testing.T) {
		date := time.Date(2026, 10, 14, 11, 30, 0, 0, time.UTC)
		c := gitCommit("aaa", "first", "")
		c.Committer = &gitalypb.CommitAuthor{Name: []byte("graft-verify"), Email: []byte("v@example.com"), Date: timestamppb.New(date)}
		svc := &fakeCommitService{stream: &fakeCommitStream{pages: []*gitalypb.ListCommitsResponse{{Commits: []*gitalypb.GitCommit{c}}}}}
		commits, err := (&CommitLister{Commits: svc, Repository: repo}).FirstParentRange(ctx, base, tip)
		require.NoError(t, err)
		require.Equal(t, []CommitInfo{{ID: "aaa", Subject: "first", Message: "first", CommitterEmail: "v@example.com", CommittedAt: date}}, commits)
	})

	t.Run("commits since a time of a branch, first parent, oldest first", func(t *testing.T) {
		since := time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)
		newer := gitCommit("new", "newer", "")
		newer.Committer = &gitalypb.CommitAuthor{Date: timestamppb.New(since.Add(time.Minute))}
		older := gitCommit("old", "older", "")
		older.Committer = &gitalypb.CommitAuthor{Date: timestamppb.New(since.Add(-time.Minute))}
		undated := gitCommit("undated", "undated", "")
		svc := &fakeCommitService{stream: &fakeCommitStream{pages: []*gitalypb.ListCommitsResponse{
			{Commits: []*gitalypb.GitCommit{newer}},
			{Commits: []*gitalypb.GitCommit{older}},
			{Commits: []*gitalypb.GitCommit{undated}},
		}}}
		l := &CommitLister{Commits: svc, Repository: repo}

		commits, err := l.CommitsSince(ctx, "main", since)
		require.NoError(t, err)
		require.Equal(t, []string{"refs/heads/main"}, svc.got.GetRevisions())
		require.True(t, svc.got.GetFirstParent())
		require.Nil(t, svc.got.GetAfter(), "the server must not stop the walk at the window")
		require.Equal(t, []string{"undated", "newer"}, []string{commits[0].Subject, commits[1].Subject})

		_, err = l.CommitsSince(ctx, "", since)
		require.Error(t, err)
	})

	t.Run("an old-dated tip does not hide the commits below it", func(t *testing.T) {
		// Fast-forwards keep the committer date of the original commit, so the tip
		// can be older than the window while the commits below it are recent.
		since := time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)
		tip := gitCommit("tip", "old tip", "")
		tip.Committer = &gitalypb.CommitAuthor{Email: []byte("dev@example.com"), Date: timestamppb.New(since.Add(-3 * time.Hour))}
		revert := gitCommit("revert", "Revert \"x\"", "")
		revert.Body = []byte("This reverts commit x.\n\nGraft-Auto-Revert: x\n")
		revert.BodySize = int64(len(revert.Body))
		revert.Committer = &gitalypb.CommitAuthor{Email: []byte("v@example.com"), Date: timestamppb.New(since.Add(time.Minute))}
		svc := &fakeCommitService{stream: &fakeCommitStream{pages: []*gitalypb.ListCommitsResponse{
			{Commits: []*gitalypb.GitCommit{tip}},
			{Commits: []*gitalypb.GitCommit{revert}},
		}}}

		commits, err := (&CommitLister{Commits: svc, Repository: repo}).CommitsSince(ctx, "main", since)
		require.NoError(t, err)
		require.Len(t, commits, 1)
		require.Equal(t, "revert", commits[0].ID)
		require.True(t, hasAutoRevertTrailer(commits[0].Message))
	})

	t.Run("a truncated message is marked", func(t *testing.T) {
		full := gitCommit("aaa", "first", "details\n\nGraft-Auto-Revert: bbb\n")
		full.BodySize = int64(len(full.Body))
		cut := gitCommit("bbb", "second", "details\n")
		cut.BodySize = 20000
		svc := &fakeCommitService{stream: &fakeCommitStream{pages: []*gitalypb.ListCommitsResponse{
			{Commits: []*gitalypb.GitCommit{full, cut}},
		}}}

		commits, err := (&CommitLister{Commits: svc, Repository: repo}).FirstParentRange(ctx, base, tip)
		require.NoError(t, err)
		require.Equal(t, []CommitInfo{
			{ID: "bbb", Subject: "second", Message: "second\n\ndetails\n", Truncated: true},
			{ID: "aaa", Subject: "first", Message: "first\n\ndetails\n\nGraft-Auto-Revert: bbb\n"},
		}, commits)
	})

	t.Run("empty range", func(t *testing.T) {
		svc := &fakeCommitService{stream: &fakeCommitStream{}}
		commits, err := (&CommitLister{Commits: svc, Repository: repo}).FirstParentRange(ctx, base, tip)
		require.NoError(t, err)
		require.Empty(t, commits)
	})

	t.Run("range larger than the limit is refused", func(t *testing.T) {
		svc := &fakeCommitService{stream: &fakeCommitStream{pages: []*gitalypb.ListCommitsResponse{
			{Commits: []*gitalypb.GitCommit{gitCommit("a", "a", ""), gitCommit("b", "b", ""), gitCommit("c", "c", "")}},
		}}}
		_, err := (&CommitLister{Commits: svc, Repository: repo, MaxCommits: 2}).FirstParentRange(ctx, base, tip)
		require.ErrorContains(t, err, "more than 2 commits")
		require.Equal(t, int32(3), svc.got.GetPaginationParams().GetLimit(), "the server stops one commit beyond the bound")
		require.ErrorIs(t, svc.ctx.Err(), context.Canceled, "the stream is cancelled on the early return")
	})

	t.Run("the server is asked for one commit beyond the default bound", func(t *testing.T) {
		svc := &fakeCommitService{stream: &fakeCommitStream{}}
		_, err := (&CommitLister{Commits: svc, Repository: repo}).FirstParentRange(ctx, base, tip)
		require.NoError(t, err)
		require.Equal(t, int32(DefaultMaxBisectCommits+1), svc.got.GetPaginationParams().GetLimit())
		require.ErrorIs(t, svc.ctx.Err(), context.Canceled)
	})

	t.Run("range at the limit is accepted", func(t *testing.T) {
		svc := &fakeCommitService{stream: &fakeCommitStream{pages: []*gitalypb.ListCommitsResponse{
			{Commits: []*gitalypb.GitCommit{gitCommit("a", "a", ""), gitCommit("b", "b", "")}},
		}}}
		commits, err := (&CommitLister{Commits: svc, Repository: repo, MaxCommits: 2}).FirstParentRange(ctx, base, tip)
		require.NoError(t, err)
		require.Len(t, commits, 2)
	})

	t.Run("stream failure is an error", func(t *testing.T) {
		svc := &fakeCommitService{stream: &fakeCommitStream{err: errors.New("connection reset")}}
		_, err := (&CommitLister{Commits: svc, Repository: repo}).FirstParentRange(ctx, base, tip)
		require.ErrorContains(t, err, "connection reset")
	})

	t.Run("rpc failure is an error", func(t *testing.T) {
		svc := &fakeCommitService{err: errors.New("unavailable")}
		_, err := (&CommitLister{Commits: svc, Repository: repo}).FirstParentRange(ctx, base, tip)
		require.ErrorContains(t, err, "unavailable")
	})

	t.Run("unconfigured lister is refused", func(t *testing.T) {
		_, err := (&CommitLister{}).FirstParentRange(ctx, base, tip)
		require.Error(t, err)
	})

	t.Run("empty base or tip is refused without calling the server", func(t *testing.T) {
		svc := &fakeCommitService{stream: &fakeCommitStream{}}
		l := &CommitLister{Commits: svc, Repository: repo}

		_, err := l.FirstParentRange(ctx, "", tip)
		require.Error(t, err)
		_, err = l.FirstParentRange(ctx, base, "")
		require.Error(t, err)
		require.Nil(t, svc.got)
	})
}

// datedCommit returns a commit of email at the given date. A zero date leaves
// the date unknown.
func datedCommit(id, email string, at time.Time, body string) *gitalypb.GitCommit {
	c := gitCommit(id, id, body)
	c.BodySize = int64(len(c.Body))
	c.Committer = &gitalypb.CommitAuthor{Email: []byte(email)}
	if !at.IsZero() {
		c.Committer.Date = timestamppb.New(at)
	}
	return c
}

// autoRevertCommit returns an automatic revert of the verifier at the date.
func autoRevertCommit(id string, at time.Time) *gitalypb.GitCommit {
	c := datedCommit(id, DefaultRevertCommitterEmail, at, "This reverts commit x.\n\nGraft-Auto-Revert: x\n")
	c.Subject = []byte(`Revert "x"`)
	return c
}

// commitService streams commits, newest first, in pages as the server does.
func commitService(commits []*gitalypb.GitCommit) *fakeCommitService {
	const pageSize = 100
	var pages []*gitalypb.ListCommitsResponse
	for len(commits) > 0 {
		n := min(pageSize, len(commits))
		pages = append(pages, &gitalypb.ListCommitsResponse{Commits: commits[:n]})
		commits = commits[n:]
	}
	return &fakeCommitService{stream: &fakeCommitStream{pages: pages}}
}

func TestCommitLister_CommitsSinceBound(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "x.git"}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)

	t.Run("a history longer than the bound is cut to the newest commits", func(t *testing.T) {
		svc := commitService([]*gitalypb.GitCommit{
			datedCommit("tip", "dev@example.com", now.Add(-time.Minute), ""),
			autoRevertCommit("revert", now.Add(-10*time.Minute)),
			datedCommit("c", "dev@example.com", now.Add(-2*time.Hour), ""),
			datedCommit("d", "dev@example.com", now.Add(-3*time.Hour), ""),
			datedCommit("e", "dev@example.com", now.Add(-4*time.Hour), ""),
		})
		commits, err := (&CommitLister{Commits: svc, Repository: repo, MaxCommits: 3}).CommitsSince(ctx, "main", since)
		require.NoError(t, err)
		require.Equal(t, int32(4), svc.got.GetPaginationParams().GetLimit(), "the server stops one commit beyond the bound")
		require.Equal(t, gitalypb.ListCommitsRequest_TOPO, svc.got.GetOrder())
		require.False(t, svc.got.GetReverse(), "the bound must keep the newest commits")
		require.Nil(t, svc.got.GetAfter())
		require.ErrorIs(t, svc.ctx.Err(), context.Canceled, "the stream is cancelled on the early return")
		require.Equal(t, []string{"revert", "tip"}, []string{commits[0].ID, commits[1].ID})
		require.Len(t, commits, 2)
	})

	t.Run("a burst beyond the bound inside the window is an error", func(t *testing.T) {
		svc := commitService([]*gitalypb.GitCommit{
			datedCommit("a", "dev@example.com", now.Add(-time.Minute), ""),
			datedCommit("b", "dev@example.com", now.Add(-2*time.Minute), ""),
			datedCommit("c", "dev@example.com", now.Add(-3*time.Minute), ""),
		})
		_, err := (&CommitLister{Commits: svc, Repository: repo, MaxCommits: 2}).CommitsSince(ctx, "main", since)
		require.ErrorContains(t, err, "more than 2 commits since")
	})

	t.Run("an undated commit at the bound does not close the window", func(t *testing.T) {
		svc := commitService([]*gitalypb.GitCommit{
			datedCommit("a", "dev@example.com", time.Time{}, ""),
			datedCommit("b", "dev@example.com", time.Time{}, ""),
			datedCommit("c", "dev@example.com", time.Time{}, ""),
		})
		commits, err := (&CommitLister{Commits: svc, Repository: repo, MaxCommits: 2}).CommitsSince(ctx, "main", since)
		require.NoError(t, err)
		require.Equal(t, []string{"b", "a"}, []string{commits[0].ID, commits[1].ID})
	})

	t.Run("a history at the bound is listed in full", func(t *testing.T) {
		svc := commitService([]*gitalypb.GitCommit{
			datedCommit("a", "dev@example.com", now.Add(-time.Minute), ""),
			datedCommit("b", "dev@example.com", now.Add(-2*time.Minute), ""),
		})
		commits, err := (&CommitLister{Commits: svc, Repository: repo, MaxCommits: 2}).CommitsSince(ctx, "main", since)
		require.NoError(t, err)
		require.Len(t, commits, 2)
	})
}

// TestAutoReverter_LongHistory checks that a branch with more first-parent
// commits than DefaultMaxBisectCommits keeps auto-revert working: the cap used
// to fail to count, which refused every revert for good.
func TestAutoReverter_LongHistory(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "x.git"}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	committer := Signature{Name: DefaultRevertCommitterName, Email: DefaultRevertCommitterEmail}

	// history returns recent lands, then old ones, newest first, with an
	// automatic revert of the window below the recent lands when withRevert.
	history := func(recent, old int, oldAt time.Time, withRevert bool) []*gitalypb.GitCommit {
		var commits []*gitalypb.GitCommit
		for i := range recent {
			commits = append(commits, datedCommit(fmt.Sprintf("recent-%d", i), "dev@example.com", now.Add(-time.Duration(i+1)*100*time.Millisecond), ""))
		}
		if withRevert {
			commits = append(commits, autoRevertCommit("revert", now.Add(-20*time.Minute)))
		}
		for i := range old {
			commits = append(commits, datedCommit(fmt.Sprintf("old-%d", i), "dev@example.com", oldAt, ""))
		}
		return commits
	}

	apply := func(t *testing.T, commits []*gitalypb.GitCommit, maxPerHour int) (RevertOutcome, *syncReverter) {
		t.Helper()
		lister := &CommitLister{Commits: commitService(commits), Repository: repo}
		rev := &syncReverter{tip: testTip}
		a := &AutoReverter{
			Reverter: rev,
			History:  lister,
			Policy:   RevertPolicy{MaxPerHour: maxPerHour, Committer: committer},
			Now:      func() time.Time { return now },
		}
		out, err := a.Apply(ctx, "main", failedResult(culpritCommit))
		require.NoError(t, err)
		return out, rev
	}

	t.Run("a long history without reverts is reverted", func(t *testing.T) {
		out, rev := apply(t, history(0, DefaultMaxBisectCommits+1, now.Add(-48*time.Hour), false), 3)
		require.Equal(t, StatusReverted, out.Status, out.Reason)
		require.Equal(t, 1, rev.count())
	})

	t.Run("a long undated history without reverts is reverted", func(t *testing.T) {
		out, _ := apply(t, history(0, DefaultMaxBisectCommits+1, time.Time{}, false), 3)
		require.Equal(t, StatusReverted, out.Status, out.Reason)
	})

	t.Run("a recent revert above the bound is counted", func(t *testing.T) {
		commits := history(5, DefaultMaxBisectCommits, now.Add(-2*time.Hour), true)
		require.Greater(t, len(commits), DefaultMaxBisectCommits)

		out, rev := apply(t, commits, 2)
		require.Equal(t, StatusReverted, out.Status, "a revert below the cap is allowed: %s", out.Reason)
		require.Equal(t, 1, rev.count())

		out, rev = apply(t, commits, 1)
		require.Equal(t, StatusRefused, out.Status)
		require.Contains(t, out.Reason, "cap reached: 1 reverts")
		require.Zero(t, rev.count())
	})

	t.Run("more lands than the bound within the hour refuse", func(t *testing.T) {
		// The window is not covered, so the cap cannot be counted.
		out, rev := apply(t, history(DefaultMaxBisectCommits+1, 0, time.Time{}, false), 3)
		require.Equal(t, StatusRefused, out.Status)
		require.Contains(t, out.Reason, "cannot count the reverts")
		require.Zero(t, rev.count())
	})
}
