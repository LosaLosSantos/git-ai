package stats

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/feder-cr/open_scion/internal/git/pktline"
	"github.com/feder-cr/open_scion/internal/helper/text"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
)

// Pipeline IDs are database IDs. Bound them to signed 64-bit decimal string
// width so instrumentation does not log arbitrary-length numeric strings as
// pipeline refs.
var pipelineRefPrefixRegex = regexp.MustCompile(`^refs/pipelines/[0-9]{1,19}$`)

const (
	// maxSampleEntries bounds how many "want" OIDs and "want-ref" refs are retained for
	// logging. A handful is enough to identify what the client asked for, which distinguishes
	// an initial fetch from a lazy blob backfill.
	maxSampleEntries = 10
	// maxWantLen is the length of a SHA-256 hex OID, the longest OID a client can legitimately
	// want; a SHA-1 OID is 40 characters. A pktline carries up to pktline.MaxPktSize bytes of
	// client-controlled data, so bounding the number of entries alone would leave the size of a
	// log line up to the client.
	maxWantLen = 64
	// maxWantRefLen bounds a retained refname. Refnames have no length limit in the protocol,
	// so a full one is too long to log even though it is well-formed.
	maxWantRefLen = 40
	// truncationMarker is appended to a refname clipped to maxWantRefLen, so that a clipped
	// name is not read as the name the client actually sent.
	truncationMarker = "..."
)

// appendWant retains an OID for logging. Values longer than an OID are dropped
// rather than truncated: a client can put anything on a "want" line, and a
// clipped value is no more resolvable than no value at all.
func appendWant(sample []string, oid string) []string {
	if len(sample) >= maxSampleEntries || len(oid) > maxWantLen {
		return sample
	}

	// Clone so that a retained OID does not pin the packet it was cut from.
	return append(sample, strings.Clone(oid))
}

// appendWantRef retains a refname for logging, clipping one that is too long.
// Unlike an OID, a truncated refname is still worth logging: the leading path
// says which kind of ref the client asked for.
func appendWantRef(sample []string, ref string) []string {
	if len(sample) >= maxSampleEntries {
		return sample
	}

	if len(ref) <= maxWantRefLen {
		// Clone so that a retained refname does not pin the packet it was cut from.
		return append(sample, strings.Clone(ref))
	}

	clipped := ref[:maxWantRefLen]
	// Refnames may hold multi-byte characters, so back off to a rune boundary to keep the
	// clipped name valid UTF-8.
	for len(clipped) > 0 && !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}

	return append(sample, clipped+truncationMarker)
}

//nolint:revive // This is unintentionally missing documentation.
type PackfileNegotiation struct {
	// UserAgent is the user agent of the client, e.g. "git/2.53.0".
	UserAgent string
	// Total size of all pktlines' data
	PayloadSize int64
	// Total number of packets
	Packets int
	// Capabilities announced by the client
	Caps []string
	// Wants is the number of objects the client announced it wants
	Wants int
	// Haves is the number of objects the client announced it has
	Haves int
	// Shallows is the number of shallow boundaries announced by the client
	Shallows int
	// Deepen-filter. One of "deepen <depth>", "deepen-since <timestamp>", "deepen-not <ref>".
	Deepen string
	// Filter-spec specified by the client.
	Filter string
	// Commands is the command submitted by the Git client
	Command string
	// PipelineRefPrefixes is log-only instrumentation and is intentionally not exposed via ToProto.
	PipelineRefPrefixes int
	// WantsSample holds the first maxSampleEntries OIDs the client announced as "want"s, in the
	// order they were received. Initial fetches want commits while lazy backfills want blobs, so
	// the OIDs tell the two apart once resolved. It is log-only instrumentation and is
	// intentionally not exposed via ToProto.
	WantsSample []string
	// WantRefs is the number of refs the client announced it wants via "want-ref", which
	// protocol v2 clients may send instead of naming objects directly. Clients only do so when
	// the server advertises the "ref-in-want" feature, which requires uploadpack.allowRefInWant
	// and which Gitaly does not enable, so this is zero unless an operator turns it on. It is
	// log-only instrumentation and is intentionally not exposed via ToProto.
	WantRefs int
	// WantRefsSample holds the first maxSampleEntries refs from those "want-ref"s, in the order
	// they were received, each clipped to maxWantRefLen. It is kept apart from WantsSample
	// because a refname and an OID cannot be told apart once mixed into one log field. Its
	// length is capped and so is not a count: use WantRefs for that. It is log-only
	// instrumentation and is intentionally not exposed via ToProto.
	WantRefsSample []string
}

type parsePackfileNegotiationConfig struct {
	parseLsRefsPipelineRefPrefixes bool
}

