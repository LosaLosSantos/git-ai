package stats

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func pktLine(tb testing.TB, s string) []byte {
	tb.Helper()
	require.LessOrEqual(tb, len(s)+4, 0xffff)
	return fmt.Appendf(nil, "%04x%s", len(s)+4, s)
}

func packHeader(tb testing.TB, objects uint32) []byte {
	tb.Helper()
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], objects)
	return hdr
}

var flush = []byte("0000")

func TestReceivePack_Parse(t *testing.T) {
	const (
		zero = "0000000000000000000000000000000000000000"
		oidA = "1111111111111111111111111111111111111111"
		oidB = "2222222222222222222222222222222222222222"
	)

	for _, tc := range []struct {
		desc     string
		request  func(t *testing.T) []byte
		expected ReceivePack
	}{
		{
			desc: "single update with packfile",
			request: func(t *testing.T) []byte {
				return slices.Concat(
					pktLine(t, fmt.Sprintf("%s %s refs/heads/main\x00report-status side-band-64k agent=git/2.45.0", oidA, oidB)),
					flush,
					packHeader(t, 5),
					[]byte("packfile-body-bytes"),
				)
			},
			expected: ReceivePack{
				UserAgent: "git/2.45.0",
				Caps:      []string{"report-status", "side-band-64k", "agent=git/2.45.0"},
				Commands:  1,
				Updates:   1,
				Objects:   5,
				Packets:   1,
			},
		},
		{
			desc: "mixed create, update and delete",
			request: func(t *testing.T) []byte {
				return slices.Concat(
					pktLine(t, fmt.Sprintf("%s %s refs/heads/main\x00report-status", oidA, oidB)),
					pktLine(t, fmt.Sprintf("%s %s refs/heads/new", zero, oidB)),
					pktLine(t, fmt.Sprintf("%s %s refs/heads/old", oidA, zero)),
					flush,
					packHeader(t, 12),
				)
			},
			expected: ReceivePack{
				Caps:     []string{"report-status"},
				Commands: 3,
				Creates:  1,
				Updates:  1,
				Deletes:  1,
				Objects:  12,
				Packets:  3,
			},
		},
		{
			desc: "delete only carries no packfile",
			request: func(t *testing.T) []byte {
				return slices.Concat(
					pktLine(t, fmt.Sprintf("%s %s refs/heads/old\x00report-status", oidA, zero)),
					flush,
				)
			},
			expected: ReceivePack{
				Caps:     []string{"report-status"},
				Commands: 1,
				Deletes:  1,
				Objects:  0,
				Packets:  1,
			},
		},
		{
			desc: "signed push certificate is skipped",
			request: func(t *testing.T) []byte {
				return slices.Concat(
					pktLine(t, "push-cert\x00report-status"),
					pktLine(t, "certificate version 0.1"),
					pktLine(t, "pusher A U Thor <author@example.com> 1234567890 +0000"),
					pktLine(t, "pushee https://example.com/repo.git"),
					pktLine(t, "nonce 1234567890-abcdef"),
					pktLine(t, ""),
					pktLine(t, fmt.Sprintf("%s %s refs/heads/main", oidA, oidB)),
					pktLine(t, "-----BEGIN PGP SIGNATURE-----"),
					pktLine(t, "signature data here"),
					pktLine(t, "-----END PGP SIGNATURE-----"),
					pktLine(t, "push-cert-end"),
					flush,
					packHeader(t, 4),
				)
			},
			// The cert wraps the real command, so we count no commands but
			// still read the object count from the packfile header.
			expected: ReceivePack{
				Objects: 4,
				Packets: 11,
			},
		},
		{
			desc: "shallow lines are ignored",
			request: func(t *testing.T) []byte {
				return slices.Concat(
					pktLine(t, fmt.Sprintf("shallow %s", oidA)),
					pktLine(t, fmt.Sprintf("%s %s refs/heads/main\x00report-status", oidA, oidB)),
					flush,
					packHeader(t, 3),
				)
			},
			expected: ReceivePack{
				Caps:     []string{"report-status"},
				Commands: 1,
				Updates:  1,
				Objects:  3,
				Packets:  2,
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			actual, err := ParseReceivePack(bytes.NewReader(tc.request(t)))
			require.NoError(t, err)

			require.Equal(t, tc.expected, actual)
		})
	}
}

func TestReceivePack_Parse_emptyStream(t *testing.T) {
	// An empty stream is what the parser sees when the RPC fails before
	// git-receive-pack reads its input.
	rp, err := ParseReceivePack(bytes.NewReader(nil))
	require.NoError(t, err)
	require.Equal(t, ReceivePack{}, rp)
}

