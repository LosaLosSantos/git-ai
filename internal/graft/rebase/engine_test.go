package rebase

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/ancestry"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

var testCommitter = git.Signature{
	Name:  gittest.DefaultCommitterName,
	Email: gittest.DefaultCommitterMail,
	When:  gittest.DefaultCommitTime,
}

// state maps file names to contents. It is the whole tree of a commit written by fixture.commit.
type state map[string]string

func (s state) entries() []gittest.TreeEntry {
	var entries []gittest.TreeEntry
	for _, name := range slices.Sorted(maps.Keys(s)) {
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: name, Content: s[name]})
	}
	return entries
}

// fixture is a repository with a helper to write commits into it.
type fixture struct {
	tb       testing.TB
	ctx      context.Context
	cfg      config.Cfg
	repo     *localrepo.Repo
	repoPath string
}

func newFixture(tb testing.TB) *fixture {
	tb.Helper()

	ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(tb), featureflag.GPGSigning, false)
	cfg := testcfg.Build(tb)
	repoProto, repoPath := gittest.CreateRepository(tb, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	return &fixture{
		tb:       tb,
		ctx:      ctx,
		cfg:      cfg,
		repo:     localrepo.NewTestRepo(tb, cfg, repoProto),
		repoPath: repoPath,
	}
}

// commit writes a commit whose tree holds exactly the files of s.
func (f *fixture) commit(s state, message string, parents ...git.ObjectID) git.ObjectID {
	f.tb.Helper()

	return gittest.WriteCommit(f.tb, f.cfg, f.repoPath,
		gittest.WithMessage(message),
		gittest.WithTreeEntries(s.entries()...),
		gittest.WithParents(parents...),
	)
}

// buildHistories writes a random upstream and branch history with a seeded generator. The
// histories conflict on some files, contain empty commits, clean cherry-picks of upstream
// content, and merges of side branches within the rebased range.
func buildHistories(f *fixture, rng *rand.Rand) (upstream, branch git.ObjectID) {
	files := []string{"f0", "f1", "f2", "f3", "f4"}
	pick := func() string { return files[rng.Intn(len(files))] }

	base := state{"a": "base\n"}
	baseOID := f.commit(base, "base")

	up := maps.Clone(base)
	upTip := baseOID
	for i := range 3 + rng.Intn(3) {
		up[pick()] = fmt.Sprintf("upstream %d %d\n", i, rng.Intn(3))
		upTip = f.commit(up, fmt.Sprintf("upstream %d", i), upTip)
	}

	br := maps.Clone(base)
	brTip := baseOID
	side := maps.Clone(base)
	sideTip := baseOID
	for i := range 5 + rng.Intn(4) {
		switch rng.Intn(10) {
		case 0: // empty commit
			brTip = f.commit(br, fmt.Sprintf("empty %d", i), brTip)
		case 1: // clean cherry-pick of upstream content
			if v, ok := up[pick()]; ok {
				br[pick()] = v
			}
			brTip = f.commit(br, fmt.Sprintf("cherry %d", i), brTip)
		case 2: // merge of a side branch
			side[pick()] = fmt.Sprintf("side %d %d\n", i, rng.Intn(3))
			sideTip = f.commit(side, fmt.Sprintf("side %d", i), sideTip)

			br[pick()] = fmt.Sprintf("merged %d\n", i)
			brTip = f.commit(br, fmt.Sprintf("merge %d", i), brTip, sideTip)
		default:
			br[pick()] = fmt.Sprintf("branch %d %d\n", i, rng.Intn(3))
			brTip = f.commit(br, fmt.Sprintf("branch %d", i), brTip)
		}
	}

	return upTip, brTip
}

// landRebase rebases branch onto upstream the way landing does: one range listing, the plan, and
// the replay of the engine. Like (*localrepo.Repo).Rebase it returns branch when upstream is
// reachable from it and upstream when branch is reachable from upstream.
func landRebase(ctx context.Context, repo *localrepo.Repo, engine *Engine, upstream, branch git.ObjectID, committer *git.Signature) (git.ObjectID, error) {
	rng, err := ancestry.Git(repo).Range(ctx, upstream, branch)
	if err != nil {
		return "", err
	}

	plan, err := PlanLanding(upstream, branch, rng)
	if err != nil {
		return "", err
	}

	switch plan.Kind() {
	case LandAlreadyLanded:
		return upstream, nil
	case LandFastForward:
		return branch, nil
	}

	todo, err := plan.Replay()
	if err != nil {
		return "", err
	}

	return engine.Replay(ctx, upstream, todo, committer)
}

