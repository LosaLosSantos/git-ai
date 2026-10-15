package log

import (
	"io"

	"github.com/sirupsen/logrus"
)

// newLogger creates a new logger for testing purposes. Use of `logrus.New()` is forbidden globally, but required here
// to verify that we correctly configure our logging infrastructure.
//
//nolint:forbidigo
func newLogger() *logrus.Logger {
	logger := logrus.New()
	logger.Out = io.Discard
	return logger
}
