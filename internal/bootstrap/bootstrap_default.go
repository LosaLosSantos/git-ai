package bootstrap

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/feder-cr/git-ai/internal/helper"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/prometheus/client_golang/prometheus"
)

// defaultBootstrap is a Bootstrap that does no additional configurations.
// As an example, it does not perform graceful upgrades.
type defaultBootstrap struct {
	starters   []Starter
	errChan    chan error
	logger     log.Logger
	connTotal  *prometheus.CounterVec
	shutdownCh chan os.Signal
}

// newDefaultBootstrap returns a defaultBootstrap.
func newDefaultBootstrap(logger log.Logger, connTotal *prometheus.CounterVec) *defaultBootstrap {
	shutdownCh := make(chan os.Signal, 3)
	signal.Notify(shutdownCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	return &defaultBootstrap{
		logger:     logger,
		connTotal:  connTotal,
		shutdownCh: shutdownCh,
	}
}

// RegisterStarter adds a starter to the pool.
func (d *defaultBootstrap) RegisterStarter(starter Starter) {
	d.starters = append(d.starters, starter)
}

// Start starts all registered starters to accept connections.
func (d *defaultBootstrap) Start() error {
	d.errChan = make(chan error, len(d.starters))
	for _, start := range d.starters {
		if err := start(d.listen, d.errChan, d.connTotal); err != nil {
			return err
		}
	}
	return nil
}

// Wait will wait until one of those conditions happen:
// 1. A shutdown signal has been received, in which case Gitaly is stopped, not restarted.
// 3. An error has been received on the errCh while starting the servers.
func (d *defaultBootstrap) Wait(gracePeriodTicker helper.Ticker, stopAction func()) error {
	pidFile := os.Getenv(EnvPidFile)
	if pidFile != "" {
		if err := writePidToFile(pidFile); err != nil {
			return fmt.Errorf("error writing pid to file %q: %w", pidFile, err)
		}
		fmt.Printf("PID printed to file %q\n", pidFile)
	}

	select {
	case s := <-d.shutdownCh:
		fl := d.logger.WithField("signal", s.String())
		fl.Info("[shutdown] stopping Gitaly after receiving signal")
		if waitError := waitGracePeriod(d.logger, gracePeriodTicker, d.shutdownCh, stopAction); waitError != nil {
			return fmt.Errorf("received signal %q: wait: %w", s, waitError)
		}
		fl.Info("[shutdown] Gitaly stopped after receiving signal")
	case err := <-d.errChan:
		return err
	}
	return nil
}

// listen is a wrapper around net.Listen to allow for additional logic.
// In the tableflip implementation, the corollary of this method also detects
// if the current process is a first invocation or the result of a tableflip upgrade.
// In the `defaultBootstrap`, there is no concept of "upgrade", and every process is
// a first-time invocation, so we always clean up the previous sockets.
func (d *defaultBootstrap) listen(network, path string) (net.Listener, error) {
	if network == "unix" {
		if err := os.RemoveAll(path); err != nil {
			return nil, err
		}
	}
	//nolint: noctx
	return net.Listen(network, path)
}
