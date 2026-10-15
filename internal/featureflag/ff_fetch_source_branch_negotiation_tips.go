package featureflag

// FetchSourceBranchNegotiationTips enables the use of --negotiation-tip in the
// FetchSourceBranch RPC.
var FetchSourceBranchNegotiationTips = NewFeatureFlag(
	"fetch_source_branch_negotiation_tips",
	"v19.4",
	"https://gitlab.com/gitlab-org/gitaly/-/work_items/7317",
	false,
)
