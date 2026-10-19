// Package reviewroots writes and removes the keep-around references that keep
// the head and base commits of open reviews alive (doc/graft/PLAN7.md, REVIEW
// ROOTS RULE). An open review is a root of the object graph: its commits must
// survive the deletion of their branch, on every node (local housekeeping
// keeps what references reach) and in the storage core (checkpoints and garbage
// collection keep what references reach). Making the roots references gives
// them all the same rule.
//
// The references are written and deleted like any other, through the
// reference-transaction hook, so in shadow and authoritative mode the voter
// records them and uploads the objects the storage core does not have yet. In
// authoritative mode the database keeps the invariant "every open review
// written with its roots required has the keep-around references of its
// commits": MetaStore.InsertReview and UpdateReview require them and
// CommitChangeset refuses to delete one that an open review uses.
package reviewroots

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
)

// MissingCommitsError is the error of Protect for commits that are not in the
// repository: a commit can only be kept while it exists. It happens for reviews
// written before the REVIEW ROOTS RULE whose commit was lost since; the storage
// core cannot bring it back either (nothing kept it live).
type MissingCommitsError struct {
	Commits []git.ObjectID
}

func (e *MissingCommitsError) Error() string {
	return fmt.Sprintf("commits %v are not in the repository", e.Commits)
}

