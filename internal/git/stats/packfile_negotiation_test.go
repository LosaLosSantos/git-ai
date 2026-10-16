package stats

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

const (
	oid1 = "78fb81a02b03f0013360292ec5106763af32c287"
	oid2 = "0f6394307cd7d4909be96a0c818d8094a4cb0e5b"
)

func requireParses(t *testing.T, reader io.Reader, expected PackfileNegotiation) {
	actual, err := ParsePackfileNegotiation(reader)
	require.NoError(t, err)
	actual.PayloadSize = 0
	actual.Packets = 0

	require.Equal(t, expected, actual)
}

func TestPackNegoWithInvalidPktline(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap")
	gittest.WritePktlineFlush(t, buf)
	// Write string with invalid length
	buf.WriteString("0002xyz")
	gittest.WritePktlineString(t, buf, "done")

	_, err := ParsePackfileNegotiation(buf)
	require.Error(t, err, "invalid pktlines should be rejected")
}

func TestPackNegoWithSingleWant(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants: 1, Caps: []string{"cap"}, WantsSample: []string{oid1},
	})
}

func TestPackNegoWithMissingCaps(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1)
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants: 1, WantsSample: []string{oid1},
	})
}

func TestPackNegoWithHave(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "have "+oid2)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants: 1, Haves: 1, Caps: []string{"cap"}, WantsSample: []string{oid1},
	})
}

func TestPackNegoWithMultipleHaveRoundds(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "have "+oid2)
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "have "+oid1)
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants:       1,
		Haves:       2,
		Caps:        []string{"cap"},
		WantsSample: []string{oid1},
	})
}

func TestPackNegoWithMultipleWants(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap")
	gittest.WritePktlineString(t, buf, "want "+oid2)
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants: 2, Caps: []string{"cap"}, WantsSample: []string{oid1, oid2},
	})
}

func TestPackNegoWithMultipleCapLines(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap1")
	gittest.WritePktlineString(t, buf, "want "+oid2+" cap2")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	_, err := ParsePackfileNegotiation(buf)
	require.Error(t, err, "multiple capability announcements should fail to parse")
}

func TestPackNegoWithDeepen(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap")
	gittest.WritePktlineString(t, buf, "deepen 1")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants:       1,
		Caps:        []string{"cap"},
		Deepen:      "deepen 1",
		WantsSample: []string{oid1},
	})
}

func TestPackNegoWithMultipleDeepens(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "deepen 1")
	gittest.WritePktlineString(t, buf, "deepen-not "+oid2)
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants:       1,
		Caps:        []string{"cap"},
		Deepen:      "deepen-not " + oid2,
		WantsSample: []string{oid1},
	})
}

func TestPackNegoWithShallow(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap")
	gittest.WritePktlineString(t, buf, "shallow "+oid1)
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants:       1,
		Caps:        []string{"cap"},
		Shallows:    1,
		WantsSample: []string{oid1},
	})
}

func TestPackNegoWithMultipleShallows(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap")
	gittest.WritePktlineString(t, buf, "shallow "+oid1)
	gittest.WritePktlineString(t, buf, "shallow "+oid2)
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants:       1,
		Caps:        []string{"cap"},
		Shallows:    2,
		WantsSample: []string{oid1},
	})
}

func TestPackNegoWithFilter(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap")
	gittest.WritePktlineString(t, buf, "filter blob:none")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants:       1,
		Caps:        []string{"cap"},
		Filter:      "blob:none",
		WantsSample: []string{oid1},
	})
}

func TestPackNegoWithMultipleFilters(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap")
	gittest.WritePktlineString(t, buf, "filter blob:none")
	gittest.WritePktlineString(t, buf, "filter blob:limit=1m")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants:       1,
		Caps:        []string{"cap"},
		Filter:      "blob:limit=1m",
		WantsSample: []string{oid1},
	})
}

func TestPackNegoWithCommandAndAgent(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=fetch")
	gittest.WritePktlineString(t, buf, "agent=git/foobar")
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Command:     "fetch",
		UserAgent:   "git/foobar",
		Wants:       1,
		Caps:        []string{"cap"},
		WantsSample: []string{oid1},
	})
}

func TestPackNegoWithAgentCapability(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=fetch")
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap agent=git/foobar")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Command:     "fetch",
		UserAgent:   "git/foobar",
		Wants:       1,
		Caps:        []string{"cap", "agent=git/foobar"},
		WantsSample: []string{oid1},
	})
}

