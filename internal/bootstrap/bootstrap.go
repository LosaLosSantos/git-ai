package bootstrap

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/cloudflare/tableflip"
	"github.com/feder-cr/open_scion/internal/helper"
	"github.com/feder-cr/open_scion/internal/helper/env"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"
)

const (
	// EnvPidFile is the name of the environment variable containing the pid file path
	EnvPidFile = "GITALY_PID_FILE"
	// EnvUpgradesEnabled is an environment variable that when defined gitaly must enable graceful upgrades on SIGHUP
	EnvUpgradesEnabled     = "GITALY_UPGRADES_ENABLED"
	socketReusePortWarning = "Unable to set SO_REUSEPORT: zero downtime upgrades will not work"
)

var (
	// ErrGracePeriodExpired is returned when the grace period for graceful shutdown expires.
	ErrGracePeriodExpired = errors.New("grace period expired")
	// ErrForceShutdown is returned when a force shutdown signal is received during grace period.
	ErrForceShutdown = errors.New("force shutdown")
)

// ListenFunc is a net.Listener factory
type ListenFunc func(net, addr string) (net.Listener, error)

// Starter is function to initialize a net.Listener
// it receives a ListenFunc to be used for net.Listener creation and a chan<- error to signal runtime errors
// It must serve incoming connections asynchronously and signal errors on the channel
// the return value is for setup errors
type Starter func(ListenFunc, chan<- error, *prometheus.CounterVec) error

// Bootstrap is an interface of the bootstrap manager.
type Bootstrap interface {
	// RegisterStarter adds starter to the pool.
	RegisterStarter(starter Starter)
	// Start starts all registered starters to accept connections.
	Start() error
	// Wait terminates all registered starters.
	Wait(gracePeriodTicker helper.Ticker, stopAction func()) error
}

// NewBootstrap returns a new bootstrap used to start the various servers (Starters)
func NewBootstrap(ctx context.Context, logger log.Logger, totalConn *prometheus.CounterVec) (Bootstrap, error) {
	upgradesEnabled, _ := env.GetBool(EnvUpgradesEnabled, false)
	if upgradesEnabled {
		// Create the tableflip upgrader
		upg, err := tableflip.New(tableflip.Options{
			PIDFile: os.Getenv(EnvPidFile),
			ListenConfig: &net.ListenConfig{
				Control: func(network, address string, c syscall.RawConn) error {
					var opErr error
					err := c.Control(func(fd uintptr) {
						opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
					})
					if err != nil {
						logger.WithError(err).Warn(socketReusePortWarning)
					}
					if opErr != nil {
						logger.WithError(opErr).Warn(socketReusePortWarning)
					}
					return nil
				},
			},
		})
		if err != nil {
			return nil, err
		}
		return newTableFlipBootstrap(ctx, logger, totalConn, upg, upg.Listen)
	}
	return newDefaultBootstrap(logger, totalConn), nil
}

func waitGracePeriod(logger log.Logger, gracePeriodTicker helper.Ticker, kill <-chan os.Signal, stopAction func()) error {
	logger.Warn("starting grace period")

	// This channel emits a value when stopAction() is done
	stopActionDone := make(chan struct{})
	go func() {
		defer close(stopActionDone)
		if stopAction != nil {
			stopAction()
		}
		logger.Info("wait: stop action completed")
	}()

	gracePeriodTicker.Reset()

	select {
	case <-gracePeriodTicker.C():
		logger.Info("wait: grace period ticker ticked")
		return ErrGracePeriodExpired
	case s := <-kill:
		logger.WithField("signal", s.String()).Info("wait: second signal received; force shutdown")
		return ErrForceShutdown
	case <-stopActionDone:
		logger.Info("wait: stopAction() has completed")
		return nil
	}
}

// writePidToFile is used by the defaultBootstrap to write the current PID
// to a file. This behavior is included in `tableflip` library but we
// need to manually do it when tableflip is not used.
// To avoid any regression, this function is a shamelessm copy-paste
// of the tableflip's implementation at:
// https://github.com/cloudflare/tableflip/blob/v1.2.3/upgrader.go#L288-L315
func writePidToFile(path string) error {
	initialWD, _ := os.Getwd()
	dir, file := filepath.Split(path)

	// if dir is empty, the user probably specified just the name
	// of the pid file expecting it to be created in the current work directory
	if dir == "" {
		dir = initialWD
	}

	if dir == "" {
		return errors.New("empty initial working directory")
	}

	fh, err := os.CreateTemp(dir, file)
	if err != nil {
		return err
	}
	defer fh.Close()
	// Remove temporary PID file if something fails
	//nolint: errcheck
	defer os.Remove(fh.Name())

	_, err = fh.WriteString(strconv.Itoa(os.Getpid()))
	if err != nil {
		return err
	}

	return os.Rename(fh.Name(), path)
}
