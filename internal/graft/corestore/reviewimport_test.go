package corestore

import (
	"testing"
	"time"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestReviewNeedsImport(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	review := func(created, updated *time.Duration) *gitalypb.Review {
		r := &gitalypb.Review{Id: "1"}
		if created != nil {
			r.CreatedAt = timestamppb.New(t0.Add(*created))
		}
		if updated != nil {
			r.UpdatedAt = timestamppb.New(t0.Add(*updated))
		}
		return r
	}
	d := func(d time.Duration) *time.Duration { return &d }

	for _, tc := range []struct {
		desc     string
		file, db *gitalypb.Review
		want     bool
	}{
		{desc: "not in the database", file: review(d(0), d(0)), db: nil, want: true},
		{desc: "changed in the file after the import", file: review(d(0), d(time.Hour)), db: review(d(0), d(0)), want: true},
		{desc: "database copy without update time", file: review(d(0), d(time.Hour)), db: review(d(0), nil), want: true},
		{desc: "unchanged", file: review(d(0), d(time.Hour)), db: review(d(0), d(time.Hour)), want: false},
		{desc: "changed in the database later", file: review(d(0), d(time.Hour)), db: review(d(0), d(2*time.Hour)), want: false},
		{desc: "another review with the same ID", file: review(d(time.Hour), d(2*time.Hour)), db: review(d(0), d(0)), want: false},
		{desc: "file without update time", file: review(d(0), nil), db: review(d(0), d(0)), want: false},
		{desc: "neither has timestamps", file: review(nil, nil), db: review(nil, nil), want: false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.want, ReviewNeedsImport(tc.file, tc.db))
		})
	}
}
