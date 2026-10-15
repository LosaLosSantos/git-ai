package graph

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/feder-cr/open_scion/internal/git"
)

// The stored value of a commit is:
//
//	uint32 generation (little-endian)
//	uint8  n, the number of skip pointers
//	n raw object IDs, the skip pointers in order
//
// The raw object IDs have the length of the object hash of the repository, 20 bytes
// for SHA-1 and 32 bytes for SHA-256. A decoder derives that length from the size of
// the value.
const (
	encodedHeaderSize = 5
	sha1Size          = 20
	sha256Size        = 32
)

// encodeEntry returns the stored value of a done entry.
func encodeEntry(e *entry) ([]byte, error) {
	if len(e.skips) > math.MaxUint8 {
		return nil, fmt.Errorf("%d skip pointers do not fit a graph value", len(e.skips))
	}

	data := make([]byte, encodedHeaderSize, encodedHeaderSize+len(e.skips)*sha256Size)
	binary.LittleEndian.PutUint32(data, e.generation)
	data[4] = uint8(len(e.skips))

	for _, skip := range e.skips {
		raw, err := skip.Bytes()
		if err != nil {
			return nil, fmt.Errorf("encoding skip pointer %s: %w", skip, err)
		}
		if len(raw) != sha1Size && len(raw) != sha256Size {
			return nil, fmt.Errorf("skip pointer %s has an unsupported length %d", skip, len(raw))
		}
		data = append(data, raw...)
	}

	return data, nil
}

// decodeEntry parses a stored value into a done entry without parents. It returns
// an error for a value that is not a valid encoding.
func decodeEntry(data []byte) (*entry, error) {
	if len(data) < encodedHeaderSize {
		return nil, fmt.Errorf("graph value of %d bytes is too short", len(data))
	}

	generation := binary.LittleEndian.Uint32(data)
	if generation == 0 {
		return nil, errors.New("graph value has generation 0")
	}

	n := int(data[4])
	body := data[encodedHeaderSize:]
	if n == 0 {
		if len(body) != 0 {
			return nil, fmt.Errorf("graph value of a root has %d trailing bytes", len(body))
		}
		return &entry{generation: generation, done: true}, nil
	}

	if len(body)%n != 0 {
		return nil, fmt.Errorf("graph value of %d skip pointers has %d bytes", n, len(body))
	}
	size := len(body) / n
	if size != sha1Size && size != sha256Size {
		return nil, fmt.Errorf("graph value has skip pointers of %d bytes", size)
	}

	skips := make([]git.ObjectID, n)
	for k := range skips {
		skips[k] = git.ObjectID(hex.EncodeToString(body[k*size : (k+1)*size]))
	}

	return &entry{generation: generation, skips: skips, done: true}, nil
}

// readStored returns the stored entry of id. A value that is missing or cannot be
// decoded is reported as not stored, so that the entry is computed again and the
// value is replaced.
func (ix *Index) readStored(id git.ObjectID) (*entry, bool) {
	data, found, err := ix.store.Get(id)
	if err != nil || !found {
		return nil, false
	}

	e, err := decodeEntry(data)
	if err != nil {
		return nil, false
	}

	return e, true
}

// writeStored stores the done entry e of id. The store is a cache: a failed write
// only costs a computation by a later query, so its error is not returned.
func (ix *Index) writeStored(id git.ObjectID, e *entry) {
	data, err := encodeEntry(e)
	if err != nil {
		return
	}

	_ = ix.store.Put(id, data)
}