func TestReceivePack_Parse_incompleteCommandList(t *testing.T) {
	// A command list ending before its flush packet is what the parser sees
	// when the captured prefix is too short.
	const oidA = "1111111111111111111111111111111111111111"
	const oidB = "2222222222222222222222222222222222222222"

	_, err := ParseReceivePack(bytes.NewReader(pktLine(t, fmt.Sprintf("%s %s refs/heads/main", oidA, oidB))))
	require.ErrorContains(t, err, "incomplete command list")
}

func TestRecordReceivePack(t *testing.T) {
	const oidA = "1111111111111111111111111111111111111111"
	const oidB = "2222222222222222222222222222222222222222"

	for _, tc := range []struct {
		desc            string
		prefix          func(t *testing.T) []byte
		expectedSamples int
	}{
		{
			desc: "empty stream records nothing",
			prefix: func(t *testing.T) []byte {
				return nil
			},
			expectedSamples: 0,
		},
		{
			desc: "truncated command list records nothing",
			prefix: func(t *testing.T) []byte {
				return pktLine(t, fmt.Sprintf("%s %s refs/heads/main", oidA, oidB))
			},
			expectedSamples: 0,
		},
		{
			desc: "unsigned push is recorded",
			prefix: func(t *testing.T) []byte {
				return slices.Concat(
					pktLine(t, fmt.Sprintf("%s %s refs/heads/main\x00report-status", oidA, oidB)),
					flush,
					packHeader(t, 3),
				)
			},
			expectedSamples: 1,
		},
		{
			desc: "signed push is recorded despite zero commands",
			prefix: func(t *testing.T) []byte {
				return slices.Concat(
					pktLine(t, "push-cert\x00report-status"),
					pktLine(t, "certificate version 0.1"),
					pktLine(t, fmt.Sprintf("%s %s refs/heads/main", oidA, oidB)),
					pktLine(t, "push-cert-end"),
					flush,
					packHeader(t, 7),
				)
			},
			expectedSamples: 1,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := log.InitContextCustomFields(testhelper.Context(t))
			histogram := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "test_record_receive_pack_objects"})

			RecordReceivePack(ctx, testhelper.SharedLogger(t), tc.prefix(t), histogram)

			testhelper.RequireHistogramSampleCounts(t, histogram, map[string]int{
				"test_record_receive_pack_objects": tc.expectedSamples,
			})
		})
	}
}

func TestPrefixCapture(t *testing.T) {
	c := &PrefixCapture{}

	n, err := c.Write([]byte("hello"))
	require.NoError(t, err)
	require.Equal(t, 5, n)

	// Writes past the limit must still report the full length, or an
	// io.MultiWriter would abort on a short write.
	big := bytes.Repeat([]byte("x"), ReceivePackStatsPrefixSize)
	n, err = c.Write(big)
	require.NoError(t, err)
	require.Equal(t, len(big), n)

	require.Len(t, c.Bytes(), ReceivePackStatsPrefixSize)
	require.Equal(t, "hello", string(c.Bytes()[:5]))
}

func TestReceivePack_UpdateMetrics(t *testing.T) {
	histogram := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "test_receive_pack_objects",
		Buckets: []float64{10, 100, 1000},
	})

	rp := ReceivePack{Objects: 42}
	rp.UpdateMetrics(histogram)

	require.NoError(t, testutil.CollectAndCompare(histogram, strings.NewReader(`
# HELP test_receive_pack_objects 
# TYPE test_receive_pack_objects histogram
test_receive_pack_objects_bucket{le="10"} 0
test_receive_pack_objects_bucket{le="100"} 1
test_receive_pack_objects_bucket{le="1000"} 1
test_receive_pack_objects_bucket{le="+Inf"} 1
test_receive_pack_objects_sum 42
test_receive_pack_objects_count 1
`)))
}

func TestReceivePack_Parse_invalidPackHeader(t *testing.T) {
	const oidA = "1111111111111111111111111111111111111111"
	const oidB = "2222222222222222222222222222222222222222"

	request := slices.Concat(
		pktLine(t, fmt.Sprintf("%s %s refs/heads/main\x00report-status", oidA, oidB)),
		flush,
		[]byte("NOPEbogus-header"),
	)

	_, err := ParseReceivePack(bytes.NewReader(request))
	require.ErrorContains(t, err, "unexpected packfile signature")
}
