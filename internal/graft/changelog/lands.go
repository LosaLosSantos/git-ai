package changelog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Land is the identity of one change that a land put on a branch (doc/graft/PLAN11.md): the commit
// the request submitted and the commit on the branch that holds the change. Landed is the rebased
// copy of Original, the merge commit of a merged or resolved land, or Original itself for a
// fast-forward or a created branch.
type Land struct {
	// Original is the commit the land request submitted, its commit_id peeled to a commit.
	Original string `json:"original"`
	// Landed is the commit on the branch that holds the change.
	Landed string `json:"landed"`
}

// LandRecord is what the index of a branch knows about the land of one original commit: the
// changelog entry that recorded it and the commit it landed as. A later land of the same original
// commit replaces it (see LookupLand).
type LandRecord struct {
	// Sequence is the sequence of the changelog entry that recorded the land.
	Sequence uint64
	// Landed is the commit on the branch that holds the change, see Land.Landed.
	Landed string
}

const (
	// MaxLandsSize bounds the encoded size of the identities of one update (the reference and its
	// lands), which travel in the hooks payload, in the environment of every git process of the
	// update, next to the members (see gitcmd.ChangelogMeta). Unlike the members, the identities are
	// never cut: a land request whose branch name could not fit is refused when it arrives
	// (CheckLandReference), and WithLands checks the bound again before the update runs. A batch
	// has at most MaxLandsPerUpdate members and a land of a SHA-256 repository encodes in 156
	// bytes, 15.6 KB for a full batch; the branch name is encoded in base64, so 24 KiB hold a full
	// batch and a branch name of 4096 bytes, which no files reference backend exceeds, with room.
	MaxLandsSize = 24 << 10
	// MaxLandsPerUpdate is the most lands that one update records: the largest batch the
	// configuration allows (config.MaxGraftLandBatchMaxSize).
	MaxLandsPerUpdate = 100
)

// encodedLands is the encoded form of the identities of one update whose size MaxLandsSize bounds,
// as gitcmd.ChangelogMeta encodes them: the reference as bytes, in base64.
type encodedLands struct {
	Reference []byte `json:"land_reference"`
	Lands     []Land `json:"lands"`
}

// fullBatchLandsSize is the encoded size of the identities of a full batch of SHA-256 lands on a
// branch with an empty name, the largest identities of any branch but its name.
var fullBatchLandsSize = func() int {
	lands := make([]Land, MaxLandsPerUpdate)
	for i := range lands {
		lands[i] = Land{Original: strings.Repeat("f", 64), Landed: strings.Repeat("f", 64)}
	}
	encoded, err := json.Marshal(encodedLands{Lands: lands})
	if err != nil {
		panic(fmt.Sprintf("encoding the identities of a full batch: %v", err))
	}
	return len(encoded)
}()

// CheckLandReference checks, when a land request arrives, that the identities of any update of
// reference fit MaxLandsSize: those of a full batch of SHA-256 lands on it. A branch whose name is
// too long for them cannot be landed on, since a land would not record its identity.
func CheckLandReference(reference string) error {
	if size := fullBatchLandsSize + base64.StdEncoding.EncodedLen(len(reference)); size > MaxLandsSize {
		return fmt.Errorf("the name of branch %q is too long to record the identity of a land on it: %d bytes, at most %d",
			reference, len(reference), (MaxLandsSize-fullBatchLandsSize)/4*3)
	}
	return nil
}

// ValidateLands checks the identities that an update of reference records: reference is a branch,
// every land names two full object IDs, there are at most MaxLandsPerUpdate of them and their
// encoding fits MaxLandsSize.
func ValidateLands(reference string, lands []Land) error {
	if !strings.HasPrefix(reference, "refs/heads/") {
		return fmt.Errorf("land identities of %q: not a branch", reference)
	}
	if len(lands) == 0 {
		return fmt.Errorf("land identities of %q: no lands", reference)
	}
	if len(lands) > MaxLandsPerUpdate {
		return fmt.Errorf("land identities of %q: %d lands, at most %d per update", reference, len(lands), MaxLandsPerUpdate)
	}

	for i, land := range lands {
		if !isObjectID(land.Original) || !isObjectID(land.Landed) {
			return fmt.Errorf("land identities of %q: land %d: %q -> %q is not a pair of object IDs", reference, i, land.Original, land.Landed)
		}
	}

	encoded, err := json.Marshal(encodedLands{Reference: []byte(reference), Lands: lands})
	if err != nil {
		return fmt.Errorf("land identities of %q: %w", reference, err)
	}
	if len(encoded) > MaxLandsSize {
		return fmt.Errorf("land identities of %q: %d bytes, at most %d", reference, len(encoded), MaxLandsSize)
	}

	return nil
}

// isObjectID reports whether s is a full SHA-1 or SHA-256 object ID in lower-case hex, as git
// prints them.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

type landsKey struct{}

// landsValue is what WithLands stores in a context.
type landsValue struct {
	reference string
	lands     []Land
}

// WithLands records the identities of the lands that the update of reference performed with ctx
// lands (doc/graft/PLAN11.md). The update's changelog entry for reference then carries them as
// Entry.Lands, and the index of the land identities learns them. It fails, without changing ctx,
// when they do not pass ValidateLands: the land must then not run, as it would land without its
// identity.
func WithLands(ctx context.Context, reference string, lands []Land) (context.Context, error) {
	if err := ValidateLands(reference, lands); err != nil {
		return ctx, err
	}

	return context.WithValue(ctx, landsKey{}, landsValue{reference: reference, lands: lands}), nil
}

// LandsFromContext returns the reference and the lands set by WithLands, or an empty reference and
// no lands when there are none.
func LandsFromContext(ctx context.Context) (string, []Land) {
	value, _ := ctx.Value(landsKey{}).(landsValue)
	return value.reference, value.lands
}

// LandsOf returns the lands that the changelog entry of an update of reference carries under meta:
// a copy of the lands of meta when they belong to reference, none otherwise (see Entry.Lands).
// Every writer of the changelog uses it (Append and the storage core's database), so that every
// home of the changelog records the same lands. Only the server sets them, through WithLands,
// which validated them (see gitcmd.ChangelogMeta).
func LandsOf(meta Meta, reference string) []Land {
	if len(meta.Lands) == 0 || meta.LandReference != reference {
		return nil
	}

	return append([]Land(nil), meta.Lands...)
}