// Protect writes the keep-around reference of every commit that does not have
// it, in one reference transaction. A commit that is not in the repository is a
// *MissingCommitsError and nothing is written. A commit whose reference exists
// with its value is left alone, so Protect of protected commits writes
// nothing.
func Protect(ctx context.Context, repo *localrepo.Repo, commits []git.ObjectID) error {
	names := make([]string, 0, len(commits))
	byName := make(map[string]git.ObjectID, len(commits))
	for _, commit := range commits {
		name := corestore.KeepAroundRef(commit.String())
		if _, dup := byName[name]; !dup {
			names = append(names, name)
			byName[name] = commit
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)

	current, err := gitops.ReadRefValues(ctx, repo, names)
	if err != nil {
		return fmt.Errorf("read keep-around references: %w", err)
	}

	var missing []string
	for _, name := range names {
		if current[name] != corestore.Target(byName[name]) {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	unprotected := make([]git.ObjectID, 0, len(missing))
	for _, name := range missing {
		unprotected = append(unprotected, byName[name])
	}
	absent, err := gitops.MissingObjects(ctx, repo, unprotected)
	if err != nil {
		return fmt.Errorf("look up the commits: %w", err)
	}
	if len(absent) > 0 {
		return &MissingCommitsError{Commits: absent}
	}

	updater, err := updateref.New(ctx, repo)
	if err != nil {
		return fmt.Errorf("start the keep-around update: %w", err)
	}
	defer func() { _ = updater.Close() }()

	if err := updater.Start(); err != nil {
		return fmt.Errorf("start the keep-around update: %w", err)
	}
	for _, name := range missing {
		ref := git.ReferenceName(name)
		old := current[name]
		switch {
		case old.Absent():
			err = updater.Create(ref, byName[name])
		case old.IsSymbolic():
			return fmt.Errorf("keep-around reference %s is symbolic", name)
		default:
			err = updater.Update(ref, byName[name], git.ObjectID(old))
		}
		if err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	if err := updater.Commit(); err != nil {
		// A concurrent Protect of the same commit (two reviews of one commit)
		// may have written the reference first: what counts is its value.
		after, readErr := gitops.ReadRefValues(ctx, repo, missing)
		if readErr != nil {
			return fmt.Errorf("write keep-around references: %w (and read them again: %w)", err, readErr)
		}
		for _, name := range missing {
			if after[name] != corestore.Target(byName[name]) {
				return fmt.Errorf("write keep-around references: %w", err)
			}
		}
	}
	return nil
}

// SweepReport counts what a sweep did.
type SweepReport struct {
	// Deleted is the number of keep-around references deleted.
	Deleted int
	// Kept is the number of keep-around references kept because an open review
	// uses their commit.
	Kept int
}

// Sweep deletes the keep-around references whose commit no open review of
// reviews uses: those of the candidate commits, or every keep-around reference
// of the repository when candidates is nil. Each deletion is its own
// reference transaction that expects the value read, so one refused deletion
// does not hold up the others. A review written after the open reviews were
// read may use a reference the sweep is deleting: authoritative mode refuses
// that deletion (the review store requires the reference, CommitChangeset
// refuses to delete it), and the sweep then keeps the reference; a review
// write that lost its reference to the sweep writes it again.
func Sweep(ctx context.Context, repo *localrepo.Repo, reviews store.ReviewStore, candidates []git.ObjectID) (SweepReport, error) {
	used, err := openRoots(reviews)
	if err != nil {
		return SweepReport{}, err
	}

	var refs map[string]corestore.Target
	if candidates == nil {
		all, err := gitops.ReadRefs(ctx, repo)
		if err != nil {
			return SweepReport{}, fmt.Errorf("read references: %w", err)
		}
		refs = map[string]corestore.Target{}
		for name, target := range all {
			if strings.HasPrefix(name, corestore.KeepAroundPrefix) {
				refs[name] = target
			}
		}
	} else {
		names := make([]string, 0, len(candidates))
		for _, commit := range candidates {
			names = append(names, corestore.KeepAroundRef(commit.String()))
		}
		if refs, err = gitops.ReadRefValues(ctx, repo, names); err != nil {
			return SweepReport{}, fmt.Errorf("read keep-around references: %w", err)
		}
	}

	hash, err := repo.ObjectHash(ctx)
	if err != nil {
		return SweepReport{}, fmt.Errorf("object hash: %w", err)
	}

	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)

	var report SweepReport
	for _, name := range names {
		target := refs[name]
		if target.IsSymbolic() {
			// Not a reference this package writes.
			continue
		}
		if _, ok := used[string(target)]; ok {
			report.Kept++
			continue
		}

		if err := deleteRef(ctx, repo, name, git.ObjectID(target), hash); err != nil {
			// The deletion is refused when a review written since the read uses
			// the commit: read the open reviews again to tell that from a failure.
			used, uerr := openRoots(reviews)
			if uerr != nil {
				return report, uerr
			}
			if _, ok := used[string(target)]; ok {
				report.Kept++
				continue
			}
			return report, fmt.Errorf("delete %s: %w", name, err)
		}
		report.Deleted++
	}
	return report, nil
}

// deleteRef deletes the reference if it still has the value old.
func deleteRef(ctx context.Context, repo *localrepo.Repo, name string, old git.ObjectID, hash git.ObjectHash) error {
	updater, err := updateref.New(ctx, repo)
	if err != nil {
		return err
	}
	defer func() { _ = updater.Close() }()

	if err := updater.Start(); err != nil {
		return err
	}
	if err := updater.Update(git.ReferenceName(name), hash.ZeroOID, old); err != nil {
		return err
	}
	return updater.Commit()
}

// BackfillReport counts what a backfill did.
type BackfillReport struct {
	// Protected is the number of commits whose keep-around reference was
	// written.
	Protected int
	// Missing are the commits of open reviews that are not in the repository
	// (or not object IDs): they cannot be protected any more. Such a review
	// can still be read and closed; UpdateReview refuses it with
	// FailedPrecondition naming the same review and commit.
	Missing []MissingRoot
}

// MissingRoot is a commit of an open review that the repository does not have.
type MissingRoot struct {
	ReviewID string
	Commit   string
}

// Backfill writes the keep-around references of the open reviews that do not
// have them: reviews written before the REVIEW ROOTS RULE, or imported from the
// files of shadow mode. A commit that the repository does not have cannot be
// protected; its review is reported and nothing else is held up.
func Backfill(ctx context.Context, repo *localrepo.Repo, reviews store.ReviewStore) (BackfillReport, error) {
	open, err := reviews.List("", false, 0)
	if err != nil {
		return BackfillReport{}, fmt.Errorf("list open reviews: %w", err)
	}
	hash, err := repo.ObjectHash(ctx)
	if err != nil {
		return BackfillReport{}, fmt.Errorf("object hash: %w", err)
	}

	var report BackfillReport
	byCommit := map[git.ObjectID][]string{}
	var names []string
	for _, r := range open {
		for _, commit := range []string{r.GetHeadCommitId(), r.GetBaseCommitId()} {
			if commit == "" {
				continue
			}
			if hash.ValidateHex(commit) != nil {
				report.Missing = append(report.Missing, MissingRoot{ReviewID: r.GetId(), Commit: commit})
				continue
			}
			oid := git.ObjectID(commit)
			if _, seen := byCommit[oid]; !seen {
				names = append(names, corestore.KeepAroundRef(commit))
			}
			byCommit[oid] = append(byCommit[oid], r.GetId())
		}
	}

	current, err := gitops.ReadRefValues(ctx, repo, names)
	if err != nil {
		return report, fmt.Errorf("read keep-around references: %w", err)
	}
	var unprotected []git.ObjectID
	for oid := range byCommit {
		if current[corestore.KeepAroundRef(oid.String())] != corestore.Target(oid) {
			unprotected = append(unprotected, oid)
		}
	}
	sort.Slice(unprotected, func(i, j int) bool { return unprotected[i] < unprotected[j] })

	missing, err := gitops.MissingObjects(ctx, repo, unprotected)
	if err != nil {
		return report, fmt.Errorf("look up review commits: %w", err)
	}
	absent := make(map[git.ObjectID]struct{}, len(missing))
	for _, oid := range missing {
		absent[oid] = struct{}{}
		for _, id := range byCommit[oid] {
			report.Missing = append(report.Missing, MissingRoot{ReviewID: id, Commit: oid.String()})
		}
	}

	var present []git.ObjectID
	for _, oid := range unprotected {
		if _, ok := absent[oid]; !ok {
			present = append(present, oid)
		}
	}
	if err := Protect(ctx, repo, present); err != nil {
		return report, err
	}
	report.Protected = len(present)
	sort.Slice(report.Missing, func(i, j int) bool {
		a, b := report.Missing[i], report.Missing[j]
		return a.ReviewID < b.ReviewID || (a.ReviewID == b.ReviewID && a.Commit < b.Commit)
	})
	return report, nil
}

// openRoots returns the head and base commits of the open reviews.
func openRoots(reviews store.ReviewStore) (map[string]struct{}, error) {
	open, err := reviews.List("", false, 0)
	if err != nil {
		return nil, fmt.Errorf("list open reviews: %w", err)
	}
	used := map[string]struct{}{}
	for _, r := range open {
		for _, commit := range []string{r.GetHeadCommitId(), r.GetBaseCommitId()} {
			if commit != "" {
				used[commit] = struct{}{}
			}
		}
	}
	return used, nil
}
