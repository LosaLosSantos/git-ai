package changeset

import (
	"context"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// watchPollInterval is how often WatchChangesets re-reads the log even without
// a notification, as a fallback for appends that the notifications missed,
// for example appends made by another process.
var watchPollInterval = time.Second

// WatchChangesets streams the changesets with a sequence greater than
// after_sequence and then keeps the stream open, streaming new changesets as
// they are recorded, until the client cancels.
//
// Each wake-up (a notification or the poll tick) scans the log from the last
// sequence sent. The scan starts at the closest offset index point before that
// sequence, so a wake-up reads at most a few hundred entries before the new
// ones, whatever the size of the log.
func (s *server) WatchChangesets(req *gitalypb.WatchChangesetsRequest, stream gitalypb.ChangesetService_WatchChangesetsServer) error {
	ctx := stream.Context()

	source, err := s.changelogSource(ctx, req.GetRepository())
	if err != nil {
		return err
	}

	// Subscribe before reading the backlog so that no append made in between is
	// missed: it would wake up the loop below after the backlog is sent.
	wake, unsubscribe := source.Subscribe()
	defer unsubscribe()

	ticker := time.NewTicker(watchPollInterval)
	defer ticker.Stop()

	cursor := req.GetAfterSequence()
	for {
		cursor, err = sendAfter(ctx, stream, source, cursor)
		if err != nil {
			if ctx.Err() != nil {
				// The client went away, which is the normal way to end a watch.
				return nil
			}

			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		case <-ticker.C:
		}
	}
}

// sendAfter streams every changeset of the log with a sequence greater than
// cursor and returns the sequence of the last one sent, or cursor if none was.
func sendAfter(ctx context.Context, stream gitalypb.ChangesetService_WatchChangesetsServer, source changelog.Source, cursor uint64) (uint64, error) {
	err := source.Scan(ctx, cursor, func(entry changelog.Entry) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		if err := stream.Send(&gitalypb.WatchChangesetsResponse{Changeset: toProto(entry)}); err != nil {
			return false, err
		}

		cursor = entry.Sequence
		return true, nil
	})
	if err != nil {
		return cursor, structerr.NewInternal("reading changelog: %w", err)
	}

	return cursor, nil
}
