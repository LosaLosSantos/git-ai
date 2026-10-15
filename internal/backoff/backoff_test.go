package backoff

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExponentialBackoff(t *testing.T) {
	t.Parallel()

	backoff := NewDefaultExponential(rand.New(rand.NewSource(time.Now().UnixNano())))
	testcases := []struct {
		retries         uint
		expectedBackoff time.Duration
	}{
		{retries: 0, expectedBackoff: 1000 * time.Millisecond},
		{retries: 1, expectedBackoff: 1500 * time.Millisecond},
		{retries: 2, expectedBackoff: 2250 * time.Millisecond},
		{retries: 3, expectedBackoff: 3375 * time.Millisecond},
		{retries: 4, expectedBackoff: 5062 * time.Millisecond},
		{retries: 5, expectedBackoff: 7593 * time.Millisecond},
		{retries: 6, expectedBackoff: 11390 * time.Millisecond},
		{retries: 7, expectedBackoff: 17085 * time.Millisecond},
		{retries: 8, expectedBackoff: 25628 * time.Millisecond},
		{retries: 9, expectedBackoff: 38443 * time.Millisecond},
		{retries: 10, expectedBackoff: 57665 * time.Millisecond},
		{retries: 11, expectedBackoff: 60000 * time.Millisecond},
		{retries: 12, expectedBackoff: 60000 * time.Millisecond},
		{retries: 13, expectedBackoff: 60000 * time.Millisecond},
		{retries: 14, expectedBackoff: 60000 * time.Millisecond},
		{retries: 15, expectedBackoff: 60000 * time.Millisecond},
		// This should not consume CPU at all
		{retries: 99999999, expectedBackoff: 60000 * time.Millisecond},
	}

	for _, tc := range testcases {
		for i := 0; i < 10; i++ {
			// Add a margin to prevent rounding error leading to flaky test
			jitter := 2 * float64(backoff.Jitter)
			require.InDelta(t, tc.expectedBackoff, backoff.Backoff(tc.retries), jitter)
		}
	}
}
