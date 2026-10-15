package stream

import (
	"bytes"
	"io"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/pktline"
	"github.com/stretchr/testify/require"
)

// fakeConn implements Conn. The response (server -> client) is read from the
// embedded reader, while the request (client -> server) is captured in a
// buffer.
//
// ProxyPktLine returns as soon as the response goroutine finishes, without
// waiting for the request goroutine. The writeDone channel lets the test wait
// for the request copy to complete before reading request, which both avoids a
// scheduling race on the assertion and provides the happens-before edge that
// keeps the request buffer free of data races.
type fakeConn struct {
	io.Reader
	request   bytes.Buffer
	writeDone chan struct{}
}

func (c *fakeConn) Write(p []byte) (int, error) { return c.request.Write(p) }

func (c *fakeConn) CloseWrite() error {
	close(c.writeDone)
	return nil
}

// countingWriter records how many times Write is called so we can assert that
// sideband packets are coalesced into fewer writes.
type countingWriter struct {
	w      io.Writer
	writes int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	return c.w.Write(p)
}

func TestProxyPktLine(t *testing.T) {
	t.Parallel()

	// Build a multi-megabyte stdout payload so it spans many sideband packets,
	// each of which is capped at pktline.MaxSidebandData (~64KB).
	stdoutPayload := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1MiB
	stderrPayload := []byte("a progress message on stderr")
	packetCount := (len(stdoutPayload) + pktline.MaxSidebandData - 1) / pktline.MaxSidebandData

	for _, tc := range []struct {
		desc         string
		bufferStdout bool
		// expectedWrites is asserted as an upper bound on the number of Write
		// calls made into stdout.
		expectedWrites int
	}{
		{
			desc:         "unbuffered",
			bufferStdout: false,
			// Without buffering each sideband packet results in its own write.
			expectedWrites: packetCount,
		},
		{
			desc:         "buffered",
			bufferStdout: true,
			// With buffering the packets are coalesced into the buffer and only
			// flushed once it fills up (plus a possible final flush).
			expectedWrites: len(stdoutPayload)/stdoutBufferSize + 1,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			var response bytes.Buffer
			sw := pktline.NewSidebandWriter(&response)
			_, err := sw.Writer(BandStdout).Write(stdoutPayload)
			require.NoError(t, err)
			_, err = sw.Writer(BandStderr).Write(stderrPayload)
			require.NoError(t, err)

			conn := &fakeConn{Reader: &response, writeDone: make(chan struct{})}

			var stdout, stderr bytes.Buffer
			countedStdout := &countingWriter{w: &stdout}

			stdin := bytes.NewReader([]byte("the pack-objects request"))

			require.NoError(t, ProxyPktLine(conn, stdin, countedStdout, &stderr, tc.bufferStdout))

			// The relayed bytes must be byte-for-byte identical to what the
			// server sent, regardless of buffering.
			require.Equal(t, stdoutPayload, stdout.Bytes())
			require.Equal(t, stderrPayload, stderr.Bytes())

			// ProxyPktLine returns once the response is drained, so wait for the
			// request goroutine to finish copying stdin before reading request.
			<-conn.writeDone

			// The request side (stdin) must be forwarded to the connection unchanged.
			require.Equal(t, "the pack-objects request", conn.request.String())

			require.LessOrEqual(t, countedStdout.writes, tc.expectedWrites)
			if tc.bufferStdout {
				require.Less(t, countedStdout.writes, packetCount,
					"expected sideband packets to be coalesced into fewer stdout writes")
			}
		})
	}
}