func TestPackNegoWithLsRefsPipelineRefPrefixes(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=ls-refs")
	gittest.WritePktlineDelim(t, buf)
	gittest.WritePktlineString(t, buf, "ref-prefix refs/heads/main")
	gittest.WritePktlineString(t, buf, "ref-prefix refs/pipelines/123")
	gittest.WritePktlineString(t, buf, "ref-prefix refs/pipelines/invalid")
	gittest.WritePktlineString(t, buf, "ref-prefix refs/pipelines/12345678901234567890")
	gittest.WritePktlineString(t, buf, "ref-prefix refs/pipelines/456")
	gittest.WritePktlineFlush(t, buf)

	actual, err := ParsePackfileNegotiation(buf, WithLsRefsPipelineRefPrefixes())
	require.NoError(t, err)
	actual.PayloadSize = 0
	actual.Packets = 0
	require.Equal(t, PackfileNegotiation{
		Command:             "ls-refs",
		PipelineRefPrefixes: 2,
	}, actual)
}

func TestPackNegoWithLsRefsPipelineRefPrefixesDisabled(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=ls-refs")
	gittest.WritePktlineDelim(t, buf)
	gittest.WritePktlineString(t, buf, "ref-prefix refs/pipelines/123")
	gittest.WritePktlineFlush(t, buf)

	requireParses(t, buf, PackfileNegotiation{Command: "ls-refs"})
}

func TestPackNegoWithPipelineRefPrefixForNonLsRefsCommand(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=fetch")
	gittest.WritePktlineDelim(t, buf)
	gittest.WritePktlineString(t, buf, "ref-prefix refs/pipelines/123")
	gittest.WritePktlineFlush(t, buf)

	requireParses(t, buf, PackfileNegotiation{Command: "fetch"})
}

func TestPackNegoWithInvalidRefPrefix(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=ls-refs")
	gittest.WritePktlineDelim(t, buf)
	gittest.WritePktlineString(t, buf, "ref-prefix")
	gittest.WritePktlineString(t, buf, "ref-prefix refs/pipelines/123 extra")
	gittest.WritePktlineFlush(t, buf)

	actual, err := ParsePackfileNegotiation(buf, WithLsRefsPipelineRefPrefixes())
	require.NoError(t, err)
	actual.PayloadSize = 0
	actual.Packets = 0
	require.Equal(t, PackfileNegotiation{Command: "ls-refs"}, actual)
}

func TestUpdateMetricsDeepenHistogram(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc            string
		deepen          string
		expectedSamples int
	}{
		{
			desc:            "deepen-since is not observed",
			deepen:          "deepen-since 1234567890",
			expectedSamples: 0,
		},
		{
			desc:            "deepen-not is not observed",
			deepen:          "deepen-not " + oid1,
			expectedSamples: 0,
		},
		{
			desc:            "empty deepen is not observed",
			deepen:          "",
			expectedSamples: 0,
		},
		{
			desc:            "depth 1",
			deepen:          "deepen 1",
			expectedSamples: 1,
		},
		{
			desc:            "depth 10",
			deepen:          "deepen 10",
			expectedSamples: 1,
		},
		{
			desc:            "depth 10000",
			deepen:          "deepen 10000",
			expectedSamples: 1,
		},
		{
			desc:            "sentinel value 0x7fffffff (INFINITE_DEPTH) is observed",
			deepen:          fmt.Sprintf("deepen %d", 0x7fffffff),
			expectedSamples: 1,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			negotiationMetrics := prometheus.NewCounterVec(prometheus.CounterOpts{}, []string{"git_negotiation_feature"})
			deepenHistogram := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "test_packfile_negotiation_deepen"})
			pn := PackfileNegotiation{Deepen: tc.deepen}
			pn.UpdateMetrics(negotiationMetrics, deepenHistogram)

			testhelper.RequireHistogramSampleCounts(t, deepenHistogram, map[string]int{
				"test_packfile_negotiation_deepen": tc.expectedSamples,
			})
		})
	}
}

func TestPackNegoFullBlown(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1+" cap1 cap2")
	gittest.WritePktlineString(t, buf, "want "+oid2)
	gittest.WritePktlineString(t, buf, "shallow "+oid2)
	gittest.WritePktlineString(t, buf, "shallow "+oid1)
	gittest.WritePktlineString(t, buf, "deepen 1")
	gittest.WritePktlineString(t, buf, "filter blob:none")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "have "+oid2)
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "have "+oid1)
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants:       2,
		Haves:       2,
		Caps:        []string{"cap1", "cap2"},
		Shallows:    2,
		Deepen:      "deepen 1",
		Filter:      "blob:none",
		WantsSample: []string{oid1, oid2},
	})
}

func TestPackNegoWantsSampleIsTruncated(t *testing.T) {
	buf := &bytes.Buffer{}

	// Announce more wants than the sample retains, each OID distinct so both the
	// truncation and the ordering are observable.
	var wants []string
	for i := 0; i < maxSampleEntries+5; i++ {
		oid := fmt.Sprintf("%040x", i)
		wants = append(wants, oid)
		gittest.WritePktlineString(t, buf, "want "+oid)
	}
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Wants:       maxSampleEntries + 5,
		WantsSample: wants[:maxSampleEntries],
	})
}

