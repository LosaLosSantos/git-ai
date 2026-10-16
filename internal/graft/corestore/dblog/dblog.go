// Package dblog is the changelog.Source of a repository whose changelog lives
// in the storage core's database (authoritative mode, see doc/graft/PLAN6.md,
// "Changelog and reviews in the database"). Scans page through the database,
// and watchers are woken up by the in-process notifications of the write path
// and by a poll, so that appends made by other nodes reach them too.
package dblog

import (
	"context"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
)

const (
	// pageSize is the number of entries read from the database at once.
	pageSize = 500
	// pollInterval is how often a subscriber wakes up even without a
	// notification, which covers appends made by other nodes.
	pollInterval = 250 * time.Millisecond
)

// New returns the changelog.Source of the repository repoID of meta.
func New(meta corestore.MetaStore, repoID int64) changelog.Source {
	return source{meta: meta, repoID: repoID}
}

type source struct {
	meta   corestore.MetaStore
	repoID int64
}

// Scan calls fn for every entry with a sequence greater than after, in order,
// until fn returns false or an error. It reads the database in pages.
func (s source) Scan(ctx context.Context, after uint64, fn func(changelog.Entry) (bool, error)) error {
	cursor := after
	for {
		entries, err := s.meta.ScanChangelog(ctx, s.repoID, cursor, pageSize)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			more, err := fn(entry)
			if err != nil {
				return err
			}
			if !more {
				return nil
			}
			cursor = entry.Sequence
		}

		if len(entries) < pageSize {
			return nil
		}
	}
}

// Subscribe returns a channel that receives a coalesced value after the
// changelog of the repository may have grown: on each in-process notification
// of the write path and on every poll tick while the subscription is active.
// The returned function ends the subscription.
func (s source) Subscribe() (<-chan struct{}, func()) {
	// Subscribe to the notifications before returning, so that no append made
	// after the caller's backlog read is missed.
	notified, unsubscribe := corestore.SubscribeChangelog(s.repoID)

	out := make(chan struct{}, 1)
	done := make(chan struct{})

	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-notified:
			case <-ticker.C:
			}

			select {
			case out <- struct{}{}:
			default:
			}
		}
	}()

	var once sync.Once
	return out, func() {
		once.Do(func() {
			unsubscribe()
			close(done)
		})
	}
}