// ParsePackfileNegotiationOption configures packfile negotiation parsing.
type ParsePackfileNegotiationOption func(*parsePackfileNegotiationConfig)

// WithLsRefsPipelineRefPrefixes enables parsing of ls-refs ref-prefixes targeting CI pipeline refs.
func WithLsRefsPipelineRefPrefixes() ParsePackfileNegotiationOption {
	return func(config *parsePackfileNegotiationConfig) {
		config.parseLsRefsPipelineRefPrefixes = true
	}
}

// CloneType categorises the clone based on attributes seen during the packfile
// negotiation stage.
func (n PackfileNegotiation) CloneType() string {
	switch {
	case n.Filter != "":
		// Client provided a filter spec and is performing a partial clone.
		return "partial"
	case n.Deepen != "" || n.Shallows > 0:
		// Client specified a depth or is fetching from a shallow clone.
		return "shallow"
	case n.Haves > 0:
		// Client has existing objects and is thus performing an incremental
		// fetch.
		return "fetch"
	default:
		// No filters. Also captures the edge case where clients set
		// fetch.negotiationAlgoritm=noop but that's okay.
		return "full"
	}
}

// ToProto converts PackfileNegotiation to its Protobuf representation.
func (n PackfileNegotiation) ToProto() *gitalypb.PackfileNegotiationStatistics {
	return &gitalypb.PackfileNegotiationStatistics{
		PayloadSize: n.PayloadSize,
		Packets:     int64(n.Packets),
		Caps:        n.Caps,
		Wants:       int64(n.Wants),
		Haves:       int64(n.Haves),
		Shallows:    int64(n.Shallows),
		Deepen:      n.Deepen,
		Filter:      n.Filter,
	}
}

//nolint:revive // This is unintentionally missing documentation.
func ParsePackfileNegotiation(body io.Reader, opts ...ParsePackfileNegotiationOption) (PackfileNegotiation, error) {
	n := PackfileNegotiation{}
	return n, n.Parse(body, opts...)
}

// Parse parses a packfile negotiation. It expects the protocol v0/v1 format:
//
// want <OID> <capabilities>
// [want <OID>...]
// [shallow <OID>]
// [deepen <depth>|deepen-since <timestamp>|deepen-not <ref>]
// [filter <filter-spec>]
// flush
// have <OID>
// flush|done
//
// Protocol v2 sends the same arguments wrapped in a command request, which
// opens with "command=<name>" and "agent=<agent>" ahead of a delim packet, and
// which carries arguments v0 has no equivalent for: "ref-prefix <prefix>" for
// ls-refs, and "want-ref <ref>" for fetch. See
// https://git-scm.com/docs/protocol-v2#_command_request.
func (n *PackfileNegotiation) Parse(body io.Reader, opts ...ParsePackfileNegotiationOption) error {
	defer func() { _, _ = io.Copy(io.Discard, body) }()
	config := parsePackfileNegotiationConfig{}
	for _, opt := range opts {
		opt(&config)
	}

	scanner := pktline.NewScanner(body)

	for ; scanner.Scan(); n.Packets++ {
		pkt := scanner.Bytes()
		data := text.ChompBytes(pktline.Data(pkt))
		split := strings.Split(data, " ")
		n.PayloadSize += int64(len(data))

		done := false

		lineFirstPart := split[0]

		// commands are in the form `command=<name>` so it does not fit well
		// into the switch case below. So we handle them here.
		// The commands we look for are the ones described here:
		// https://git-scm.com/docs/protocol-v2#_command_request
		// We are also interested in the `bundle-uri` commandm, which is
		// explained here:
		// https://git-scm.com/docs/bundle-uri#_implementation_plan
		if command, ok := strings.CutPrefix(lineFirstPart, "command="); ok {
			n.Command = command
			continue
		} else if agent, ok := strings.CutPrefix(lineFirstPart, "agent="); ok {
			n.UserAgent = agent
			continue
		}

		switch lineFirstPart {
		case "ref-prefix":
			if config.parseLsRefsPipelineRefPrefixes && len(split) == 2 && n.Command == "ls-refs" && pipelineRefPrefixRegex.MatchString(split[1]) {
				n.PipelineRefPrefixes++
			}
		case "want":
			if len(split) < 2 {
				return fmt.Errorf("invalid 'want' for packet %d: %v", n.Packets, data)
			}
			if len(split) > 2 && n.Caps != nil {
				return fmt.Errorf("capabilities announced multiple times in packet %d: %v", n.Packets, data)
			}

			n.Wants++
			n.WantsSample = appendWant(n.WantsSample, split[1])
			if len(split) > 2 {
				n.Caps = split[2:]
				for _, cap := range n.Caps {
					if agent, ok := strings.CutPrefix(cap, "agent="); ok {
						n.UserAgent = agent
						break
					}
				}
			}
		case "want-ref":
			// A "want-ref" names a ref instead of an object, so it neither counts towards
			// Wants nor lands in WantsSample. Malformed lines are ignored rather than
			// rejected, as with "ref-prefix" above: git-upload-pack(1) passes the argument
			// through verbatim, so failing here would drop all instrumentation for, or even
			// fail, a request that Git itself serves.
			if len(split) == 2 {
				n.WantRefs++
				n.WantRefsSample = appendWantRef(n.WantRefsSample, split[1])
			}
		case "shallow":
			if len(split) != 2 {
				return fmt.Errorf("invalid 'shallow' for packet %d: %v", n.Packets, data)
			}
			n.Shallows++
		case "deepen", "deepen-since", "deepen-not":
			if len(split) != 2 {
				return fmt.Errorf("invalid 'deepen' for packet %d: %v", n.Packets, data)
			}
			n.Deepen = data
		case "filter":
			if len(split) != 2 {
				return fmt.Errorf("invalid 'filter' for packet %d: %v", n.Packets, data)
			}
			n.Filter = split[1]
		case "have":
			if len(split) != 2 {
				return fmt.Errorf("invalid 'have' for packet %d: %v", n.Packets, data)
			}
			n.Haves++
		case "done":
			done = true
		}

		if done {
			break
		}
	}

	if scanner.Err() != nil {
		return scanner.Err()
	}

	return nil
}

