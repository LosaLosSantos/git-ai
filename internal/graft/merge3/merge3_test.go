package merge3

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var labels = Labels{Ours: "ours", Base: "base", Theirs: "theirs"}

func TestMerge(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name               string
		base, ours, theirs string
		hunks              []Hunk
		markers            string
	}{
		{
			name: "one line changed on both sides",
			base: "1\n2\n3\n", ours: "1\nO\n3\n", theirs: "1\nT\n3\n",
			hunks:   []Hunk{{Base: Range{1, 2}, Ours: Range{1, 2}, Theirs: Range{1, 2}}},
			markers: "1\n<<<<<<< ours\nO\n||||||| base\n2\n=======\nT\n>>>>>>> theirs\n3\n",
		},
		{
			// Ours adds two lines at the top and changes line 3; theirs changes the end. The
			// clean changes of both sides are applied around the conflict.
			name: "clean changes around a conflict",
			base: "1\n2\n3\n4\n5\n", ours: "a\nb\n1\n2\nO\n4\n5\n", theirs: "1\n2\nT\n4\nfive\n",
			hunks:   []Hunk{{Base: Range{2, 3}, Ours: Range{4, 5}, Theirs: Range{2, 3}}},
			markers: "a\nb\n1\n2\n<<<<<<< ours\nO\n||||||| base\n3\n=======\nT\n>>>>>>> theirs\n4\nfive\n",
		},
		{
			name: "non-overlapping changes merge cleanly",
			base: "1\n2\n3\n4\n5\n", ours: "O\n2\n3\n4\n5\n", theirs: "1\n2\n3\n4\nT\n",
			markers: "O\n2\n3\n4\nT\n",
		},
		{
			name: "the same change on both sides is no conflict",
			base: "1\n2\n", ours: "1\nX\n", theirs: "1\nX\n",
			markers: "1\nX\n",
		},
		{
			name: "both insert at the same point",
			base: "1\n2\n", ours: "1\nO\n2\n", theirs: "1\nT\n2\n",
			hunks:   []Hunk{{Base: Range{1, 1}, Ours: Range{1, 2}, Theirs: Range{1, 2}}},
			markers: "1\n<<<<<<< ours\nO\n||||||| base\n=======\nT\n>>>>>>> theirs\n2\n",
		},
		{
			name: "add/add: no base",
			base: "", ours: "o1\no2\n", theirs: "t1\n",
			hunks:   []Hunk{{Base: Range{0, 0}, Ours: Range{0, 2}, Theirs: Range{0, 1}}},
			markers: "<<<<<<< ours\no1\no2\n||||||| base\n=======\nt1\n>>>>>>> theirs\n",
		},
		{
			name: "a side without a final newline gets one before the marker",
			base: "1\n2", ours: "1\nO", theirs: "1\nT",
			hunks:   []Hunk{{Base: Range{1, 2}, Ours: Range{1, 2}, Theirs: Range{1, 2}}},
			markers: "1\n<<<<<<< ours\nO\n||||||| base\n2\n=======\nT\n>>>>>>> theirs\n",
		},
		{
			name: "two conflicts",
			base: "1\n2\n3\n4\n5\n", ours: "O1\n2\n3\n4\nO5\n", theirs: "T1\n2\n3\n4\nT5\n",
			hunks: []Hunk{
				{Base: Range{0, 1}, Ours: Range{0, 1}, Theirs: Range{0, 1}},
				{Base: Range{4, 5}, Ours: Range{4, 5}, Theirs: Range{4, 5}},
			},
			markers: "<<<<<<< ours\nO1\n||||||| base\n1\n=======\nT1\n>>>>>>> theirs\n2\n3\n4\n<<<<<<< ours\nO5\n||||||| base\n5\n=======\nT5\n>>>>>>> theirs\n",
		},
		{
			name: "a deletion against an edit",
			base: "1\n2\n3\n", ours: "1\n3\n", theirs: "1\nT\n3\n",
			hunks:   []Hunk{{Base: Range{1, 2}, Ours: Range{1, 1}, Theirs: Range{1, 2}}},
			markers: "1\n<<<<<<< ours\n||||||| base\n2\n=======\nT\n>>>>>>> theirs\n3\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r, ok := Merge([]byte(tc.base), []byte(tc.ours), []byte(tc.theirs))
			require.True(t, ok)
			require.Equal(t, tc.hunks, r.Hunks())
			require.Equal(t, tc.markers, string(r.WithMarkers(labels)))
		})
	}
}

func TestMergeTooLarge(t *testing.T) {
	t.Parallel()

	var a, b strings.Builder
	for i := 0; i < 3000; i++ {
		a.WriteString("a\n")
		b.WriteString("b\n")
	}
	_, ok := Merge([]byte(a.String()), []byte(b.String()), []byte("x\n"))
	require.False(t, ok)
}

func TestWithMarkersRoundTripsACleanMerge(t *testing.T) {
	t.Parallel()

	// With no conflict, the merge holds every change of both sides and no marker.
	r, ok := Merge([]byte("a\nb\nc\nd\n"), []byte("A\nb\nc\nd\n"), []byte("a\nb\nc\nD\ne\n"))
	require.True(t, ok)
	require.Empty(t, r.Hunks())
	require.Equal(t, "A\nb\nc\nD\ne\n", string(r.WithMarkers(labels)))
}
