package sdk

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

const (
	// maxWatchFailures is the number of consecutive reconnects without
	// progress after which WatchChangesets gives up.
	maxWatchFailures = 5
	// watchRetryBackoff is the base delay between reconnects.
	watchRetryBackoff = 200 * time.Millisecond
)

// Changeset is one entry of the repository's changeset log.
type Changeset = gitalypb.Changeset

// Changesets returns the changesets with a sequence number greater than
// after, in ascending order. The log is dense: sequence numbers start at 1
// and have no gaps, so the last returned sequence number is the cursor for
// the next call.
func (r *Repo) Changesets(ctx context.Context, after uint64, limit uint32) ([]*Changeset, error) {
	stream, err := r.client.changesets.ListChangesets(ctx, &gitalypb.ListChangesetsRequest{
		Repository:    r.repository(),
		AfterSequence: after,
		Limit:         limit,
	})
	if err != nil {
		return nil, err
	}

	var changesets []*Changeset
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return changesets, nil
		}
		if err != nil {
			return nil, err
		}

		changesets = append(changesets, msg.GetChangesets()...)
	}
}

// WatchChangesets streams the changesets with a sequence number greater than
// after, followed by changesets as they are recorded, until ctx is canceled.
//
// Changesets are sent on the first channel in sequence order. The second
// channel receives at most one error that ends the watch and is closed
// together with the first channel. Transient failures are retried from the
// last delivered sequence number, so no changeset is skipped or repeated.
// Cancelling ctx closes both channels without an error.
func (r *Repo) WatchChangesets(ctx context.Context, after uint64) (<-chan *Changeset, <-chan error) {
	changes := make(chan *Changeset)
	errs := make(chan error, 1)

	go func() {
		defer close(changes)
		defer close(errs)

		cursor := after
		failures := 0
		for {
			before := cursor
			err := r.watchFrom(ctx, &cursor, changes)
			if ctx.Err() != nil {
				return
			}

			if !isTransient(err) {
				errs <- err
				return
			}

			if cursor > before {
				failures = 0
			} else {
				failures++
			}
			if failures >= maxWatchFailures {
				errs <- err
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(watchRetryBackoff * time.Duration(failures+1)):
			}
		}
	}()

	return changes, errs
}

// watchFrom runs one WatchChangesets stream starting after *cursor and sends
// each changeset to out, advancing *cursor. It returns the error that ended
// the stream.
func (r *Repo) watchFrom(ctx context.Context, cursor *uint64, out chan<- *Changeset) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := r.client.changesets.WatchChangesets(ctx, &gitalypb.WatchChangesetsRequest{
		Repository:    r.repository(),
		AfterSequence: *cursor,
	})
	if err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}

		select {
		case out <- msg.GetChangeset():
			*cursor = msg.GetChangeset().GetSequence()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
