package featureflag

// ReceivePackStats enables recording push statistics from the receive-pack
// request as log fields and a Prometheus histogram.
var ReceivePackStats = NewFeatureFlag(
	"receive_pack_stats",
	"v19.2.0",
	"https://gitlab.com/gitlab-org/gitaly/-/work_items/7292",
	false,
)