// rebaseOptions returns the options of (*localrepo.Repo).Rebase that landing uses: reapplied
// cherry-picks, and the committer when it is set.
func rebaseOptions(committer *git.Signature) []localrepo.RebaseOption {
	opts := []localrepo.RebaseOption{localrepo.RebaseWithReappliedCherryPicks()}
	if committer != nil {
		opts = append(opts, localrepo.RebaseWithCommitter(*committer))
	}
	return opts
}

// hasMergesInRange reports whether git rev-list --merges upstream..branch lists a commit, which is
// how landing found merge commits before the plan.
func hasMergesInRange(t *testing.T, f *fixture, upstream, branch git.ObjectID) bool {
	t.Helper()
	return strings.TrimSpace(string(gittest.Exec(t, f.cfg, "-C", f.repoPath, "rev-list", "--merges", upstream.String()+".."+branch.String()))) != ""
}

// requireSameRebase rebases with landRebase and with localrepo and requires the same outcome: the
// same object ID, or an error from both, with the same conflicting commit and conflict contents for
// a conflict. It returns the error of the localrepo rebase.
func requireSameRebase(t *testing.T, f *fixture, engine *Engine, upstream, branch git.ObjectID, committer *git.Signature) error {
	t.Helper()

	wantOID, wantErr := f.repo.Rebase(f.ctx, upstream.String(), branch.String(), rebaseOptions(committer)...)
	gotOID, gotErr := landRebase(f.ctx, f.repo, engine, upstream, branch, committer)

	require.Equal(t, wantOID, gotOID)
	if wantErr == nil {
		require.NoError(t, gotErr)
		return nil
	}
	require.Error(t, gotErr)

	var wantConflict, gotConflict *localrepo.RebaseConflictError
	if errors.As(wantErr, &wantConflict) {
		require.ErrorAs(t, gotErr, &gotConflict)
		require.Equal(t, wantConflict.Commit, gotConflict.Commit)
		require.Equal(t, wantConflict.ConflictError, gotConflict.ConflictError)
	}

	return wantErr
}

// TestEngineReplayMatchesLocalrepo rebases random histories the way landing does and with
// localrepo. A range with merge commits is refused by the plan, as landing refused it before with
// git rev-list --merges; every other range gives the commits of localrepo, or its conflict.
func TestEngineReplayMatchesLocalrepo(t *testing.T) {
	t.Parallel()

	var conflicts, successes, merges int
	for seed := int64(1); seed <= 40; seed++ {
		f := newFixture(t)
		rng := rand.New(rand.NewSource(seed))
		upstream, branch := buildHistories(f, rng)

		engine := New(f.repo)
		t.Cleanup(func() { require.NoError(t, engine.Close()) })

		var committer *git.Signature
		if seed%2 == 1 {
			committer = &testCommitter
		}

		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			if hasMergesInRange(t, f, upstream, branch) {
				merges++
				_, err := landRebase(f.ctx, f.repo, engine, upstream, branch, committer)
				require.ErrorIs(t, err, ErrMergesInRange)
				return
			}

			err := requireSameRebase(t, f, engine, upstream, branch, committer)

			var conflictErr *localrepo.RebaseConflictError
			switch {
			case errors.As(err, &conflictErr):
				conflicts++
			case err == nil:
				successes++
			}
		})
	}

	require.Positive(t, conflicts, "the seeds must cover conflicts")
	require.Positive(t, successes, "the seeds must cover successful rebases")
	require.Positive(t, merges, "the seeds must cover ranges with merge commits")
}

