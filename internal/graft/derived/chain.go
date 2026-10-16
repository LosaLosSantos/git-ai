package derived

import (
	"context"
	"fmt"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/log"
)

const (
	// absentValue is the stored value of a chain entry that says that the path does
	// not exist in the history. It is written explicitly so that an empty or
	// truncated entry can never be mistaken for it.
	absentValue = "absent"

	// checkpointInterval is the distance between the chain members that a walk stores
	// besides the result commit. Storing every member would let a single read
	// of a long history write one file per commit, so only every checkpointInterval-th
	// member is stored. A later query that starts anywhere on a stored chain reaches a
	// stored member after fewer than checkpointInterval steps, because every member's
	// value depends on the same next commit as before.
	checkpointInterval = 64

	// maxStoredPerWalk bounds the number of entries one walk writes to the store.
	// Every query is answered by the walk it starts, so the writes of a request
	// must not grow with the length of the history: a reader that names paths
	// freely could otherwise fill the disk. Checkpoints already keep the writes far
	// below one per commit; this is the absolute limit on top of them. Commits
	// beyond the bound are simply computed again by a later query that reaches them.
	maxStoredPerWalk = 1024
)

// encodeValue returns the stored form of a chain value. The empty object ID means
// that the result is absent.
func encodeValue(id git.ObjectID) []byte {
	if id == "" {
		return []byte(absentValue)
	}
	return []byte(id)
}

// decodeValue parses a stored chain value. It returns false when the data is not
// a valid value, which is the case for corrupt entries.
func decodeValue(data []byte) (git.ObjectID, bool) {
	if string(data) == absentValue {
		return "", true
	}
	if !objectIDRegexp.Match(data) {
		return "", false
	}
	return git.ObjectID(data), true
}

// chainStep computes the link of one commit in a chain walk. When next is not
// empty the walk continues at next, whose value is also the value of the commit.
// When next is empty the walk ends and result is the value of the commit. An
// empty result says that the path does not exist.
type chainStep func(ctx context.Context, commit git.ObjectID) (next, result git.ObjectID, err error)

// chainWalk describes a walk over a chain kind. A chain kind is a derived kind
// whose value for a commit is either the commit itself or the value of exactly one
// other commit, the next commit of the chain. The lastmod and blame-origin kinds
// are chain kinds, and so are the kinds that Git's history simplification and blame
// follow. The walk reads stored values first and computes the links it has to,
// without recursion, and stores a bounded number of the results it computed.
type chainWalk struct {
	// kind is the store kind of the values.
	kind string
	// key returns the store key of the value of a commit.
	key func(commit git.ObjectID) string
	// step computes the link of a commit that has no stored value.
	step chainStep
	// store holds the values. A nil store neither reads nor writes.
	store *Store
	// logger reports discarded and failed entries.
	logger log.Logger
	// onCompute, when not nil, is called for every commit whose link is computed
	// rather than read from the store.
	onCompute func()
}

// run walks the chain from start and returns the value of start, the empty object
// ID when the path does not exist. The walk is iterative, so deep histories do not
// grow the stack.
//
// The writes of a walk are bounded: its first member, every checkpointInterval-th
// member and the result commit, when the walk reached it, are stored, as are members
// whose entries were found to be corrupt, and never more than maxStoredPerWalk
// entries in all. A result saying that the value is absent is never stored as a new
// entry. Every path name that a client invents has that result, so storing it would
// let readers create entries without bound; such queries are answered by walking
// instead.
func (w chainWalk) run(ctx context.Context, start git.ObjectID) (git.ObjectID, error) {
	var chain []git.ObjectID
	var result git.ObjectID
	// corrupt holds the chain members whose stored entries were unreadable. They are
	// always rewritten, which replaces an existing file instead of adding one.
	corrupt := make(map[git.ObjectID]bool)

	for current := start; ; {
		value, found, err := w.store.Get(w.kind, w.key(current))
		if err != nil {
			return "", fmt.Errorf("reading derived %s: %w", w.kind, err)
		}
		if found {
			stored, valid := decodeValue(value)
			if valid {
				result = stored
				break
			}
			// A corrupt entry counts as missing. It is recomputed and replaced below.
			w.logger.WithField("kind", w.kind).WithField("commit", current.String()).Warn("derived: discarding unreadable entry")
			corrupt[current] = true
		}

		chain = append(chain, current)
		if w.onCompute != nil {
			w.onCompute()
		}

		next, link, err := w.step(ctx, current)
		if err != nil {
			return "", err
		}
		if next == "" {
			result = link
			break
		}
		current = next
	}

	stored := 0
	for i, member := range chain {
		if stored >= maxStoredPerWalk {
			break
		}
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("storing derived %s: %w", w.kind, err)
		}

		// The result commit ends the chain when the walk computed it, rather than finding
		// a stored member, and storing it answers later walks that reach it at once.
		checkpoint := i%checkpointInterval == 0 || member == result
		if !corrupt[member] && (result == "" || !checkpoint) {
			continue
		}

		stored++
		if err := w.store.Put(w.kind, w.key(member), encodeValue(result)); err != nil {
			// A failed write only costs a recomputation later, so it must not fail the query.
			w.logger.WithField("kind", w.kind).WithError(err).WithField("commit", member.String()).Warn("derived: storing chain value failed")
		}
	}

	return result, nil
}
