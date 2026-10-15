/title [Flaky Test] <test name>

<!--
Provide any relevant information about the flaky test. Such as:
- Link to CI job with flaky test failure
- Copy of logs containing failure
-->

## Steps
- [ ] This issue has been assigned the current milestone
- [ ] Flaky test has been quarantined with [`testhelper.SkipQuarantinedTest`](https://gitlab.com/gitlab-org/gitaly/-/blob/master/internal/testhelper/testhelper.go#L388-L398).
- [ ] Flaky test has been fixed and quarantine removed.

/label ~"Category:Gitaly" ~"group::gitaly" ~"section::enablement" ~"devops::systems" ~"type::maintenance" ~"maintenance::pipelines" ~"failure::flaky-test" ~"priority::1"
/assign me
/label ~"workflow::ready for development"
