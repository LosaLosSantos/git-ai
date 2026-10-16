// Package verify runs the test command on the tip of a branch after lands and
// records the last green tip in a green ref.
//
// The verifier follows the changeset log of the repository from a persisted
// cursor. Lands on the watched branch start a batch; after the batch window
// the current tip is materialized in a work tree (sparse, see Config.Sparse)
// and the test command runs in it. A passing tip moves the green ref, with a
// compare-and-swap, so the green ref always names a tip that passed. A failing
// tip is handed to a Bisector, which finds the culprit and may revert it.
//
// A Verifier is not safe for concurrent use: Run and RunOnce must not run at
// the same time, and a second call fails while the first one is running.
//
// Every test run starts from a work tree that holds exactly the commit under
// test: the tree is removed and cloned again before each run, so a test that
// leaves files, directories or a .git directory behind cannot affect a later
// commit or a bisect step. The cost is one clone per run.
//
// Trust model: the test command runs the code that agents landed, with the
// privileges of the verifier. It inherits the environment of the verifier
// (os.Environ plus Test.Env), so it can read the operator token (token_file or
// the configuration). Operators must run graft-verify as a separate user or in
// a container, with the token file not readable by that user, and should pass
// only the environment the test command needs in Test.Env. Two verifiers must
// not share a work_dir.
package verify

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

// changesetPage is the number of changesets listed per call by RunOnce.
const changesetPage = 1000

// ErrBusy is returned when Run or RunOnce is called while the verifier is
// already running.
var ErrBusy = errors.New("graft-verify: verifier is already running")

// Tester materializes a commit and runs the test command on it. The bisector
// uses it to test commits between the last green tip and the failing tip.
type Tester interface {
	Test(ctx context.Context, commit string) (TestResult, error)
}

// Failure describes a failing tip that the bisector must analyze.
type Failure struct {
	// Branch is the verified branch, without the refs/heads/ prefix.
	Branch string
	// Good is the last green tip, or "" when no tip is known to be green.
	Good string
	// Bad is the tip that failed the test command.
	Bad string
	// Output is the tail of the output of the failing run.
	Output string
	// Tester tests any commit of the repository.
	Tester Tester
	// Backend reads the repository (refs, trees, changesets).
	Backend workspace.Backend
}

// Bisector analyzes a failing tip. It finds the culprit, may revert it and
// reports what it did. An error is recorded in the outcome of the cycle, and
// the cycle still counts as done. Implementations must be safe to call from a
// single goroutine.
type Bisector interface {
	Bisect(ctx context.Context, f Failure) error
}

// Outcome is the result of one verification cycle.
type Outcome struct {
	Branch string `json:"branch"`
	// Tip is the commit that was tested. It is empty when the cycle was
	// skipped.
	Tip string `json:"tip,omitempty"`
	// Passed reports whether the test command passed on Tip.
	Passed   bool `json:"passed"`
	TimedOut bool `json:"timed_out,omitempty"`
	// Green is the green tip after the cycle.
	Green string `json:"green,omitempty"`
	// GreenMoved reports whether the cycle moved the green ref.
	GreenMoved bool `json:"green_moved"`
	// Skipped is the reason the cycle tested nothing, for example that the
	// branch no longer exists.
	Skipped string `json:"skipped,omitempty"`
	// Output is the tail of the output of the test command.
	Output string `json:"output,omitempty"`
	// BisectError is the error of the bisector, when it failed.
	BisectError string `json:"bisect_error,omitempty"`
	// Cursor is the changeset sequence covered by the cycle.
	Cursor uint64 `json:"cursor"`
}

// Verifier verifies one branch of one repository.
type Verifier struct {
	cfg      Config
	branch   string // refs/heads/<branch>
	backend  workspace.Backend
	pin      *pinnedBackend
	greens   greenRefs
	bisector Bisector
	log      *slog.Logger
	treeDir  string

	// running guards Run and RunOnce.
	running sync.Mutex

	// cursor is the changeset sequence persisted in the state file.
	cursor uint64
	// green is the last known green tip.
	green string
}

// newVerifier creates a verifier over backend and greens. It prepares the work
// directory, loads the state file and reads the current green tip.
func newVerifier(ctx context.Context, cfg Config, backend workspace.Backend, greens greenRefs, logger *slog.Logger) (*Verifier, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}

	workDir, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	if err := prepareWorkDir(workDir); err != nil {
		return nil, err
	}
	st, err := loadState(cfg.StateFile)
	if err != nil {
		return nil, err
	}
	green, err := greens.Current(ctx, cfg.GreenRef)
	if err != nil {
		return nil, fmt.Errorf("read green ref %s: %w", cfg.GreenRef, err)
	}

	return &Verifier{
		cfg:     cfg,
		branch:  "refs/heads/" + cfg.Branch,
		backend: backend,
		pin:     &pinnedBackend{Backend: backend},
		greens:  greens,
		log:     logger.With("branch", cfg.Branch),
		treeDir: filepath.Join(workDir, treeDirName),
		cursor:  st.Cursor,
		green:   green,
	}, nil
}