func TestEngineRebaseUnrelatedHistories(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	upstream := f.commit(state{"a": "upstream\n"}, "upstream")
	branch := f.commit(state{"b": "branch\n"}, "unrelated root")

	engine := New(f.repo)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })

	// Without a merge base localrepo fails in its merge-base lookup, and the plan refuses the range.
	require.Error(t, requireSameRebase(t, f, engine, upstream, branch, &testCommitter))
	_, err := landRebase(f.ctx, f.repo, engine, upstream, branch, &testCommitter)
	require.ErrorIs(t, err, ErrUnrelated)
}

func TestEngineRebaseLongCommitMessage(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	base := f.commit(state{"a": "base\n"}, "base")
	upstream := f.commit(state{"a": "base\n", "u": "upstream\n"}, "upstream", base)
	longMessage := "long\n\n" + strings.Repeat("0123456789abcdef\n", 2000)
	branch := f.commit(state{"a": "base\n", "b": "branch\n"}, longMessage, base)

	engine := New(f.repo)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })

	require.NoError(t, requireSameRebase(t, f, engine, upstream, branch, nil))
}

// fakeBackends counts the helper processes started and the operations done through them. Merges
// are done by the real merge-tree stream and commits by the real commit writer, unless the writer
// is made to report ErrCommitWriterUnsupported, in which case the commit goes through WriteCommit.
type fakeBackends struct {
	repo *localrepo.Repo

	unsupported bool // the commit writer reports localrepo.ErrCommitWriterUnsupported
	streamErr   error

	streamStarts, writerStarts int
	streamClosed, writerClosed int
	merges, writes             int
}

func (fb *fakeBackends) backends() backends {
	return backends{
		newMergeStream: func(ctx context.Context) (mergeStream, error) {
			fb.streamStarts++
			if fb.streamErr != nil {
				return nil, fb.streamErr
			}

			stream, err := fb.repo.NewMergeTreeStream(ctx)
			if err != nil {
				return nil, err
			}
			return &fakeStream{fb: fb, stream: stream}, nil
		},
		newCommitWriter: func(ctx context.Context) (commitWriter, error) {
			fb.writerStarts++

			writer, err := fb.repo.NewCommitWriter(ctx)
			if err != nil {
				return nil, err
			}
			return &fakeWriter{fb: fb, writer: writer}, nil
		},
	}
}

type fakeStream struct {
	fb     *fakeBackends
	stream *localrepo.MergeTreeStream
}

func (s *fakeStream) Merge(ctx context.Context, base, ours, theirs git.ObjectID) (git.ObjectID, error) {
	s.fb.merges++
	return s.stream.Merge(ctx, base, ours, theirs)
}

func (s *fakeStream) Close() error {
	s.fb.streamClosed++
	return s.stream.Close()
}

type fakeWriter struct {
	fb     *fakeBackends
	writer *localrepo.CommitWriter
}

func (w *fakeWriter) Write(ctx context.Context, cfg localrepo.WriteCommitConfig) (git.ObjectID, error) {
	w.fb.writes++
	if w.fb.unsupported {
		return "", localrepo.ErrCommitWriterUnsupported
	}
	return w.writer.Write(ctx, cfg)
}

func (w *fakeWriter) Close() error {
	w.fb.writerClosed++
	return w.writer.Close()
}

// linearBranches returns an upstream with one commit on base and a branch with three commits on
// base that touch other files, so that the rebase needs three merges and three writes.
func linearBranches(f *fixture) (base, upstream, branch git.ObjectID) {
	base = f.commit(state{"a": "base\n"}, "base")
	upstream = f.commit(state{"a": "base\n", "u": "upstream\n"}, "upstream", base)

	tip := base
	s := state{"a": "base\n"}
	for i := range 3 {
		s[fmt.Sprintf("b%d", i)] = fmt.Sprintf("branch %d\n", i)
		tip = f.commit(s, fmt.Sprintf("branch %d", i), tip)
	}

	return base, upstream, tip
}

