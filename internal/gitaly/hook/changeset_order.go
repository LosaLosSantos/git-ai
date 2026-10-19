package hook

import (
	"crypto/sha256"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
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

	changelogUnorderedRecordsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "graft_changelog_unordered_records_total",
		Help: "Committed reference transactions recorded in the changeset log without their place in the order: their ticket expired before their committed phase, or they took none.",
	})
)

// appendChangesetGroup writes a group of records to the changeset log. Tests replace it.
var appendChangesetGroup = changelog.AppendGroup

// changesetTicket is the place of a prepared reference transaction in the order of the changeset
// log of its repository.
type changesetTicket struct {
	// key identifies the transaction: the digest of the invocation of the git process that runs
	// it and of the updates the hook was given (changesetTicketKey). Git gives the same updates to
	// every phase of a transaction, and the invocation tells apart the transactions of different
	// processes, so that a transaction with the same updates as another never takes, releases or
	// records the other's ticket.
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
//
// A ticket is in one of three states, and every change of state happens under the lock: queued
// (in the queue, without record), ready (in the queue, with its record: only the group writer
// removes it, once its entries are written), or gone (out of the queue: written, aborted,
// released or expired). The committed phase finds its queued ticket and makes it ready in one
// step (recordChangeset), so a ticket that expired or was released before that step is never
// mistaken for a written one: the transaction is then recorded at once, out of order, and counted.
//
// The queues are kept by repository (changesetQueueKey), not by path, so that a committed phase
// that cannot resolve the path of its repository can still give up its place.
var changesetTickets = struct {
	sync.Mutex
	queues map[string]*changesetQueue
}{queues: map[string]*changesetQueue{}}

// changesetQueueKey returns the key of the queue of the repository: its storage and relative
// path, which every hook payload of the repository carries, whatever its object directories.
func changesetQueueKey(repo storage.Repository) string {
	return repo.GetStorageName() + "\x00" + repo.GetRelativePath()
}

// changesetTicketKey returns the key of the transaction that the git process with the given
// invocation runs with the given hook input.
func changesetTicketKey(invocation string, changes []byte) [sha256.Size]byte {
	digest := sha256.New()
	digest.Write([]byte(invocation))
	digest.Write([]byte{0})
	digest.Write(changes)

	var key [sha256.Size]byte
	digest.Sum(key[:0])
	return key
}

// changesetQueueLocked returns the queue with the given key, creating it. The caller must hold
// changesetTickets.
func changesetQueueLocked(queueKey string) *changesetQueue {
	q, ok := changesetTickets.queues[queueKey]
	if !ok {
		q = &changesetQueue{changed: make(chan struct{})}
		changesetTickets.queues[queueKey] = q
	}
	return q
}

// dropIfIdleLocked forgets the queue q with the given key when it holds nothing. The caller must
// hold changesetTickets.
func dropIfIdleLocked(queueKey string, q *changesetQueue) {
	if len(q.tickets) == 0 && !q.writing && changesetTickets.queues[queueKey] == q {
		delete(changesetTickets.queues, queueKey)
	}
}

// queuedTicketLocked returns the position and the queued ticket with the given key in q, the
// oldest one if there are several, or nil. The caller must hold changesetTickets.
func queuedTicketLocked(q *changesetQueue, key [sha256.Size]byte) (int, *changesetTicket) {
	for i, ticket := range q.tickets {
		if ticket.key == key && ticket.record == nil {
			return i, ticket
		}
	}
	return -1, nil
}

// takeChangesetTicket appends a ticket for the prepared transaction that the git process with the
// given invocation runs with the given hook input to the queue with the given key.
func takeChangesetTicket(queueKey, invocation string, changes []byte) {
	changesetTickets.Lock()
	defer changesetTickets.Unlock()

	now := time.Now()
	q := changesetQueueLocked(queueKey)
	pruneExpiredChangesetTickets(q, now)
	q.tickets = append(q.tickets, &changesetTicket{
		key:      changesetTicketKey(invocation, changes),
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

// releaseChangesetTicket gives up the place of the transaction that the git process with the
// given invocation runs with the given hook input, in the queue with the given key: its ticket, if
// it is still queued, is removed, and the transactions that wait for it are woken. It is for a
// transaction that was aborted or records nothing.
func releaseChangesetTicket(queueKey, invocation string, changes []byte) {
	changesetTickets.Lock()
	defer changesetTickets.Unlock()

	q, ok := changesetTickets.queues[queueKey]
	if !ok {
		return
	}

	if i, ticket := queuedTicketLocked(q, changesetTicketKey(invocation, changes)); ticket != nil {
		close(ticket.done)
		q.tickets = append(q.tickets[:i:i], q.tickets[i+1:]...)
		q.signal()
	}

	dropIfIdleLocked(queueKey, q)
}

// recordChangeset records record, the committed transaction that the git process with the given
// invocation runs with the given hook input, in the changeset log of the repository at repoPath,
// whose queue has the given key. It returns once the entries are durable or failed to be written.
//
// The ticket of the transaction is found and made ready in one step. Without a queued ticket
// (it expired, or the transaction took none) the record is appended at once, out of order, and
// counted (graft_changelog_unordered_records_total). Otherwise it is recorded after the
// transactions whose tickets are before it: while no group is being written and the ticket at
// the head of the queue is ready, the caller writes every ready ticket at the head with one call
// of changelog.AppendGroup, one write and one fsync, and wakes their committed phases with the
// outcome. Otherwise it waits for the queue to change or for the ticket at the head to expire.
// The wait does not end with the request: the updates are committed in git already, so their
// entries are always written, in order. It ends at the latest once every ticket before this one
// was written, aborted or expired, which changesetTicketTimeout bounds.
func recordChangeset(queueKey, repoPath, invocation string, changes []byte, record changelog.Record) error {
	start := time.Now()
	defer func() { changesetTicketWaitSeconds.Observe(time.Since(start).Seconds()) }()

	changesetTickets.Lock()
	var ticket *changesetTicket
	q, ok := changesetTickets.queues[queueKey]
	if ok {
		_, ticket = queuedTicketLocked(q, changesetTicketKey(invocation, changes))
	}
	if ticket == nil {
		changesetTickets.Unlock()
		changelogUnorderedRecordsTotal.Inc()
		_, err := appendChangesetGroup(repoPath, []changelog.Record{record})
		return err
	}
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
			writeChangesetGroupLocked(queueKey, repoPath, q)
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

// writeChangesetGroupLocked writes the ready tickets at the head of q, the queue with the given
// key of the repository at repoPath, as one group. The caller must hold changesetTickets, which is
// released while the group is written and held again when the function returns.
func writeChangesetGroupLocked(queueKey, repoPath string, q *changesetQueue) {
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
	err := appendChangesetGroupRecovering(repoPath, records)
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
	dropIfIdleLocked(queueKey, q)
}

// appendChangesetGroupRecovering writes the group of records and turns a panic of the write into
// its error. The group's committed phases, which wait for its outcome, then all see the failure
// and report their updates as lost, and the queue is not left behind a writer that never ends.
func appendChangesetGroupRecovering(repoPath string, records []changelog.Record) (returnedErr error) {
	defer func() {
		if r := recover(); r != nil {
			returnedErr = fmt.Errorf("writing the changeset group panicked: %v\n%s", r, debug.Stack())
		}
	}()

	_, err := appendChangesetGroup(repoPath, records)
	return err
}
