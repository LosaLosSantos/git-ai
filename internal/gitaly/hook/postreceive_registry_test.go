package hook

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPostReceiveRegistry(t *testing.T) {
	t.Parallel()

	t.Run("signal before wait", func(t *testing.T) {
		t.Parallel()

		registry := NewPostReceiveRegistry()

		waitCh, cleanup, err := registry.RegisterWaiter(1)
		require.NoError(t, err)
		defer cleanup()

		registry.Signal(1, nil)

		select {
		case <-waitCh:
			// Success - channel was closed
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for signal")
		}
	})

	t.Run("wait before signal", func(t *testing.T) {
		t.Parallel()

		registry := NewPostReceiveRegistry()

		waitCh, cleanup, err := registry.RegisterWaiter(1)
		require.NoError(t, err)
		defer cleanup()

		waiting, done := make(chan struct{}), make(chan struct{})

		go func() {
			close(waiting)
			<-waitCh
			close(done)
		}()

		<-waiting
		registry.Signal(1, nil)

		select {
		case <-done:
			// Success
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for signal")
		}
	})

	t.Run("signal before register", func(t *testing.T) {
		t.Parallel()

		registry := NewPostReceiveRegistry()

		registry.Signal(1, nil)

		waitCh, cleanup, err := registry.RegisterWaiter(1)
		require.NoError(t, err)
		defer cleanup()

		select {
		case <-waitCh:
			// Success - pre-closed channel returned immediately
		default:
			t.Fatal("channel should be closed immediately when signal arrived before register")
		}
	})

	t.Run("signal after cleanup is no-op", func(t *testing.T) {
		t.Parallel()

		registry := NewPostReceiveRegistry()

		_, cleanup, err := registry.RegisterWaiter(1)
		require.NoError(t, err)

		cleanup()

		// Should not panic
		registry.Signal(1, nil)
	})

	t.Run("duplicate signal is no-op", func(t *testing.T) {
		t.Parallel()

		registry := NewPostReceiveRegistry()

		waitCh, cleanup, err := registry.RegisterWaiter(1)
		require.NoError(t, err)
		defer cleanup()

		registry.Signal(1, nil)
		registry.Signal(1, nil) // Should not panic

		select {
		case <-waitCh:
			// Success
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for signal")
		}
	})

	t.Run("duplicate registration returns error", func(t *testing.T) {
		t.Parallel()

		registry := NewPostReceiveRegistry()

		_, cleanup, err := registry.RegisterWaiter(1)
		require.NoError(t, err)
		defer cleanup()

		_, _, err = registry.RegisterWaiter(1)
		require.ErrorIs(t, err, ErrPostReceiveAlreadyRegistered)
	})

	t.Run("multiple waiters", func(t *testing.T) {
		t.Parallel()

		registry := NewPostReceiveRegistry()

		waitCh1, cleanup1, err := registry.RegisterWaiter(1)
		require.NoError(t, err)
		defer cleanup1()

		waitCh2, cleanup2, err := registry.RegisterWaiter(2)
		require.NoError(t, err)
		defer cleanup2()

		waitCh3, cleanup3, err := registry.RegisterWaiter(3)
		require.NoError(t, err)
		defer cleanup3()

		var wg sync.WaitGroup
		wg.Add(3)

		go func() {
			defer wg.Done()
			<-waitCh1
		}()

		go func() {
			defer wg.Done()
			<-waitCh2
		}()

		go func() {
			defer wg.Done()
			<-waitCh3
		}()

		registry.Signal(1, nil)
		registry.Signal(2, nil)
		registry.Signal(3, nil)

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Success
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for all signals")
		}
	})

	t.Run("concurrent register and signal", func(t *testing.T) {
		t.Parallel()

		registry := NewPostReceiveRegistry()

		const numGoroutines = 10
		var wg sync.WaitGroup
		wg.Add(numGoroutines * 2)

		// Use channels to synchronize: signal only after registration is complete
		registered := make([]chan struct{}, numGoroutines)
		for i := range registered {
			registered[i] = make(chan struct{})
		}

		for i := uint64(0); i < numGoroutines; i++ {
			go func() {
				defer wg.Done()
				waitCh, cleanup, err := registry.RegisterWaiter(i)
				if err != nil {
					close(registered[i])
					return
				}
				defer cleanup()
				close(registered[i]) // Signal that registration is complete
				<-waitCh
			}()

			go func() {
				defer wg.Done()
				<-registered[i] // Wait for registration to complete before signaling
				registry.Signal(i, nil)
			}()
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Success
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for concurrent operations")
		}
	})

	t.Run("reuse transaction ID after cleanup", func(t *testing.T) {
		t.Parallel()

		registry := NewPostReceiveRegistry()

		waitCh1, cleanup1, err := registry.RegisterWaiter(1)
		require.NoError(t, err)

		registry.Signal(1, nil)
		<-waitCh1
		cleanup1()

		// Should be able to register same ID again
		waitCh2, cleanup2, err := registry.RegisterWaiter(1)
		require.NoError(t, err)
		defer cleanup2()

		registry.Signal(1, nil)

		select {
		case <-waitCh2:
			// Success
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for signal")
		}
	})
}

func TestPostReceiveRegistry_CleanupBeforeSignal(t *testing.T) {
	t.Parallel()

	registry := NewPostReceiveRegistry()

	waitCh, cleanup, err := registry.RegisterWaiter(1)
	require.NoError(t, err)

	// Cleanup before signal - simulates timeout scenario
	cleanup()

	// Channel should never be signaled
	select {
	case <-waitCh:
		t.Fatal("channel should not be closed after cleanup")
	case <-time.After(50 * time.Millisecond):
		// Expected - channel remains open
	}

	// Signal after cleanup should be a no-op for the original channel.
	// The signal is buffered internally, but since transaction IDs are never
	// reused in practice this is harmless.
	registry.Signal(1, nil)

	select {
	case <-waitCh:
		t.Fatal("channel should not be closed after cleanup")
	default:
		// Expected
	}
}