// UpdateMetrics updates Prometheus metrics with features that have been used
// during a packfile negotiation.
func (n *PackfileNegotiation) UpdateMetrics(c *prometheus.CounterVec, h prometheus.Histogram) {
	if n.Command != "" {
		c.WithLabelValues(n.Command).Inc()
	}
	if n.Deepen != "" {
		c.WithLabelValues("deepen").Inc()
	}
	if n.Filter != "" {
		c.WithLabelValues("filter").Inc()
	}
	if n.Haves > 0 {
		c.WithLabelValues("have").Inc()
	}
	c.WithLabelValues("total").Inc()

	if depth, ok := strings.CutPrefix(n.Deepen, "deepen "); ok {
		if d, err := strconv.Atoi(depth); err == nil {
			h.Observe(float64(d))
		}
	}
}

// UpdateLogFields add fields on the logger instance to provide
// details about the command that was used during packfile negotiation.
func (n *PackfileNegotiation) UpdateLogFields(ctx context.Context) {
	if n.Command != "" {
		log.CustomFieldsFromContext(ctx).RecordMetadata("negotiation.command", n.Command)
	}
	if n.UserAgent != "" {
		log.CustomFieldsFromContext(ctx).RecordMetadata("negotiation.user_agent", n.UserAgent)
	}
	if n.Deepen != "" {
		log.CustomFieldsFromContext(ctx).RecordMetadata("negotiation.deepen", n.Deepen)
	}
	if n.Filter != "" {
		log.CustomFieldsFromContext(ctx).RecordMetadata("negotiation.filter", n.Filter)
	}
	log.CustomFieldsFromContext(ctx).RecordMetadata("negotiation.haves", n.Haves)
	log.CustomFieldsFromContext(ctx).RecordMetadata("negotiation.wants", n.Wants)
	if len(n.WantsSample) > 0 {
		log.CustomFieldsFromContext(ctx).RecordMetadata("negotiation.wants_sample", n.WantsSample)
	}
	if n.WantRefs > 0 {
		log.CustomFieldsFromContext(ctx).RecordMetadata("negotiation.want_refs", n.WantRefs)
	}
	if len(n.WantRefsSample) > 0 {
		log.CustomFieldsFromContext(ctx).RecordMetadata("negotiation.want_refs_sample", n.WantRefsSample)
	}
	if n.Shallows > 0 {
		log.CustomFieldsFromContext(ctx).RecordMetadata("negotiation.shallows", n.Shallows)
	}

	log.FromContext(ctx).
		WithFields(log.CustomFieldsFromContext(ctx).Fields()).
		InfoContext(ctx, "packfile negotiation complete")
}

// UpdateLsRefsPipelineRefLogFields adds log fields for ls-refs pipeline ref-prefix usage.
func (n *PackfileNegotiation) UpdateLsRefsPipelineRefLogFields(ctx context.Context) {
	if n.PipelineRefPrefixes > 0 {
		log.CustomFieldsFromContext(ctx).RecordMetadata("negotiation.pipeline_ref_prefixes", n.PipelineRefPrefixes)
	}
}
