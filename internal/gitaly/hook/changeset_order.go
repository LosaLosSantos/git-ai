package hook

import (
	"crypto/sha256"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// changesetTicketTimeout bounds how long recording waits for a reference transaction that was
// prepared earlier. A transaction that never reports its outcome, because its process died
// between the prepared and the committed phase or because Git does not run the committed phase
// for it, would otherwise block the changeset log of the repository forever. Tests lower it.
var changesetTicketTimeout = 5 * time.Second

var (
	changelogGroupTransactions = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "graft_changelog_group_entries",
		Help:    "Reference transactions recorded in the changeset log by one write and one fsync.",
		Buckets: []float64{1, 2, 4, 8, 16, 32, 64, 128},
	})

	changelogAppendSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "graft_changelog_append_seconds",
		Help:    "Time of one write of the changeset log, with its fsync, for a group of reference transactions.",
		Buckets: prometheus.DefBuckets,
	})

	changesetTicketWaitSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "graft_changeset_ticket_wait_seconds",
		Help:    "Time from the committed phase of a reference transaction to its entries being durable in the changeset log.",
		Buckets: prometheus.DefBuckets,
	})

	changesetTicketsExpiredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "graft_changeset_tickets_expired_total",
		Help: "Prepared reference transactions that did not report their outcome in time, so that the transactions after them were recorded without them.",
	})

	changelogRecordsLostTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "graft_changelog_records_lost_total",
		Help: "Committed reference updates that the changeset log does not record, because writing them failed.",
	})
)

// changesetTicket is the place of a prepared reference transaction in the order of the changeset
// log of its repository.
type changesetTicket struct {
	// key identifies the transaction: the digest of the updates the hook was given. Git gives
	// the same updates to every phase of a transaction.
	key [sha256.Size]byte
	// done is closed when the entries of the transaction were written (see err), or when the
	// transaction was aborted, expired or records nothing.
	done chan struct{}
	// deadline is when later transactions stop waiting for this one.
	deadline time.Time
	// record holds the updates of the transaction once its committed phase ran. The ticket is
	// ready from then on, and only the group writer removes it.
	record *changelog.Record
	// err is the outcome of the write of the record. It is set before done is closed.
	err error
}

// changesetQueue holds the tickets of one repository in the order of their prepared phases.
type changesetQueue struct {
	tickets []*changesetTicket
	// writing is set while a committed phase writes a group of the queue.
	writing bool
	// changed is closed and replaced whenever the queue changes, which wakes the committed phases
	// that wait.
	changed chan struct{}
}

// signal wakes the committed phases that wait on the queue. The caller must hold changesetTickets.
func (q *changesetQueue) signal() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// changesetTickets orders the recording of committed reference updates in the changeset log.
//
// The committed phase of the reference-transaction hook runs after the references were updated
// and unlocked. A concurrent writer can then update the same references and record its update
// before the first one is recorded, so the log would describe the updates out of order. The
// prepared phase runs while the references are locked, so the order in which transactions reach
// it is the order of their updates for every reference they share. Each prepared transaction
// whose updates are recorded takes a ticket in that order, and the log records the transactions of
// a repository in the order of their tickets, whatever references they update: a transaction that
// read a reference which an earlier one updated is recorded after it.
//
// The entries are written in groups (recordInOrder): the committed phase that finds no group being
// written writes every ready ticket at the head of the queue with one write and one fsync. Each
// committed phase returns only once its own entries are durable.
//
// The tickets live in the process that serves the hook calls, which is the Gitaly process that
// spawned Git. A ticket whose transaction never reports its outcome expires after
// changesetTicketTimeout, after which the later transactions record without waiting for it.
var changesetTickets = struct {
	sync.Mutex
	queues map[string]*changesetQueue
}{queues: map[string]*changesetQueue{}}

// changesetTicketKey returns the key of the transaction with the given hook input.
func changesetTicketKey(changes []byte) [sha256.Size]byte {
	return sha256.Sum256(changes)
}

// changesetQueueLocked returns the queue of the repository at repoPath, creating it. The caller
// must hold changesetTickets.
func changesetQueueLocked(repoPath string) *changesetQueue {
	q, ok := changesetTickets.queues[repoPath]
	if !ok {
		q = &changesetQueue{changed: make(chan struct{})}
		changesetTickets.queues[repoPath] = q
	}
	return q
}

// dropIfIdleLocked forgets the queue of repoPath when it holds nothing. The caller must hold
// changesetTickets.
func dropIfIdleLocked(repoPath string, q *changesetQueue) {
	if len(q.tickets) == 0 && !q.writing && changesetTickets.queues[repoPath] == q {
		delete(changesetTickets.queues, repoPath)
	}
}

// takeChangesetTicket appends a ticket for the prepared transaction with the given hook input to
// the queue of the repository at repoPath.
func takeChangesetTicket(repoPath string, changes []byte) {
	changesetTickets.Lock()
	defer changesetTickets.Unlock()

	now := time.Now()
	q := changesetQueueLocked(repoPath)
	pruneExpiredChangesetTickets(q, now)
	q.tickets = append(q.tickets, &changesetTicket{
		key:      changesetTicketKey(changes),
		done:     make(chan struct{}),
		deadline: now.Add(changesetTicketTimeout),
	})
}

