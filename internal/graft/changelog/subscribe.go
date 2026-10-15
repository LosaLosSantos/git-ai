package changelog

import "sync"

// subscribers maps a changelog path to the channels that are notified when
// entries are appended to it.
var (
	subscribersMu sync.Mutex
	subscribers   = map[string]map[chan struct{}]struct{}{}
)

// Subscribe returns a channel that receives a value whenever entries are
// appended to the changeset log of the repository whose git directory is
// repoPath, and a function that unsubscribes. Notifications are coalesced: a
// receiver that is slow to read sees one pending value, not one per append, so
// it must re-read the log after each receive. Subscribe before reading the
// backlog to avoid missing appends that happen in between.
func Subscribe(repoPath string) (<-chan struct{}, func()) {
	path := Path(repoPath)
	ch := make(chan struct{}, 1)

	subscribersMu.Lock()
	if subscribers[path] == nil {
		subscribers[path] = map[chan struct{}]struct{}{}
	}
	subscribers[path][ch] = struct{}{}
	subscribersMu.Unlock()

	unsubscribe := func() {
		subscribersMu.Lock()
		defer subscribersMu.Unlock()

		delete(subscribers[path], ch)
		if len(subscribers[path]) == 0 {
			delete(subscribers, path)
		}
	}

	return ch, unsubscribe
}

// notify wakes up the subscribers of the changelog at path. It never blocks.
func notify(path string) {
	subscribersMu.Lock()
	defer subscribersMu.Unlock()

	for ch := range subscribers[path] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
