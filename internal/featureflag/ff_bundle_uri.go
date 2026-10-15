package featureflag

// BundleURI enables the use of git's bundle URI feature
var BundleURI = NewFeatureFlag(
	"bundle_uri",
	"v16.6.0",
	"https://gitlab.com/gitlab-org/gitaly/-/issues/5656",
	false,
)
