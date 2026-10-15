package hook

import (
	"errors"
	"sync"
)

// ErrPostReceiveAlreadyRegistered is returned when a transaction ID is already registered.
var ErrPostReceiveAlreadyRegistered = errors.New("transaction already registered")

// PostReceiveRegistry coordinates PostReceiveHook execution with Praefect signaling.
// When Praefect transactions are active, the PostReceiveHook needs to wait for Praefect
// to signal that all replicas have completed and the database has been updated before
// calling Rails. This registry provides the synchronization mechanism.
//
// Signal and RegisterWaiter race, and Signal can arrive first. Both are driven off the
// same event — the transaction's committed phase concluding — but then run on
// independent paths:
//
//   - Praefect (async monitor): CommittedConcluded -> DB generation write -> backchannel
//     SignalPostReceiveReady RPC -> Signal.
//   - Primary Gitaly (the push): committed-phase vote returns -> git finishes the ref
//     update -> post-receive hook runs -> waitForPraefectSignal -> RegisterWaiter.
//
// The committed phase concludes at essentially the same instant on both sides, so if the
// signal path (which includes a DB round-trip and a network RPC) happens to complete
// before the primary reaches its post-receive hook — e.g. fast/local Postgres or
// single-node quorum — Signal is called before RegisterWaiter. To handle this, Signal
// buffers the result in `signaled` and RegisterWaiter returns a pre-populated channel if
// a signal is already waiting. This buffering is required for correctness: without it the
// hook would block until timeout whenever the signal wins the race.
type PostReceiveRegistry struct {
	waiters  map[uint64]chan error // registered but not yet signaled
	signaled map[uint64]error      // signaled before RegisterWaiter was called; nil means success
	mu       sync.Mutex
}

// NewPostReceiveRegistry creates a new PostReceiveRegistry.
func NewPostReceiveRegistry() *PostReceiveRegistry {
	return &PostReceiveRegistry{
		waiters:  make(map[uint64]chan error),
		signaled: make(map[uint64]error),
	}
}

// RegisterWaiter registers a waiter for the given transaction ID. It returns a channel
// that will receive nil on success or an error if post-transaction work failed, and a
// cleanup function that must be called when the waiter is done (either after receiving
// the signal or on timeout/error).
//
// If Signal was already called for this transaction ID before RegisterWaiter (see comment at PostReceiveRegistry for
// explanation), the returned channel will already have a value ready, allowing the caller to proceed immediately.
func (r *PostReceiveRegistry) RegisterWaiter(txID uint64) (<-chan error, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.waiters[txID]; ok {
		return nil, nil, ErrPostReceiveAlreadyRegistered
	}
	ch := make(chan error, 1)

	// Signal arrived before us — return a pre-populated channel immediately.
	if signalErr, ok := r.signaled[txID]; ok {
		delete(r.signaled, txID)
		ch <- signalErr
		return ch, func() {
			// Closing the channel is not strictly necessary here, since the consumer is not
			// ranging over it, just waiting for a single signal, and the channel will
			// eventually be GC'd once no goroutine holds a reference. We close it here
			// as good practice.
			close(ch)
		}, nil
	}

	r.waiters[txID] = ch

	cleanup := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.waiters, txID)
		delete(r.signaled, txID) // harmless dead-ish defensiveness
	}

	return ch, cleanup, nil
}

// Signal unblocks the waiter for the given transaction ID by sending err on its channel.
// A nil err indicates success; a non-nil err indicates post-transaction work failed.
// If the signal arrives before the waiter registers, the signal is stored so that
// when RegisterWaiter is called later, it will return a pre-populated channel.
// If the waiter has already been cleaned up, this is a no-op.
func (r *PostReceiveRegistry) Signal(txID uint64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ch, ok := r.waiters[txID]
	if !ok {
		r.signaled[txID] = err
		return
	}

	select {
	case ch <- err:
	default:
		// Channel already has a value, nothing to do.
	}
}
