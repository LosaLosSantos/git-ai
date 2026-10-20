package agentrefs

import "testing"

func TestRevisionHidden(t *testing.T) {
	t.Parallel()

	for rev, want := range map[string]bool{
		"refs/graft/proposals/0123/abcd":         true,
		"refs/graft/green/main":                  true,
		"refs/graft/*":                           true,
		"graft/green/main":                       true, // git completes it to refs/graft/green/main
		"graft/proposals/0123/abcd":              true,
		"graft/alice/6446a710c36ed315":           false, // a workspace's temporary branch
		"graft/greenhouse":                       false,
		"refs/graft/green/main:README.md":        true,
		"main..refs/graft/green/main":            true,
		"refs/graft/green/main^{commit}":         true,
		"refs/graft/green/main~2":                true,
		"^refs/graft/green/main":                 true,
		"refs/heads/main":                        false,
		"refs/heads/graft/x":                     false,
		"main":                                   false,
		"refs/*":                                 false,
		"--glob=x\nrefs/graft/green/main":        true, // a line feed, a carriage return, a tab, a NUL
		"--glob=x\rrefs/graft/green/main":        true, // each ends a revision for git
		"--glob=x\trefs/graft/green/main":        true,
		"--glob=x\x00refs/graft/green/main":      true,
		"--glob=x\ngraft/green/main":             true,
		"--branches=x\nrefs/graft/proposals/1/1": true,
		"refs/heads/main\nrefs/heads/other":      false,
		"--glob=refs/graft/*":                    true, // a pattern after =
		"--glob=graft/proposals/*":               true,
		"--glob=refs/*":                          false, // gitcmd leaves refs/graft/ out of the walk
		"--all":                                  false, // likewise
		"0123456789abcdef0123456789abcdef01234":  false,
		"main:refs/notes/graft":                  false,
		"main:graft/x.go":                        false, // a path, not a reference
		"main:refs/graft/x":                      false, // a path too
	} {
		if got := RevisionHidden(rev); got != want {
			t.Errorf("RevisionHidden(%q) = %v, want %v", rev, got, want)
		}
	}
	if !Hidden("refs/graft/x") || Hidden("refs/heads/graft/x") {
		t.Error("Hidden")
	}
}
