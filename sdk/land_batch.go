package sdk

import "github.com/feder-cr/open_scion/proto/go/gitalypb"

// BatchInfo describes the land batch a landing was part of. The server lands
// concurrent changes on the same branch in batches, see
// doc/graft/LAND-BATCHING.md. A change that landed alone has Size 1 and
// Position 1, and Batched reports false for it.
type BatchInfo struct {
	// Size is the number of changes in the batch that landed or were already
	// landed.
	Size int32
	// Position is the 1-based position of this change among the changes of the
	// batch, in the order the batch took them (the order of submission).
	Position int32
}

// Batched reports whether the change shared its batch with other changes.
func (b BatchInfo) Batched() bool {
	return b.Size > 1
}

// batchOf returns the batch of a UserLand response. The getters are nil safe,
// so a nil response, or a server that does not send the batch fields, yields the
// zero BatchInfo.
func batchOf(resp *gitalypb.UserLandResponse) BatchInfo {
	return BatchInfo{
		Size:     resp.GetBatchSize(),
		Position: resp.GetBatchPosition(),
	}
}
