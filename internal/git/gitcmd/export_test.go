package gitcmd

// RepositoryObjectHash and CommonGitDir expose the attr.tree object-format detection to the tests of
// package gitcmd_test, which compare it with what the bundled git reports.
var (
	RepositoryObjectHash = repositoryObjectHash
	CommonGitDir         = commonGitDir
)