func TestPackNegoWithWantRef(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=fetch")
	gittest.WritePktlineDelim(t, buf)
	gittest.WritePktlineString(t, buf, "want-ref refs/heads/main")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	// A "want-ref" names a ref instead of an object, so it must not be counted as a want nor
	// land in the OID sample.
	requireParses(t, buf, PackfileNegotiation{
		Command:        "fetch",
		WantRefs:       1,
		WantRefsSample: []string{"refs/heads/main"},
	})
}

func TestPackNegoWithMixedWantsAndWantRefs(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=fetch")
	gittest.WritePktlineDelim(t, buf)
	gittest.WritePktlineString(t, buf, "want "+oid1)
	gittest.WritePktlineString(t, buf, "want-ref refs/heads/main")
	gittest.WritePktlineString(t, buf, "want "+oid2)
	gittest.WritePktlineString(t, buf, "want-ref refs/tags/v1.0.0")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	requireParses(t, buf, PackfileNegotiation{
		Command:        "fetch",
		Wants:          2,
		WantsSample:    []string{oid1, oid2},
		WantRefs:       2,
		WantRefsSample: []string{"refs/heads/main", "refs/tags/v1.0.0"},
	})
}

func TestPackNegoWantRefsSampleIsTruncated(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=fetch")
	gittest.WritePktlineDelim(t, buf)

	// Announce more want-refs than the sample retains, each ref distinct so both the truncation
	// and the ordering are observable.
	var refs []string
	for i := 0; i < maxSampleEntries+5; i++ {
		ref := fmt.Sprintf("refs/heads/branch-%d", i)
		refs = append(refs, ref)
		gittest.WritePktlineString(t, buf, "want-ref "+ref)
	}
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	// The counter keeps the true number of want-refs, which the capped sample cannot.
	requireParses(t, buf, PackfileNegotiation{
		Command:        "fetch",
		WantRefs:       maxSampleEntries + 5,
		WantRefsSample: refs[:maxSampleEntries],
	})
}

func TestPackNegoWithInvalidWantRef(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=fetch")
	gittest.WritePktlineDelim(t, buf)
	gittest.WritePktlineString(t, buf, "want-ref")
	gittest.WritePktlineString(t, buf, "want-ref refs/heads/main extra")
	gittest.WritePktlineString(t, buf, "want-ref refs/heads/valid")
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	// Malformed want-refs are ignored rather than failing the parse: this is log-only
	// instrumentation, and a parse error drops every other statistic for the request.
	requireParses(t, buf, PackfileNegotiation{
		Command:        "fetch",
		WantRefs:       1,
		WantRefsSample: []string{"refs/heads/valid"},
	})
}

func TestPackNegoWantSampleDropsOverlongValues(t *testing.T) {
	sha256OID := strings.Repeat("a", maxWantLen)
	overlong := strings.Repeat("b", maxWantLen+1)

	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "want "+oid1)
	gittest.WritePktlineString(t, buf, "want "+sha256OID)
	gittest.WritePktlineString(t, buf, "want "+overlong)
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	// A SHA-256 OID is retained in full. Anything longer is not an OID, and a clipped one would
	// be no more resolvable than none at all, so it is dropped but still counted.
	requireParses(t, buf, PackfileNegotiation{
		Wants:       3,
		WantsSample: []string{oid1, sha256OID},
	})
}

func TestPackNegoWantRefSampleClipsOverlongRefs(t *testing.T) {
	exactly := "refs/heads/" + strings.Repeat("a", maxWantRefLen-len("refs/heads/"))
	require.Len(t, exactly, maxWantRefLen)
	overlong := "refs/heads/" + strings.Repeat("b", maxWantRefLen)

	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=fetch")
	gittest.WritePktlineDelim(t, buf)
	gittest.WritePktlineString(t, buf, "want-ref "+exactly)
	gittest.WritePktlineString(t, buf, "want-ref "+overlong)
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	// Refnames have no length limit, so an over-long one is clipped and marked rather than
	// dropped: the leading path still says which kind of ref was wanted.
	requireParses(t, buf, PackfileNegotiation{
		Command:  "fetch",
		WantRefs: 2,
		WantRefsSample: []string{
			exactly,
			overlong[:maxWantRefLen] + truncationMarker,
		},
	})
}

