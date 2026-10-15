package derived

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/feder-cr/open_scion/internal/log"
)

// cachedJSON returns the value stored under kind and key, decoded from JSON. When
// no value is stored, or the stored one cannot be decoded, compute produces the value
// and it is stored, replacing an unreadable entry. A failed write only costs a
// recomputation later, so it does not fail the call.
//
// The value must be a pure function of key: every reader of the key gets the same
// value, which is why a stored value never needs invalidation.
func cachedJSON[V any](
	ctx context.Context,
	store *Store,
	logger log.Logger,
	kind, key string,
	compute func(ctx context.Context) (V, error),
) (V, error) {
	var zero V

	cached, found, err := store.Get(kind, key)
	if err != nil {
		return zero, fmt.Errorf("reading derived %s: %w", kind, err)
	}
	if found {
		var value V
		if err := json.Unmarshal(cached, &value); err == nil {
			return value, nil
		}
		// A corrupt entry is recomputed below and replaced by the write.
		logger.WithField("kind", kind).Warn("derived: discarding unreadable entry")
	}

	value, err := compute(ctx)
	if err != nil {
		return zero, err
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return zero, fmt.Errorf("encoding derived %s: %w", kind, err)
	}
	if err := store.Put(kind, key, encoded); err != nil {
		logger.WithError(err).WithField("kind", kind).Warn("derived: storing value failed")
	}

	return value, nil
}
