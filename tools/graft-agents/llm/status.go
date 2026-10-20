package llm

import "errors"

// StatusError is implemented by the errors of providers for an answer of an HTTP API with an
// error status.
type StatusError interface {
	error
	// HTTPStatus is the status of the answer.
	HTTPStatus() int
	// BeforeGeneration reports whether the answer came before the model generated anything: the
	// status of the response to the request, not an error in the middle of a stream.
	BeforeGeneration() bool
}

// HTTPStatus returns the status of the provider's answer that err carries, if it carries one.
func HTTPStatus(err error) (int, bool) {
	var se StatusError
	if !errors.As(err, &se) {
		return 0, false
	}
	return se.HTTPStatus(), true
}

// Unbilled reports whether err is a refusal of the call that the provider does not bill: an
// answer before generation with the status 400 (a bad request), 401 (no or a wrong key), 402 (no
// credit) or 403 (forbidden). Every other failure may have been billed: 429 and 5xx answers, a
// timeout, a broken connection, an error in the middle of a stream, or a provider that reports no
// status (such as a command).
func Unbilled(err error) bool {
	var se StatusError
	if !errors.As(err, &se) || !se.BeforeGeneration() {
		return false
	}
	switch se.HTTPStatus() {
	case 400, 401, 402, 403:
		return true
	}
	return false
}