func TestPackNegoWantRefSampleClipsOnRuneBoundary(t *testing.T) {
	// Place a multi-byte rune so that it straddles the cut, and pad past the cap so the ref is
	// clipped at all.
	prefix := "refs/heads/" + strings.Repeat("a", maxWantRefLen-len("refs/heads/")-1)
	ref := prefix + "\u00e4" + strings.Repeat("c", maxWantRefLen)

	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=fetch")
	gittest.WritePktlineDelim(t, buf)
	gittest.WritePktlineString(t, buf, "want-ref "+ref)
	gittest.WritePktlineFlush(t, buf)
	gittest.WritePktlineString(t, buf, "done")

	actual, err := ParsePackfileNegotiation(buf)
	require.NoError(t, err)
	require.Equal(t, []string{prefix + truncationMarker}, actual.WantRefsSample)
	require.True(t, utf8.ValidString(actual.WantRefsSample[0]))
}

func TestPackNegoWantsSampleWithoutWants(t *testing.T) {
	buf := &bytes.Buffer{}
	gittest.WritePktlineString(t, buf, "command=ls-refs")
	gittest.WritePktlineDelim(t, buf)
	gittest.WritePktlineString(t, buf, "ref-prefix refs/heads/main")
	gittest.WritePktlineFlush(t, buf)

	requireParses(t, buf, PackfileNegotiation{
		Command: "ls-refs",
	})
}

func TestPackNegoUpdateLogFieldsSamples(t *testing.T) {
	for _, tc := range []struct {
		desc           string
		negotiation    PackfileNegotiation
		expectedFields map[string]any
	}{
		{
			desc:        "wants sample is recorded when present",
			negotiation: PackfileNegotiation{Wants: 2, WantsSample: []string{oid1, oid2}},
			expectedFields: map[string]any{
				"negotiation.wants_sample": []string{oid1, oid2},
			},
		},
		{
			desc:           "wants sample is omitted when empty",
			negotiation:    PackfileNegotiation{Command: "ls-refs"},
			expectedFields: map[string]any{},
		},
		{
			desc:        "want-refs count and sample are recorded when present",
			negotiation: PackfileNegotiation{WantRefs: 1, WantRefsSample: []string{"refs/heads/main"}},
			expectedFields: map[string]any{
				"negotiation.want_refs":        1,
				"negotiation.want_refs_sample": []string{"refs/heads/main"},
			},
		},
		{
			desc:        "want-refs count is recorded when the sample was dropped",
			negotiation: PackfileNegotiation{WantRefs: 1},
			expectedFields: map[string]any{
				"negotiation.want_refs": 1,
			},
		},
		{
			desc: "both samples are recorded independently",
			negotiation: PackfileNegotiation{
				Wants:          1,
				WantsSample:    []string{oid1},
				WantRefs:       1,
				WantRefsSample: []string{"refs/heads/main"},
			},
			expectedFields: map[string]any{
				"negotiation.wants_sample":     []string{oid1},
				"negotiation.want_refs":        1,
				"negotiation.want_refs_sample": []string{"refs/heads/main"},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := log.InitContextCustomFields(testhelper.Context(t))

			tc.negotiation.UpdateLogFields(ctx)

			fields := log.CustomFieldsFromContext(ctx).Fields()
			for _, key := range []string{
				"negotiation.wants_sample",
				"negotiation.want_refs",
				"negotiation.want_refs_sample",
			} {
				value, ok := fields[key]
				expected, expectPresent := tc.expectedFields[key]
				require.Equal(t, expectPresent, ok, "unexpected presence of %q", key)
				if expectPresent {
					require.Equal(t, expected, value)
				}
			}
		})
	}
}

func TestPackfileNegotiation_CloneType(t *testing.T) {
	for _, tc := range []struct {
		desc        string
		negotiation PackfileNegotiation
		expected    string
	}{
		{
			desc:        "full clone",
			negotiation: PackfileNegotiation{Wants: 1},
			expected:    "full",
		},
		{
			desc:        "fetch with haves",
			negotiation: PackfileNegotiation{Wants: 1, Haves: 1},
			expected:    "fetch",
		},
		{
			desc:        "shallow via deepen",
			negotiation: PackfileNegotiation{Wants: 1, Deepen: "deepen 1"},
			expected:    "shallow",
		},
		{
			desc:        "shallow via boundaries",
			negotiation: PackfileNegotiation{Wants: 1, Shallows: 2},
			expected:    "shallow",
		},
		{
			desc:        "partial clone",
			negotiation: PackfileNegotiation{Wants: 1, Filter: "blob:none"},
			expected:    "partial",
		},
		{
			desc:        "filter takes precedence over shallow and haves",
			negotiation: PackfileNegotiation{Wants: 1, Haves: 1, Shallows: 1, Deepen: "deepen 1", Filter: "blob:none"},
			expected:    "partial",
		},
		{
			desc:        "shallow takes precedence over haves",
			negotiation: PackfileNegotiation{Wants: 1, Haves: 1, Shallows: 1},
			expected:    "shallow",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.expected, tc.negotiation.CloneType())
		})
	}
}