func TestEngineStartsHelpersLazily(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	base, upstream, branch := linearBranches(f)

	fb := &fakeBackends{repo: f.repo}
	engine := newEngine(f.repo, fb.backends())

	// A fast-forward, as the base is an ancestor of the branch, needs no helper process.
	oid, err := landRebase(f.ctx, f.repo, engine, base, branch, nil)
	require.NoError(t, err)
	require.Equal(t, branch, oid)
	require.Zero(t, fb.streamStarts)
	require.Zero(t, fb.writerStarts)

	// A real rebase starts one helper of each kind, once.
	oid, err = landRebase(f.ctx, f.repo, engine, upstream, branch, &testCommitter)
	require.NoError(t, err)
	require.NotEmpty(t, oid)
	require.Equal(t, 1, fb.streamStarts)
	require.Equal(t, 1, fb.writerStarts)
	require.Equal(t, 3, fb.merges)
	require.Equal(t, 3, fb.writes)

	require.NoError(t, engine.Close())
	require.Equal(t, 1, fb.streamClosed)
	require.Equal(t, 1, fb.writerClosed)

	// Close is idempotent and closes nothing more.
	require.NoError(t, engine.Close())
	require.Equal(t, 1, fb.streamClosed)
	require.Equal(t, 1, fb.writerClosed)
}

func TestEngineClosesNothingWhenNeverUsed(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	fb := &fakeBackends{repo: f.repo}
	engine := newEngine(f.repo, fb.backends())

	require.NoError(t, engine.Close())
	require.Zero(t, fb.streamStarts+fb.writerStarts+fb.streamClosed+fb.writerClosed)
}

func TestEngineFallsBackToWriteCommitWhenUnsupported(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, upstream, branch := linearBranches(f)

	fb := &fakeBackends{repo: f.repo, unsupported: true}
	engine := newEngine(f.repo, fb.backends())
	t.Cleanup(func() { require.NoError(t, engine.Close()) })

	require.NoError(t, requireSameRebase(t, f, engine, upstream, branch, &testCommitter))
	require.Equal(t, 3, fb.writes, "every commit is offered to the writer first")
}

func TestEngineReportsHelperStartFailure(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, upstream, branch := linearBranches(f)

	startErr := errors.New("no merge-tree stream")
	fb := &fakeBackends{repo: f.repo, streamErr: startErr}
	engine := newEngine(f.repo, fb.backends())
	t.Cleanup(func() { require.NoError(t, engine.Close()) })

	_, err := landRebase(f.ctx, f.repo, engine, upstream, branch, nil)
	require.ErrorIs(t, err, startErr)
	require.Zero(t, fb.writes)
}

func BenchmarkRebaseEngine(b *testing.B) {
	benchmarkRebase(b, func(f *fixture, upstream, branch git.ObjectID) {
		// The fake backends count the helper processes the engine starts. The git command that lists
		// the range and the object reads are not counted.
		fb := &fakeBackends{repo: f.repo}
		engine := newEngine(f.repo, fb.backends())
		defer func() { require.NoError(b, engine.Close()) }()

		var iterations int
		for b.Loop() {
			iterations++
			if _, err := landRebase(f.ctx, f.repo, engine, upstream, branch, &testCommitter); err != nil {
				b.Fatal(err)
			}
		}

		b.ReportMetric(float64(fb.streamStarts+fb.writerStarts)/float64(iterations), "helper-starts/op")
	})
}

func BenchmarkLocalrepoRebase(b *testing.B) {
	benchmarkRebase(b, func(f *fixture, upstream, branch git.ObjectID) {
		for b.Loop() {
			if _, err := f.repo.Rebase(f.ctx, upstream.String(), branch.String(), rebaseOptions(&testCommitter)...); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// benchmarkRebase rebases a 40 commit chain onto a single upstream commit.
func benchmarkRebase(b *testing.B, run func(f *fixture, upstream, branch git.ObjectID)) {
	f := newFixture(b)

	base := f.commit(state{"a": "base\n"}, "base")
	upstream := f.commit(state{"a": "base\n", "u": "upstream\n"}, "upstream", base)

	s := state{"a": "base\n"}
	tip := base
	for i := range 40 {
		s[fmt.Sprintf("b%02d", i)] = fmt.Sprintf("member %d\n", i)
		tip = f.commit(s, fmt.Sprintf("member %d", i), tip)
	}

	b.ResetTimer()
	run(f, upstream, tip)
}
