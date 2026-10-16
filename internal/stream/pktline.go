package stream

import (
	"bufio"
	"fmt"
	"io"

	"github.com/feder-cr/git-ai/internal/git/pktline"
)

// Conn represents a bi-directional client side connection.
type Conn interface {
	io.Reader
	io.Writer
	CloseWrite() error
}

// BandStdout and BandStderr are the pktline band ID's for stdout and
// stderr, respectively.
const (
	BandStdout = 1
	BandStderr = 2

	// stdoutBufferSize is the size of the buffer used to batch sideband packets
	// before writing them to stdout. Each sideband packet carries at most
	// pktline.MaxSidebandData (~64KB) bytes, so without buffering a large
	// payload (such as a packfile served from the pack-objects cache) results
	// in one write syscall per packet. Buffering combines multiple packets into
	// fewer, larger writes, which significantly reduces the number of writes
	// into the stdout pipe and the lock contention that comes with them.
	stdoutBufferSize = 256 * 1024
)

// ProxyPktLine lets a client de-multiplex and proxy stdin, stdout and
// stderr streams. Stdout and stderr are interleaved with Git's pktline
// format.
//
// If bufferStdout is true, the sideband packets destined for stdout are
// buffered and written out in fewer, larger writes, which reduces the number of
// write syscalls (and the lock contention they cause) for large payloads. This
// must only be enabled for one-way bulk transfers, such as serving a packfile
// from the pack-objects cache. It must NOT be enabled for interactive sessions
// that negotiate over the same stream (for example git fetch), because
// withholding the server's responses inside the buffer would stall the protocol
// and deadlock the session.
func ProxyPktLine(c Conn, stdin io.Reader, stdout, stderr io.Writer, bufferStdout bool) error {
	copyRequest := func(c Conn) error {
		if _, err := io.Copy(c, stdin); err != nil {
			return fmt.Errorf("copy request: %w", err)
		}
		if err := c.CloseWrite(); err != nil {
			return fmt.Errorf("close request: %w", err)
		}
		return nil
	}

	copyResponse := func(c Conn) (err error) {
		// Optionally batch sideband packets into fewer, larger writes to stdout.
		// This reduces the number of writes into the stdout pipe, which matters
		// in particular when serving a packfile from the pack-objects cache: in
		// that case Git's own write batching does not apply because
		// git-pack-objects(1) is never run.
		out := stdout
		if bufferStdout {
			buffered := bufio.NewWriterSize(stdout, stdoutBufferSize)
			out = buffered
			defer func() {
				// Only surface the flush error if the copy itself succeeded, so
				// we don't mask the original failure.
				if flushErr := buffered.Flush(); flushErr != nil && err == nil {
					err = fmt.Errorf("flush response: %w", flushErr)
				}
			}()
		}

		if err := pktline.EachSidebandPacket(c, func(band byte, data []byte) (err error) {
			switch band {
			case BandStdout:
				_, err = out.Write(data)
			case BandStderr:
				_, err = stderr.Write(data)
			default:
				err = fmt.Errorf("unexpected band: %d", band)
			}
			return err
		}); err != nil {
			return fmt.Errorf("copy response: %w", err)
		}

		return nil
	}

	request := make(chan error, 1)
	go func() { request <- copyRequest(c) }()

	response := make(chan error, 1)
	go func() { response <- copyResponse(c) }()

	for {
		select {
		case err := <-response:
			// Server is done. No point in waiting for client.
			return err
		case err := <-request:
			if err != nil {
				return err
			}
			// Client is now done. Wait for server to finish too.
		}
	}
}