// prepareWorkDir creates dir and marks it as a graft-verify work directory. An
// existing directory is accepted only when it is empty or already marked, so
// that a misconfigured work_dir never gets its content removed.
func prepareWorkDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create work_dir: %w", err)
	}

	marker := filepath.Join(dir, markerName)
	_, err := os.Stat(marker)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("work_dir %s is not empty and not a graft-verify work directory", dir)
	}
	return os.WriteFile(marker, []byte("graft-verify work directory; it is removed and rebuilt as needed\n"), 0o644)
}

// SetBisector sets the bisector that analyzes failing tips. Without one, a
// failing tip is only reported.
func (v *Verifier) SetBisector(b Bisector) {
	v.bisector = b
}

// Cursor returns the changeset sequence that the verifier has covered.
func (v *Verifier) Cursor() uint64 {
	return v.cursor
}

// Green returns the last green tip known to the verifier.
func (v *Verifier) Green() string {
	return v.green
}

// Test materializes commit in the work tree and runs the test command on it.
// It implements Tester.
func (v *Verifier) Test(ctx context.Context, commit string) (TestResult, error) {
	// A commit that does not exist must never be tested: the tree of a missing
	// commit could come out empty, and an empty tree passes.
	oid, err := v.backend.ResolveRevision(ctx, commit)
	if err != nil {
		return TestResult{}, fmt.Errorf("resolve %s: %w", commit, err)
	}
	if oid != commit {
		return TestResult{}, fmt.Errorf("resolve %s: got %s, want the full object ID", commit, oid)
	}

	if err := v.materialize(ctx, commit); err != nil {
		return TestResult{}, fmt.Errorf("materialize %s: %w", commit, err)
	}
	return runTests(ctx, v.treeDir, v.cfg.Test, commit)
}

// materialize makes the work tree hold exactly commit. The tree is removed and
// cloned again on every call, and not updated in place: a test run can leave
// anything behind (modified or deleted tracked files, untracked files, empty
// directories, a .git directory, special files) and none of it may reach the
// tree of a later commit or a bisect step. Removing the tree covers all of
// them at once, and no check of the tree can miss a kind of leftover.
func (v *Verifier) materialize(ctx context.Context, commit string) error {
	v.pin.pin(commit)

	if err := removeTree(v.treeDir); err != nil {
		return fmt.Errorf("remove work tree: %w", err)
	}

	if _, err := workspace.Clone(ctx, v.treeDir, v.pin, workspace.CloneOptions{
		Address: v.cfg.Address,
		Storage: v.cfg.Storage,
		Repo:    v.cfg.Repository,
		Branch:  pinRevision,
		Sparse:  v.cfg.Sparse,
	}); err != nil {
		return fmt.Errorf("clone work tree: %w", err)
	}
	return nil
}

// removeTree removes dir and everything in it. A test run may have made
// directories unreadable or unwritable, for example with chmod 000, so every
// directory is made accessible to its owner before its entries are read.
func removeTree(dir string) error {
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.RemoveAll(dir)
}

// cycle tests the current tip of the branch, which covers every changeset up
// to seq, and acts on the result. An error means that nothing was decided:
// the cursor must not move past seq.
func (v *Verifier) cycle(ctx context.Context, seq uint64) (Outcome, error) {
	out := Outcome{Branch: v.cfg.Branch, Cursor: seq}

	tip, err := v.backend.ResolveRevision(ctx, v.branch)
	if err != nil {
		if errors.Is(err, workspace.ErrNotFound) {
			out.Skipped = "branch does not exist"
			out.Green = v.green
			return out, nil
		}
		return out, fmt.Errorf("resolve branch: %w", err)
	}
	out.Tip = tip

	res, err := v.Test(ctx, tip)
	if err != nil {
		return out, err
	}
	out.Passed = res.Passed
	out.TimedOut = res.TimedOut
	out.Output = res.Output

	if res.Passed {
		moved, err := v.advanceGreen(ctx, tip)
		if err != nil {
			return out, err
		}
		out.GreenMoved = moved
	} else if v.bisector != nil {
		err := v.bisector.Bisect(ctx, Failure{
			Branch:  v.cfg.Branch,
			Good:    v.green,
			Bad:     tip,
			Output:  res.Output,
			Tester:  v,
			Backend: v.backend,
		})
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			out.BisectError = err.Error()
		}
	}

	out.Green = v.green
	return out, nil
}

