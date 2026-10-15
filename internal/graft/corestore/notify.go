package corestore

import "sync"

// Changelog notifications within one process. The write path calls
// NotifyChangelog after it committed a changeset that appended changelog
// entries; watchers of the database changelog (ChangesetService in
// authoritative mode) wake up immediately on the node that made the change
// and poll for changes made by other nodes.

var notifier = struct {
	sync.Mutex
	subs map[int64]map[chan struct{}]struct{}
}{subs: map[int64]map[chan struct{}]struct{}{}}

// SubscribeChangelog returns a channel that receives a value (coalesced,
// buffer of one) after each NotifyChangelog for the repository, and a
// function that ends the subscription.
func SubscribeChangelog(repoID int64) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	notifier.Lock()
	if notifier.subs[repoID] == nil {
		notifier.subs[repoID] = map[chan struct{}]struct{}{}
	}
	notifier.subs[repoID][ch] = struct{}{}
	notifier.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			notifier.Lock()
			delete(notifier.subs[repoID], ch)
			if len(notifier.subs[repoID]) == 0 {
				delete(notifier.subs, repoID)
			}
			notifier.Unlock()
		})
	}
}

// NotifyChangelog wakes up the subscribers of the repository. It never blocks.
func NotifyChangelog(repoID int64) {
	notifier.Lock()
	defer notifier.Unlock()

	for ch := range notifier.subs[repoID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
