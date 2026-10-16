package bootstrap

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/helper"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestDefaultBootstrap_starterError(t *testing.T) {
	logger := testhelper.NewLogger(t)
	b := newDefaultBootstrap(logger, &prometheus.CounterVec{})

	listeners := mockListeners{}
	start := func(network, address string) Starter {
		listeners[network] = &mockListener{}
		return func(listen ListenFunc, errors chan<- error, _ *prometheus.CounterVec) error {
			listeners[network].errorCh = errors
			listeners[network].listening = true
			return nil
		}
	}

	for network, address := range map[string]string{
		"tcp":  "127.0.0.1:0",
		"unix": "some-socket",
	} {
		b.RegisterStarter(start(network, address))
	}

	err := b.Start()
	require.NoError(t, err)

	waitCh := make(chan error)
	go func() {
		waitCh <- b.Wait(helper.NewManualTicker(), nil)
	}()

	listeners["tcp"].errorCh <- errors.New("some error")

	waitErr := <-waitCh
	require.Equal(t, errors.New("some error"), waitErr)
}

func Test_newDefaultBootstrap_Wait(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {

		t.Run(fmt.Sprintf("%s/no_action", sig), func(t *testing.T) {
			logger := testhelper.NewLogger(t)
			b := newDefaultBootstrap(logger, &prometheus.CounterVec{})

			err := b.Start()
			require.NoError(t, err)

			ticker := helper.NewManualTicker()
			waitCh := make(chan error)
			go func() {
				waitCh <- b.Wait(ticker, nil)
			}()

			self, err := os.FindProcess(os.Getpid())
			require.NoError(t, err)
			require.NoError(t, self.Signal(sig))

			require.Nil(t, <-waitCh)
		})

		t.Run(fmt.Sprintf("%s/with_action_timeout", sig), func(t *testing.T) {
			logger := testhelper.NewLogger(t)
			b := newDefaultBootstrap(logger, &prometheus.CounterVec{})

			err := b.Start()
			require.NoError(t, err)

			ticker := helper.NewManualTicker()

			stopActionFn := func() {
				// This is a way to simulate the gracefulShutdown timeout
				// to expire.
				ticker.Tick()
			}
			waitCh := make(chan error)
			go func() {
				waitCh <- b.Wait(ticker, stopActionFn)
			}()

			self, err := os.FindProcess(os.Getpid())
			require.NoError(t, err)
			require.NoError(t, self.Signal(sig))

			require.ErrorIs(t, <-waitCh, ErrGracePeriodExpired)
		})

		t.Run(fmt.Sprintf("%s/pid_written", sig), func(t *testing.T) {
			logger := testhelper.NewLogger(t)

			// Set the PID file and the environment variable
			tempDir := testhelper.TempDir(t)
			pidFile := filepath.Join(tempDir, "pid_file")
			t.Setenv(EnvPidFile, pidFile)

			b := newDefaultBootstrap(logger, &prometheus.CounterVec{})

			err := b.Start()
			require.NoError(t, err)

			// Call Wait in a goroutine
			// This should write the current process's PID
			// to the pid file
			ticker := helper.NewManualTicker()
			waitCh := make(chan error)
			go func() {
				waitCh <- b.Wait(ticker, nil)
			}()

			// Get the current process's PID
			self, err := os.FindProcess(os.Getpid())
			require.NoError(t, err)

			// Send a signal to the process to trigger Wait to return
			require.NoError(t, self.Signal(sig))

			// Block until Wait returns
			require.Nil(t, <-waitCh)

			// Assert that the PID file has been updated with the new value
			pidContent, err := os.ReadFile(pidFile)
			require.NoError(t, err)
			require.Equal(t, string(pidContent), fmt.Sprintf("%v", os.Getpid()))
		})
	}
}

func Test_defaultBootstrap_Start(t *testing.T) {
	t.Run("old unix socket is replaced", func(t *testing.T) {
		logger := testhelper.NewLogger(t)
		b := newDefaultBootstrap(logger, &prometheus.CounterVec{})

		tempDir := testhelper.TempDir(t)
		socketPath := filepath.Join(tempDir, "gitaly-test-unix-socket")

		// Create a pre-existing file at the socket path. We use a regular file
		// rather than a real socket so we can later assert the path is now a
		// socket (and therefore has been removed and re-created).
		require.NoError(t, os.WriteFile(socketPath, nil, mode.File))

		preStat, err := os.Stat(socketPath)
		require.NoError(t, err)
		require.Zero(t, preStat.Mode()&os.ModeSocket, "precondition: path should not yet be a socket")

		// Keep a reference to the listener here so we can close it
		// after the test
		var listener net.Listener
		b.RegisterStarter(func(listen ListenFunc, _ chan<- error, _ *prometheus.CounterVec) error {
			l, err := listen("unix", socketPath)
			if err != nil {
				return err
			}
			listener = l
			return nil
		})

		require.NoError(t, b.Start())
		t.Cleanup(func() { _ = listener.Close() })

		postStat, err := os.Stat(socketPath)
		require.NoError(t, err)
		require.NotZero(t, postStat.Mode()&os.ModeSocket, "path should now be a unix socket")
	})
}
