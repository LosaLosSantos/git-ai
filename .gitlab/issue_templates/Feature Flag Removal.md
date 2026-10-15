/title [Feature flag] Removal of feature flag name

/relate feature flag default enable issue here (if exists)
/relate feature flag rollout issue here

## What

Remove the `:feature_name` feature flag.

## Owners

- Team: Gitaly
- Most appropriate slack channel to reach out to: `#g_gitaly`
- Best individual to reach out to: NAME

## Steps

- [ ] Remove the feature flag and the pre-feature-flag code ([howto](https://gitlab.com/gitlab-org/gitaly/-/blob/master/doc/PROCESS.md#feature-lifecycle-after-it-is-live))
- [ ] Wait for the MR to be deployed to production
- [ ] Remove the feature flag via chatops ([howto](https://gitlab.com/gitlab-org/gitaly/-/blob/master/doc/PROCESS.md#remove-the-feature-flag-via-chatops))

Please refer to the [documentation of feature flags](https://gitlab.com/gitlab-org/gitaly/-/blob/master/doc/PROCESS.md#feature-flags) for further information.

/assign me
/label ~"devops::systems" ~"group::gitaly" ~"feature flag" ~"feature::maintainance" ~"Category:Gitaly" ~"section::enablement" ~"workflow::ready for development"
