package structerr

import (
	"errors"

	"google.golang.org/grpc/codes"
)

// GRPCCode translates errors into codes.Code values.
//
//   - If present, it will un-wrap the top-level error that implements the `GRPCStatus()` function
//     and return its error code. As we internally use `structerr`s to propagate gRPC error codes
//     the error code propagation semantics match what the `structerr` is doing in that case.
//   - If no error is found in the chain that implements `GRPCStatus()` then `codes.Unknown` is
//     returned.
//   - If err is nil then `codes.OK` is returned.
//
// Semantics of this function thus match what the calling-side of an RPC would see if the given
// error was returned from that RPC.
func GRPCCode(err error) codes.Code {
	var status grpcStatuser

	switch {
	case err == nil:
		return codes.OK
	case errors.As(err, &status):
		return status.GRPCStatus().Code()
	default:
		return codes.Unknown
	}
}
