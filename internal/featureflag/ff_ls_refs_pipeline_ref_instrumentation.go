package featureflag

// LsRefsPipelineRefInstrumentation enables metrics and logs for ls-refs requests targeting pipeline refs.
var LsRefsPipelineRefInstrumentation = NewFeatureFlag(
	"ls_refs_pipeline_ref_instrumentation",
	"v19.1.0",
	"https://gitlab.com/gitlab-org/gitlab/-/work_items/591794",
	false,
)
