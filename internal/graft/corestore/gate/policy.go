package gate

// AuthoritativeMutators are the repository-scoped MUTATOR RPCs that may run
// in authoritative mode: their reference changes all go through the
// reference-transaction hook (and so through the voter), or the gate handles
// them itself (CreateRepository, RemoveRepository). Every other
// repository-scoped mutator is rejected with FailedPrecondition. An entry
// stays only when an authoritative-mode test proves it (WP56); adding one
// needs that proof. MAINTENANCE and ACCESSOR RPCs are not listed: they run
// after EnsureFresh.
var AuthoritativeMutators = map[string]struct{}{
	"/gitaly.OperationService/UserApplyPatch":        {},
	"/gitaly.OperationService/UserCherryPick":        {},
	"/gitaly.OperationService/UserCommitFiles":       {},
	"/gitaly.OperationService/UserCreateBranch":      {},
	"/gitaly.OperationService/UserCreateTag":         {},
	"/gitaly.OperationService/UserDeleteBranch":      {},
	"/gitaly.OperationService/UserDeleteTag":         {},
	"/gitaly.OperationService/UserFFBranch":          {},
	"/gitaly.OperationService/UserLand":              {},
	"/gitaly.OperationService/UserMergeBranch":       {},
	"/gitaly.OperationService/UserMergeToRef":        {},
	"/gitaly.OperationService/UserRebaseToRef":       {},
	"/gitaly.OperationService/UserRebaseConfirmable": {},
	"/gitaly.OperationService/UserRevert":            {},
	"/gitaly.OperationService/UserSquash":            {},
	"/gitaly.OperationService/UserUpdateBranch":      {},
	"/gitaly.OperationService/UserUpdateSubmodule":   {},
	"/gitaly.ConflictsService/ResolveConflicts":      {},
	"/gitaly.RefService/DeleteRefs":                  {},
	"/gitaly.RefService/UpdateReferences":            {},
	"/gitaly.RepositoryService/WriteRef":             {},
	"/gitaly.RepositoryService/CreateRepository":     {},
	"/gitaly.RepositoryService/RemoveRepository":     {},
	"/gitaly.SSHService/SSHReceivePack":              {},
	"/gitaly.SmartHTTPService/PostReceivePack":       {},
	// The Graft review service: its state is in the database in authoritative
	// mode (WP59).
	"/gitaly.ReviewService/CreateReview":        {},
	"/gitaly.ReviewService/UpdateReview":        {},
	"/gitaly.ReviewService/AddReviewComment":    {},
	"/gitaly.ReviewService/SubmitReviewVerdict": {},
	"/gitaly.ReviewService/CloseReview":         {},
}

// Passthrough are the services whose RPCs the gate never touches: the hook
// RPCs run while git holds reference locks, and the storage core's own
// service handles untracked repositories itself.
var Passthrough = []string{
	"/gitaly.HookService/",
	"/gitaly.StorageCoreService/",
}
