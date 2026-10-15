package featureflag

// FetchSourceBranchHideRefs enables the use of fetch.hideRefs in the
// FetchSourceBranch RPC.
var FetchSourceBranchHideRefs = NewFeatureFlag(
	"fetch_source_branch_hide_refs",
	"v19.4",
	"https://gitlab.com/gitlab-org/gitaly/-/work_items/7318",
	false,
)
