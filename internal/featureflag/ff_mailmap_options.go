package featureflag

// MailmapOptions enables the use of mailmap feature in GitLab
var MailmapOptions = NewFeatureFlag(
	"mailmap_options",
	"v16.2.0",
	"https://gitlab.com/gitlab-org/gitaly/-/issues/5394",
	true,
)
