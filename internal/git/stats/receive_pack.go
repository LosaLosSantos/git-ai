package stats

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/feder-cr/git-ai/internal/git/pktline"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/prometheus/client_golang/prometheus"
)

// ReceivePackStatsPrefixSize is the number of leading bytes of a
// git-receive-pack request that are captured to extract push statistics. The
// command list and the packfile header sit at the very start of the request.
// A push whose command list is larger than this loses its statistics.
const ReceivePackStatsPrefixSize = 64 * 1024

// ReceivePack parses the request a Git client sends to git-receive-pack(1)
// (i.e. the client side of a push). The request consists of a list of
// reference-update commands in pkt-line format, terminated by a flush packet,
// optionally followed by a packfile.
//
// See https://git-scm.com/docs/pack-protocol#_reference_update_request_and_packfile_transfer.
type ReceivePack struct {
	// UserAgent is the user agent announced by the client, e.g. "git/2.53.0".
	UserAgent string
	// Capabilities announced by the client (report-status, side-band-64k, ...).
	Caps []string
	// Commands is the number of reference-update commands in the request.
	Commands int
	// Creates is the number of references the client wants to create.
	Creates int
	// Deletes is the number of references the client wants to delete.
	Deletes int
	// Updates is the number of references the client wants to update.
	Updates int
	// Objects is the number of objects in the packfile as announced by the
	// PACK header. A delete-only push carries no packfile
	// and leaves this at zero.
	Objects uint32
	// Packets is the total number of pktlines in the command list. It
	// distinguishes an empty request from one whose command list was
	// truncated.
	Packets int
}

// ParseReceivePack parses a git-receive-pack(1) request from the given reader.
func ParseReceivePack(body io.Reader) (ReceivePack, error) {
	r := ReceivePack{}
	return r, r.Parse(body)
}

// Parse reads the reference-update commands and the packfile header from a
// git-receive-pack(1) request body.
func (r *ReceivePack) Parse(body io.Reader) error {
	capsParsed := false
	inPushCert := false
	sawFlush := false
	for {
		// Use pktline.Read rather than pktline.NewScanner because we should
		// strictly consume up to the end of the command-list in the Reference
		// Update Request.
		pkt, err := pktline.Read(body)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("reading command list: %w", err)
		}
		if pktline.IsFlush(pkt) {
			sawFlush = true
			break
		}

		r.Packets++
		line := text.ChompBytes(pktline.Data(pkt))

		// A push certificate (git push --signed) wraps the command list
		// together with header and PGP-signature lines. Skip the whole block
		// rather than miscount its contents as reference updates.
		if inPushCert {
			if strings.HasPrefix(line, "push-cert-end") {
				inPushCert = false
			}
			continue
		}
		if strings.HasPrefix(line, "push-cert") {
			inPushCert = true
			continue
		}

		// Shallow lines precede the command list.
		if strings.HasPrefix(line, "shallow ") {
			continue
		}

		// The first command line carries the capabilities after a NUL byte:
		//   <old-oid> SP <new-oid> SP <ref-name> NUL <capabilities>
		if !capsParsed {
			capsParsed = true
			if idx := strings.IndexByte(line, 0); idx >= 0 {
				r.Caps = strings.Fields(line[idx+1:])
				for _, cap := range r.Caps {
					if agent, ok := strings.CutPrefix(cap, "agent="); ok {
						r.UserAgent = agent
						break
					}
				}
				line = line[:idx]
			}
		}

		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 {
			continue
		}
		oldOID, newOID := fields[0], fields[1]

		r.Commands++
		switch {
		case isZeroOID(oldOID):
			r.Creates++
		case isZeroOID(newOID):
			r.Deletes++
		default:
			r.Updates++
		}
	}

	if !sawFlush {
		// An empty stream happens when the RPC fails before git-receive-pack
		// reads its input.
		if r.Packets == 0 {
			return nil
		}
		return fmt.Errorf("incomplete command list: no flush after %d packets", r.Packets)
	}

	// The packfile header is "PACK" + 4-byte version + 4-byte object count,
	// all big-endian. EOF means no packfile follows the command list.
	var header [12]byte
	if _, err := io.ReadFull(body, header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return fmt.Errorf("reading packfile header: %w", err)
	}

	if string(header[0:4]) != "PACK" {
		return fmt.Errorf("unexpected packfile signature: %q", header[0:4])
	}
	r.Objects = binary.BigEndian.Uint32(header[8:12])

	return nil
}

func isZeroOID(oid string) bool {
	for _, c := range oid {
		if c != '0' {
			return false
		}
	}
	return len(oid) > 0
}

// UpdateMetrics observes the push's object count in the given histogram. The
// histogram deliberately carries no labels: repository and user attribution
// stays in the logs to keep metric cardinality bounded.
func (r *ReceivePack) UpdateMetrics(objects prometheus.Histogram) {
	objects.Observe(float64(r.Objects))
}

// UpdateLogFields records the push statistics as custom log fields on the
// request.
func (r *ReceivePack) UpdateLogFields(ctx context.Context) {
	customFields := log.CustomFieldsFromContext(ctx)
	if customFields == nil {
		return
	}

	if r.UserAgent != "" {
		customFields.RecordMetadata("receive_pack.user_agent", r.UserAgent)
	}
	customFields.RecordMetadata("receive_pack.commands", r.Commands)
	customFields.RecordMetadata("receive_pack.creates", r.Creates)
	customFields.RecordMetadata("receive_pack.deletes", r.Deletes)
	customFields.RecordMetadata("receive_pack.updates", r.Updates)
	customFields.RecordMetadata("receive_pack.objects", int(r.Objects))

	log.FromContext(ctx).
		WithFields(customFields.Fields()).
		InfoContext(ctx, "receive-pack request parsed")
}

// PrefixCapture is an io.Writer that retains the first ReceivePackStatsPrefixSize
// bytes written to it and discards the rest. It never blocks or errors, so it
// can tee the git-receive-pack stdin path without affecting git.
type PrefixCapture struct {
	buf []byte
}

// Write retains bytes up to the prefix limit and reports the full length as
// written so the writer never short-writes.
func (c *PrefixCapture) Write(p []byte) (int, error) {
	if room := ReceivePackStatsPrefixSize - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// Bytes returns the captured prefix.
func (c *PrefixCapture) Bytes() []byte {
	return c.buf
}

// RecordReceivePack parses a captured prefix of a git-receive-pack request
// and, when it holds a complete command list, records push statistics as log
// fields and observes the object count in the histogram.
func RecordReceivePack(ctx context.Context, logger log.Logger, prefix []byte, objects prometheus.Histogram) {
	var rp ReceivePack
	if err := rp.Parse(bytes.NewReader(prefix)); err != nil {
		logger.WithError(err).DebugContext(ctx, "failed parsing receive-pack request")
		return
	}
	// No packets at all means the RPC failed before git read its input. A
	// signed push reports zero commands but still carries packets and an
	// object count, so don't guard on Commands here.
	if rp.Packets == 0 {
		return
	}

	rp.UpdateLogFields(ctx)
	rp.UpdateMetrics(objects)
}
