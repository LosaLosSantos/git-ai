package sqlstore_test

import (
	"fmt"
	"testing"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// BenchmarkCommitChangesetLands measures the commit of a land's changeset with the identities of
// none or of a batch of its lands (doc/graft/PLAN11.md, 4.1), so that the cost of their rows is the
// difference.
func BenchmarkCommitChangesetLands(b *testing.B) {
	for _, dialect := range coretest.Dialects() {
		for _, lands := range []int{0, 10} {
			b.Run(fmt.Sprintf("%s/%d lands", dialect, lands), func(b *testing.B) {
				ctx := testhelper.Context(b)
				s := openStore(b, ctx, coretest.DatabaseURL(b, dialect))
				repo, _, err := s.RegisterRepository(ctx, testKey("bench"), "sha1", corestore.InitialState{})
				if err != nil {
					b.Fatal(err)
				}

				old := corestore.Target("")
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					tip := oid(1000000 + i)
					meta := &changelog.Meta{Operation: "land_batch", Actor: "graft-batch"}
					if lands > 0 {
						meta.LandReference = "refs/heads/main"
						meta.LandInvocation = fmt.Sprint("inv-", i)
						for j := 0; j < lands; j++ {
							meta.Lands = append(meta.Lands, changelog.Land{Original: string(oid(2000000 + i*lands + j)), Landed: string(oid(3000000 + i*lands + j))})
						}
					}
					update := corestore.RefUpdate{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: old, New: tip}
					if _, err := s.CommitChangeset(ctx, corestore.ChangesetInput{RepoID: repo.ID, Updates: []corestore.RefUpdate{update}, Operation: "land_batch", NodeID: "node-a", Changelog: meta}); err != nil {
						b.Fatal(err)
					}
					old = tip
				}
			})
		}
	}
}
