package structerr

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGRPCCode(t *testing.T) {
	t.Parallel()

	for desc, tc := range map[string]struct {
		in  error
		exp codes.Code
	}{
		"unwrapped status": {
			in:  status.Error(codes.NotFound, ""),
			exp: codes.NotFound,
		},
		"wrapped status": {
			in:  fmt.Errorf("context: %w", status.Error(codes.NotFound, "")),
			exp: codes.NotFound,
		},
		"unwrapped status created by helpers": {
			in:  NewNotFound(""),
			exp: codes.NotFound,
		},
		"wrapped status created by helpers": {
			in:  fmt.Errorf("context: %w", NewNotFound("")),
			exp: codes.NotFound,
		},
		"double wrapped status created by helpers": {
			in:  fmt.Errorf("outer: %w", fmt.Errorf("context: %w", NewNotFound(""))),
			exp: codes.NotFound,
		},
		"double helper wrapped status": {
			in:  NewInvalidArgument("outer: %w", fmt.Errorf("context: %w", NewNotFound(""))),
			exp: codes.NotFound,
		},
		"nil input": {
			in:  nil,
			exp: codes.OK,
		},
		"no code defined": {
			in:  errors.New("an error"),
			exp: codes.Unknown,
		},
	} {
		t.Run(desc, func(t *testing.T) {
			require.Equal(t, tc.exp, GRPCCode(tc.in))
		})
	}
}
