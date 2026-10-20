package changelog

import (
	"fmt"
	"testing"
)

// BenchmarkLookupLandAfterOneAppend measures a lookup that catches up one new entry of a log of the
// given size, as a land that diverges does after the land before it.
func BenchmarkLookupLandAfterOneAppend(b *testing.B) {
	for _, size := range []int{10, 250, 2000} {
		b.Run(fmt.Sprintf("log of %d", size), func(b *testing.B) {
			repoPath := b.TempDir()
			records := make([]Record, size)
			for i := range records {
				records[i] = Record{
					Meta:    Meta{Operation: "land", LandReference: "refs/heads/main", LandInvocation: fmt.Sprint("inv-", i), Lands: []Land{{Original: landOID(1000000 + i), Landed: landOID(2000000 + i)}}},
					Updates: []Update{{Reference: "refs/heads/main", OldOID: landOID(0), NewOID: landOID(2000000 + i)}},
				}
			}
			if _, err := AppendGroup(repoPath, records); err != nil {
				b.Fatal(err)
			}
			if _, err := LookupLand(repoPath, "refs/heads/main", landOID(1)); err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				land := Land{Original: landOID(3000000 + i), Landed: landOID(4000000 + i)}
				if _, err := Append(repoPath, Meta{Operation: "land", LandReference: "refs/heads/main", Lands: []Land{land}},
					[]Update{{Reference: "refs/heads/main", OldOID: landOID(0), NewOID: land.Landed}}); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()

				if _, err := LookupLand(repoPath, "refs/heads/main", land.Original); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
