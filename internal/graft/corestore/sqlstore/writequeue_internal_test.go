package sqlstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWriteQueueNeverLosesTheAdmission races the hand-over of the admission
// against the end of the waiting writer's context: whichever wins, the
// admission ends up either with that writer or free, never with nobody while
// marked busy.
func TestWriteQueueNeverLosesTheAdmission(t *testing.T) {
	queued := make(chan struct{})
	queuedHook = func(int) { queued <- struct{}{} }
	t.Cleanup(func() { queuedHook = nil })

	for range 500 {
		var q writeQueue
		require.NoError(t, q.acquire(context.Background()))

		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- q.acquire(ctx) }()

		// Once the second writer is queued, hand over and cancel at the same
		// time.
		<-queued
		go cancel()
		q.release()

		if err := <-result; err == nil {
			// The waiter got the admission: it holds it, and must release it.
			q.release()
		}
		require.Zero(t, q.waiting())
		require.False(t, q.busy, "the admission is free once every holder released it")
	}
}

// TestWriteQueueAcquireWithEndedContext checks that a writer whose context has
// already ended neither takes nor waits for the admission.
func TestWriteQueueAcquireWithEndedContext(t *testing.T) {
	var q writeQueue
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, q.acquire(ctx), context.Canceled)
	require.False(t, q.busy)
	require.Zero(t, q.waiting())
}
