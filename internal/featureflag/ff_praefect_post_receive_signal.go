package featureflag

// PraefectPostReceiveSignal enables the mechanism where Praefect signals the
// PostReceiveHook after completing transaction coordination (DB write + replication
// job queuing) before the hook calls Rails. This prevents a race where Rails is
// notified before all replicas have applied the write.
var PraefectPostReceiveSignal = NewFeatureFlag(
	"praefect_post_receive_signal",
	"v19.1.0",
	"https://gitlab.com/gitlab-org/gitaly/-/issues/5406",
	false,
)
