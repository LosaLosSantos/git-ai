package hook

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"
)

// changesetTicketTimeout bounds how long recording waits for a reference transaction that was
// prepared earlier. A transaction that never reports its outcome, because its process died
// between the prepared and the committed phase or because Git does not run the committed phase
// for it, would otherwise block the changeset log of the repository forever. Tests lower it.
var changesetTicketTimeout = 5 * time.Second

// changesetTicket is the place of a prepared reference transaction in the order of the changeset
// log of its repository.
type changesetTicket struct {
	// key identifies the transaction: the digest of the updates the hook was given. Git gives
	// the same updates to every phase of a transaction.
	key [sha256.Size]byte
	// done is closed when the transaction was recorded or aborted.
	done chan struct{}
	// deadline is when later transactions stop waiting for this one.
	deadline time.Time
}

// changesetTickets orders the recording of committed reference updates in the changeset log.
//
// The committed phase of the reference-transaction hook runs after the references were updated
// and unlocked. A concurrent writer can then update the same references and record its update
// before the first one is recorded, so the log would describe the updates out of order. The
// prepared phase runs while the references are locked, so the order in which transactions reach
// it is the order of their updates for every reference they share. Each prepared transaction
// whose updates are recorded takes a ticket in that order, and its committed phase records only
// once every earlier ticket of the repository was recorded or aborted.
//
// The tickets live in the process that serves the hook calls, which is the Gitaly process that
// spawned Git. A ticket whose transaction never reports its outcome expires after
// changesetTicketTimeout, after which the later transactions record without waiting for it.
var changesetTickets = struct {
	sync.Mutex
	queues map[string][]*changesetTicket
}{queues: map[string][]*changesetTicket{}}

// changesetTicketKey returns the key of the transaction with the given hook input.
func changesetTicketKey(changes []byte) [sha256.Size]byte {
	return sha256.Sum256(changes)
}

// takeChangesetTicket appends a ticket for the prepared transaction with the given hook input to
// the queue of the repository at repoPath.
func takeChangesetTicket(repoPath string, changes []byte) {
	changesetTickets.Lock()
	defer changesetTickets.Unlock()

	now := time.Now()
	queue := pruneExpiredChangesetTickets(changesetTickets.queues[repoPath], now)
	changesetTickets.queues[repoPath] = append(queue, &changesetTicket{
		key:      changesetTicketKey(changes),
		done:     make(chan struct{}),
		deadline: now.Add(changesetTicketTimeout),
	})
}

// pruneExpiredChangesetTickets drops the expired tickets from the front of the queue. Expired
// tickets further back are dropped once they reach the front. The caller must hold
// changesetTickets.
func pruneExpiredChangesetTickets(queue []*changesetTicket, now time.Time) []*changesetTicket {
	for len(queue) > 0 && !now.Before(queue[0].deadline) {
		close(queue[0].done)
		queue = queue[1:]
	}

	return queue
}

// findChangesetTicket returns the oldest ticket of the transaction with the given hook input and
// the tickets that precede it in the queue of the repository at repoPath. It returns a nil ticket
// when the transaction has none, for example because it was prepared before the changeset log was
// enabled or its ticket expired.
func findChangesetTicket(repoPath string, changes []byte) (*changesetTicket, []*changesetTicket) {
	changesetTickets.Lock()
	defer changesetTickets.Unlock()

	key := changesetTicketKey(changes)
	queue := changesetTickets.queues[repoPath]
	for i, ticket := range queue {
		if ticket.key == key {
			return ticket, append([]*changesetTicket(nil), queue[:i]...)
		}
	}

	return nil, nil
}

// releaseChangesetTicket removes the ticket from the queue of the repository at repoPath and
// wakes the transactions that wait for it.
func releaseChangesetTicket(repoPath string, ticket *changesetTicket) {
	changesetTickets.Lock()
	defer changesetTickets.Unlock()

	queue := changesetTickets.queues[repoPath]
	for i, queued := range queue {
		if queued != ticket {
			continue
		}

		close(ticket.done)
		queue = append(queue[:i:i], queue[i+1:]...)
		break
	}

	if len(queue) == 0 {
		delete(changesetTickets.queues, repoPath)
		return
	}
	changesetTickets.queues[repoPath] = queue
}

// abortChangesetTicket releases the ticket of the aborted transaction with the given hook input,
// if it has one.
func abortChangesetTicket(repoPath string, changes []byte) {
	if ticket, _ := findChangesetTicket(repoPath, changes); ticket != nil {
		releaseChangesetTicket(repoPath, ticket)
	}
}

// waitForChangesetTickets waits until every ticket of earlier was recorded, aborted or expired, or
// until the context is done. It reports whether it waited for all of them.
func waitForChangesetTickets(ctx context.Context, earlier []*changesetTicket) bool {
	for _, ticket := range earlier {
		timer := time.NewTimer(time.Until(ticket.deadline))

		select {
		case <-ticket.done:
			timer.Stop()
		case <-timer.C:
			return false
		case <-ctx.Done():
			timer.Stop()
			return false
		}
	}

	return true
}
