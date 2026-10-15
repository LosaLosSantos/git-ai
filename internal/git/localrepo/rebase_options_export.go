package localrepo

import "github.com/feder-cr/open_scion/internal/git"

// RebaseSettings is the resolved form of a set of RebaseOptions. It lets packages that implement
// a rebase on top of the localrepo primitives honor the same options as (*Repo).Rebase.
type RebaseSettings struct {
	// Committer is the signature to use as committer of the rebased commits. Nil means that each
	// commit keeps the committer of the commit it replays.
	Committer *git.Signature
	// ReapplyCherryPicks is set when patch-equivalent commits must be picked, see
	// RebaseWithReappliedCherryPicks.
	ReapplyCherryPicks bool
}

// ResolveRebaseOptions applies options and returns the resulting settings.
func ResolveRebaseOptions(options ...RebaseOption) RebaseSettings {
	var config rebaseConfig
	for _, option := range options {
		option(&config)
	}

	return RebaseSettings{
		Committer:          config.committer,
		ReapplyCherryPicks: config.reapplyCherryPicks,
	}
}