// pruneExpiredChangesetTickets drops the expired tickets from the front of the queue whose
// transactions did not run their committed phase. A ready ticket is never dropped: its committed
// phase waits for its entries to be written. Expired tickets further back are dropped once they
// reach the front. The caller must hold changesetTickets.
func pruneExpiredChangesetTickets(q *changesetQueue, now time.Time) {
	pruned := 0
	for len(q.tickets) > 0 && q.tickets[0].record == nil && !now.Before(q.tickets[0].deadline) {
		close(q.tickets[0].done)
		q.tickets = q.tickets[1:]
		pruned++
	}

	if pruned > 0 {
		changesetTicketsExpiredTotal.Add(float64(pruned))
		q.signal()
	}
}

// findChangesetTicket returns the oldest ticket of the transaction with the given hook input in
// the queue of the repository at repoPath. It returns nil when the transaction has none, for
// example because it was prepared before the changeset log was enabled or its ticket expired.
func findChangesetTicket(repoPath string, changes []byte) *changesetTicket {
	changesetTickets.Lock()
	defer changesetTickets.Unlock()

	q, ok := changesetTickets.queues[repoPath]
	if !ok {
		return nil
	}

	key := changesetTicketKey(changes)
	for _, ticket := range q.tickets {
		if ticket.key == key && ticket.record == nil {
			return ticket
		}
	}

	return nil
}

// releaseChangesetTicket removes the ticket, whose transaction records nothing, from the queue of
// the repository at repoPath and wakes the transactions that wait for it.
func releaseChangesetTicket(repoPath string, ticket *changesetTicket) {
	changesetTickets.Lock()
	defer changesetTickets.Unlock()

	q, ok := changesetTickets.queues[repoPath]
	if !ok {
		return
	}

	for i, queued := range q.tickets {
		if queued != ticket {
			continue
		}

		close(ticket.done)
		q.tickets = append(q.tickets[:i:i], q.tickets[i+1:]...)
		q.signal()
		break
	}

	dropIfIdleLocked(repoPath, q)
}

// abortChangesetTicket releases the ticket of the aborted transaction with the given hook input,
// if it has one.
func abortChangesetTicket(repoPath string, changes []byte) {
	if ticket := findChangesetTicket(repoPath, changes); ticket != nil {
		releaseChangesetTicket(repoPath, ticket)
	}
}

// recordInOrder records the record of the transaction of ticket in the changeset log of the
// repository at repoPath, after the transactions whose tickets are before it, and returns once
// its entries are durable or failed to be written.
//
// The ticket becomes ready. While no group is being written and the ticket at the head of the
// queue is ready, the caller writes every ready ticket at the head with one call of
// changelog.AppendGroup, one write and one fsync, and wakes their committed phases with the
// outcome. Otherwise it waits for the queue to change or for the ticket at the head to expire.
// The wait does not end with the request: the updates are committed in git already, so their
// entries are always written, in order. It ends at the latest once every ticket before this one
// was written, aborted or expired, which changesetTicketTimeout bounds.
func recordInOrder(repoPath string, ticket *changesetTicket, record changelog.Record) error {
	start := time.Now()
	defer func() { changesetTicketWaitSeconds.Observe(time.Since(start).Seconds()) }()

	changesetTickets.Lock()
	q := changesetQueueLocked(repoPath)
	ticket.record = &record
	q.signal()

	for {
		select {
		case <-ticket.done:
			changesetTickets.Unlock()
			return ticket.err
		default:
		}

		pruneExpiredChangesetTickets(q, time.Now())

		if !q.writing && len(q.tickets) > 0 && q.tickets[0].record != nil {
			writeChangesetGroupLocked(repoPath, q)
			continue
		}

		changed := q.changed
		var expiry *time.Timer
		var expired <-chan time.Time
		if len(q.tickets) > 0 && q.tickets[0].record == nil {
			expiry = time.NewTimer(time.Until(q.tickets[0].deadline))
			expired = expiry.C
		}
		changesetTickets.Unlock()

		select {
		case <-ticket.done:
		case <-changed:
		case <-expired:
		}
		if expiry != nil {
			expiry.Stop()
		}

		changesetTickets.Lock()
	}
}

// writeChangesetGroupLocked writes the ready tickets at the head of q, the queue of the repository
// at repoPath, as one group. The caller must hold changesetTickets, which is released while the
// group is written.
func writeChangesetGroupLocked(repoPath string, q *changesetQueue) {
	n := 0
	for n < len(q.tickets) && q.tickets[n].record != nil {
		n++
	}
	group := append([]*changesetTicket(nil), q.tickets[:n]...)
	q.writing = true
	changesetTickets.Unlock()

	records := make([]changelog.Record, 0, len(group))
	for _, ticket := range group {
		records = append(records, *ticket.record)
	}

	start := time.Now()
	_, err := changelog.AppendGroup(repoPath, records)
	changelogAppendSeconds.Observe(time.Since(start).Seconds())
	changelogGroupTransactions.Observe(float64(len(group)))

	changesetTickets.Lock()
	written := make(map[*changesetTicket]bool, len(group))
	for _, ticket := range group {
		ticket.err = err
		close(ticket.done)
		written[ticket] = true
	}

	// Only the group writer removes ready tickets, and tickets are only added at the back, so the
	// group is still at the head of the queue; the tickets are removed by identity all the same.
	remaining := q.tickets[:0:0]
	for _, ticket := range q.tickets {
		if !written[ticket] {
			remaining = append(remaining, ticket)
		}
	}
	q.tickets = remaining
	q.writing = false
	q.signal()
	dropIfIdleLocked(repoPath, q)
}
