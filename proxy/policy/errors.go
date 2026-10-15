package policy

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// ErrNoTargetRepository indicates that no target repository annotation was populated.
	ErrNoTargetRepository = errors.New("no target repository")
	// ErrAmbiguousTargetRepository indicates that multiple target repository fields were populated.
	ErrAmbiguousTargetRepository = errors.New("ambiguous target repository")
	// ErrMalformedRequest indicates that the first request frame could not be decoded.
	ErrMalformedRequest = errors.New("malformed request")
)

// InvariantError is a stable, machine-readable proxy policy failure.
type InvariantError struct {
	reason string
	cause  error
}

// Error returns the wire-visible proxy reason.
func (e *InvariantError) Error() string { return "proxy: " + e.reason }

// Unwrap exposes the underlying extraction error, when present.
func (e *InvariantError) Unwrap() error { return e.cause }

// GRPCStatus maps policy failures to PermissionDenied.
func (e *InvariantError) GRPCStatus() *status.Status {
	return status.New(codes.PermissionDenied, e.Error())
}

// Reason returns the stable denial reason without the proxy prefix.
func (e *InvariantError) Reason() string { return e.reason }

func invariantError(reason string, cause error) error {
	return &InvariantError{reason: reason, cause: cause}
}

// Denial is the exported constructor for host-originated policy failures;
// package checks use invariantError as their local shorthand.
func Denial(reason string, cause error) error {
	return invariantError(reason, cause)
}
