package featureflag

// NewRepoMVCCBackend is used to set 'mvcc' as the reference backend for new
// repositories. When enabled, CreateRepository initializes repositories with
// the MVCC reference backend, which are served by the dedicated MVCC-enabled
// Git binary.
var NewRepoMVCCBackend = NewFeatureFlag(
	"new_repo_mvcc_backend",
	"v19.3.0",
	"https://gitlab.com/gitlab-org/gitaly/-/issues/7238",
	false,
)