// advanceGreen moves the green ref to tip, when it is not there already. It
// reports whether the ref moved.
func (v *Verifier) advanceGreen(ctx context.Context, tip string) (bool, error) {
	current, err := v.greens.Current(ctx, v.cfg.GreenRef)
	if err != nil {
		return false, fmt.Errorf("read green ref: %w", err)
	}
	if current == tip {
		v.green = tip
		return false, nil
	}
	if err := v.greens.Move(ctx, v.cfg.GreenRef, current, tip); err != nil {
		return false, fmt.Errorf("move green ref: %w", err)
	}
	v.green = tip
	return true, nil
}

// commit persists seq as the cursor.
func (v *Verifier) commit(seq uint64) error {
	if err := saveState(v.cfg.StateFile, State{Cursor: seq}); err != nil {
		return err
	}
	v.cursor = seq
	return nil
}

// Run follows the changeset log and verifies the branch after lands, until ctx
// is done. It returns nil when ctx is done and an error when the watch of the
// log fails for good. A cycle that fails, for example because the test command
// cannot be started, is retried after the batch window.
func (v *Verifier) Run(ctx context.Context) error {
	if !v.running.TryLock() {
		return ErrBusy
	}
	defer v.running.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	window := v.cfg.BatchWindow.Std()
	retryDelay := max(window, time.Second)

	changes, errs := v.backend.WatchChangesets(ctx, v.cursor)
	lastSeq := v.cursor
	pending := false

	timer := time.NewTimer(time.Hour)
	timer.Stop()
	var fire <-chan time.Time
	arm := func(d time.Duration) {
		timer.Reset(d)
		fire = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case c, ok := <-changes:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				// errs may already have been observed as closed, in which case
				// it is nil and must not be received from again.
				if errs != nil {
					if err, ok := <-errs; ok && err != nil {
						return fmt.Errorf("watch changesets: %w", err)
					}
				}
				return errors.New("watch changesets: the watch ended")
			}
			if c.Sequence > lastSeq {
				lastSeq = c.Sequence
			}

			if c.Reference == v.branch {
				if !pending {
					pending = true
					arm(window)
				}
			} else if !pending && lastSeq > v.cursor {
				// Nothing on the branch is waiting for a test, so the changes
				// that are not on the branch are covered as well.
				if err := v.commit(lastSeq); err != nil {
					v.log.Error("persist cursor", "err", err)
				}
			}

		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				return fmt.Errorf("watch changesets: %w", err)
			}

		case <-fire:
			fire = nil
			pending = false

			seq := lastSeq
			out, err := v.cycle(ctx, seq)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				v.log.Error("verification cycle failed, retrying", "err", err, "retry_in", retryDelay)
				pending = true
				arm(retryDelay)
				continue
			}
			if err := v.commit(seq); err != nil {
				return fmt.Errorf("persist cursor: %w", err)
			}
			v.logOutcome(out)
		}
	}
}

// RunOnce verifies the branch once, for CI. It reads the changesets after the
// cursor and, when the branch changed, tests its tip. ran reports whether a
// cycle ran. Without a change on the branch, nothing is tested.
func (v *Verifier) RunOnce(ctx context.Context) (out Outcome, ran bool, err error) {
	if !v.running.TryLock() {
		return Outcome{}, false, ErrBusy
	}
	defer v.running.Unlock()

	seq := v.cursor
	branchChanged := false
	for {
		batch, err := v.backend.Changesets(ctx, seq, changesetPage)
		if err != nil {
			return Outcome{}, false, fmt.Errorf("list changesets: %w", err)
		}
		for _, c := range batch {
			if c.Reference == v.branch {
				branchChanged = true
			}
			seq = c.Sequence
		}
		if len(batch) < changesetPage {
			break
		}
	}

	if !branchChanged {
		if seq > v.cursor {
			if err := v.commit(seq); err != nil {
				return Outcome{}, false, err
			}
		}
		return Outcome{}, false, nil
	}

	out, err = v.cycle(ctx, seq)
	if err != nil {
		return out, true, err
	}
	if err := v.commit(seq); err != nil {
		return out, true, fmt.Errorf("persist cursor: %w", err)
	}
	v.logOutcome(out)
	return out, true, nil
}

// logOutcome logs the result of a cycle.
func (v *Verifier) logOutcome(out Outcome) {
	if out.Skipped != "" {
		v.log.Info("verification skipped", "reason", out.Skipped)
		return
	}
	v.log.Info("verification cycle done",
		"tip", out.Tip,
		"passed", out.Passed,
		"timed_out", out.TimedOut,
		"green", out.Green,
		"green_moved", out.GreenMoved,
		"bisect_error", out.BisectError,
		"cursor", out.Cursor,
	)
}
