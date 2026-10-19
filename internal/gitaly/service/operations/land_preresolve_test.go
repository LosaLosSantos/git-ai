package operations

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The tests of the resolution out of the lane (PLAN10 15.8): the resolver is called before the
// lane, a file's verdict is kept by its sides, and the lane applies the verdicts whose files it
// finds unchanged without ever calling the resolver.

// answerByCall is the body of a resolver script (resolverScript) that answers its n-th request with
// dir/answer-n.json when the test wrote one (writeAnswerN), and with dir/answer.json otherwise, and
// that moves the branch to the commit in dir/move-n, when the test wrote one, before it answers.
// The paths of git and of the repository are in dir/git and dir/repo (writeMoveFiles).
var answerByCall = fmt.Sprintf(`if [ -f "$DIR/move-$N" ]; then
  "$(cat "$DIR/git")" -C "$(cat "$DIR/repo")" update-ref refs/heads/%s "$(cat "$DIR/move-$N")"
fi
if [ -f "$DIR/answer-$N.json" ]; then cat "$DIR/answer-$N.json"; exit 0; fi`, git.DefaultBranch)

// writeAnswerN sets the answer the resolver script of dir gives to its n-th request.
func writeAnswerN(t *testing.T, dir string, n int, resp resolver.Response) {
	t.Helper()

	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("answer-%d.json", n)), raw, 0o600))
}

// writeMoves makes the resolver script of dir move the branch of fx to moves[n-1] while it answers
// its n-th request.
func writeMoves(t *testing.T, dir string, fx resolveFixture, moves ...git.ObjectID) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(gitBinary(t, fx.cfg)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "repo"), []byte(fx.repoPath), 0o600))
	for i, move := range moves {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("move-%d", i+1)), []byte(move.String()), 0o600))
	}
}

// answerOf resolves each of paths to "merged-<path>".
func answerOf(paths ...string) resolver.Response {
	resp := resolver.Response{Version: resolver.ProtocolVersion, Resolver: "graft-resolve/fake"}
	for _, p := range paths {
		resp.Files = append(resp.Files, resolver.ResponseFile{Origin: resolver.OriginRule,
			Path: p, Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged-" + p + "\n"),
		})
	}
	return resp
}

// requestPaths returns the paths of the files of the n-th request the resolver script of dir got.
func requestPaths(t *testing.T, dir string, n int) []string {
	t.Helper()

	var paths []string
	for _, f := range resolverRequest(t, dir, n).Files {
		paths = append(paths, f.Path)
	}
	return paths
}

// concurrentCallsBody is the body of a resolver script (resolverScript) that is called at once by
// several lands: it appends a line per call, which resolverCalls' counter, read, incremented and
// written, would lose when calls overlap. Read the count with concurrentCalls.
const concurrentCallsBody = `echo x >> "$DIR/lines"`

// concurrentCalls returns the number of calls counted by concurrentCallsBody.
func concurrentCalls(t *testing.T, dir string) int {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "lines"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	require.NoError(t, err)
	return len(raw) / 2
}

// countIn returns the number in the file path, 0 when there is none yet.
func countIn(t *testing.T, path string) int {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0 // the script is writing it
	}
	return n
}

// entries returns regular files with the given contents, path and content alternating.
func entries(pathsAndContents ...string) []gittest.TreeEntry {
	var out []gittest.TreeEntry
	for i := 0; i < len(pathsAndContents); i += 2 {
		out = append(out, gittest.TreeEntry{Mode: "100644", Path: pathsAndContents[i], Content: pathsAndContents[i+1]})
	}
	return out
}

// landResult is the outcome of a UserLand call run in a goroutine.
type landResult struct {
	resp *gitalypb.UserLandResponse
	err  error
}

// landAsync runs UserLand for commit in a goroutine and returns its outcome's channel.
func landAsync(fx resolveFixture, commit git.ObjectID, message string) <-chan landResult {
	req := fx.request(message)
	req.CommitId = commit.String()
	done := make(chan landResult, 1)
	go func() {
		resp, err := fx.client.UserLand(fx.ctx, req)
		done <- landResult{resp: resp, err: err}
	}()
	return done
}

// requireRunning asserts that the land of done has not ended.
func requireRunning(t *testing.T, done <-chan landResult, what string) {
	t.Helper()

	select {
	case r := <-done:
		require.FailNow(t, what+" ended", "response %v, error %v", r.resp, r.err)
	default:
	}
}

// requireLanded waits for the land of done and asserts that it landed.
func requireLanded(t *testing.T, done <-chan landResult) *gitalypb.UserLandResponse {
	t.Helper()

	select {
	case r := <-done:
		require.NoError(t, r.err)
		return r.resp
	case <-time.After(60 * time.Second):
		require.FailNow(t, "the land did not end")
		return nil
	}
}

// heldLane is a standalone server whose lane for the branch is held. Branch main and the commits
// a, b and c conflict on the file of their name each. The land of fx.commit (commit a) resolves a
// and then waits in its verification, which runs in the lane, until release is called; first is its
// outcome. The resolver answers the land of a (its first request), and the test sets the answers of
// the next requests.
type heldLane struct {
	fx      resolveFixture
	dir     string
	vdir    string
	b, c    git.ObjectID
	first   <-chan landResult
	release func()
}

func newHeldLane(t *testing.T) heldLane {
	t.Helper()

	dir, argv := resolverScript(t, answerByCall)
	vdir := t.TempDir()
	// The first verification, the land of a's, waits for the release, at most a minute so that a
	// failed test never hangs; the others pass at once.
	verify := verifyScript(t, fmt.Sprintf(`V='%s'
N=$(cat "$V/calls" 2>/dev/null || echo 0)
N=$((N+1))
echo "$N" > "$V/calls"
if [ "$N" = 1 ]; then
  i=0
  while [ ! -f "$V/release" ] && [ "$i" -lt 600 ]; do sleep 0.1; i=$((i+1)); done
fi
if [ "$N" != 1 ] && [ -f "$V/fail" ]; then
  echo "tests failed"
  exit 3
fi
exit 0`, vdir))
	graft := batchingGraft()
	graft.Land.Resolver = withVerify(enabledResolver(argv), verify, "")

	fx := newResolveFixtureIn(t, graft,
		entries("a", "base\n", "b", "base\n", "c", "base\n"),
		entries("a", "ours\n", "b", "ours\n", "c", "ours\n"),
		entries("a", "theirs-a\n", "b", "base\n", "c", "base\n"),
	)
	b := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base),
		gittest.WithTreeEntries(entries("a", "base\n", "b", "theirs-b\n", "c", "base\n")...))
	c := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base),
		gittest.WithTreeEntries(entries("a", "base\n", "b", "base\n", "c", "theirs-c\n")...))
	writeAnswerN(t, dir, 1, answerOf("a"))

	var once sync.Once
	release := func() {
		once.Do(func() { require.NoError(t, os.WriteFile(filepath.Join(vdir, "release"), nil, 0o600)) })
	}
	t.Cleanup(release)

	first := landAsync(fx, fx.commit, "Land a")
	require.Eventually(t, func() bool { return countIn(t, filepath.Join(vdir, "calls")) == 1 },
		30*time.Second, 10*time.Millisecond, "the land of a never reached its verification")

	return heldLane{fx: fx, dir: dir, vdir: vdir, b: b, c: c, first: first, release: release}
}

// verifications returns the number of verifications run so far.
func (h heldLane) verifications(t *testing.T) int {
	return countIn(t, filepath.Join(h.vdir, "calls"))
}

func TestUserLand_resolvesOutOfTheLane(t *testing.T) {
	t.Parallel()

	t.Run("the resolver answers while another land holds the lane", func(t *testing.T) {
		t.Parallel()

		h := newHeldLane(t)
		writeAnswerN(t, h.dir, 2, answerOf("b"))

		second := landAsync(h.fx, h.b, "Land b")
		require.Eventually(t, func() bool { return countIn(t, filepath.Join(h.dir, "calls")) == 2 },
			30*time.Second, 10*time.Millisecond, "b was not resolved while the lane was held")
		requireRunning(t, h.first, "the land of a")
		require.Equal(t, 1, h.verifications(t))

		h.release()
		first := requireLanded(t, h.first)
		resp := requireLanded(t, second)

		// b's verdict, made at the old tip, holds at a's merge, which left b alone.
		require.Equal(t, 2, resolverCalls(t, h.dir))
		require.Equal(t, first.GetLandedCommitId(), resp.GetOldTip())
		landed := git.ObjectID(resp.GetLandedCommitId())
		commit, err := h.fx.repo.ReadCommit(h.fx.ctx, landed.Revision())
		require.NoError(t, err)
		require.Equal(t, []string{first.GetLandedCommitId(), h.b.String()}, commit.GetParentIds())
		require.Equal(t, "merged-a\n", h.fx.blobAt(t, landed, "a"))
		require.Equal(t, "merged-b\n", h.fx.blobAt(t, landed, "b"))
		require.Equal(t, "ours\n", h.fx.blobAt(t, landed, "c"))
	})

	t.Run("an answer that does not land never enters the lane", func(t *testing.T) {
		t.Parallel()

		h := newHeldLane(t)
		// A model's answer for b, whose verification fails: b cannot land (15.9). The verification
		// runs out of the lane too, while the lane is held.
		proposal := answerOf("b")
		proposal.Resolver = "graft-resolve/llm"
		proposal.Files[0].Origin = resolver.OriginModel
		writeAnswerN(t, h.dir, 2, proposal)
		require.NoError(t, os.WriteFile(filepath.Join(h.vdir, "fail"), nil, 0o600))

		// The land of b returns while the lane is still held: it never queued behind a.
		req := h.fx.request("Land b")
		req.CommitId = h.b.String()
		_, err := h.fx.client.UserLand(h.fx.ctx, req)
		requireMergeConflict(t, err)
		require.Equal(t, [][]byte{[]byte("b")}, landErrorDetail(t, err).GetMergeConflict().GetConflictingFiles())
		requireRunning(t, h.first, "the land of a")
		require.Equal(t, 2, h.verifications(t), "a's, held in the lane, and b's, out of it")
		require.Contains(t, packageFile(t, landConflictDetail(err), "b").GetReason(), "verification failed: exit status 3: tests failed")

		// The proposal was kept although the lane was busy with a batch: it was written before the
		// lane, on the published tip, never on a tip that exists only in a batch's quarantine
		// (review of 6090d5b, item 4).
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.NotEmpty(t, pkg.GetProposalRef())
		kept, err := h.fx.repo.ResolveRevision(h.fx.ctx, git.Revision(pkg.GetProposalRef()))
		require.NoError(t, err)
		commit, err := h.fx.repo.ReadCommit(h.fx.ctx, kept.Revision())
		require.NoError(t, err)
		require.Equal(t, []string{h.fx.tip.String(), h.b.String()}, commit.GetParentIds())
		require.Equal(t, "merged-b\n", h.fx.blobAt(t, kept, "b"))

		h.release()
		requireLanded(t, h.first)
		require.Equal(t, 2, resolverCalls(t, h.dir))
	})

	t.Run("verdicts made before a batch are applied in it, each on the one before", func(t *testing.T) {
		t.Parallel()

		h := newHeldLane(t)
		writeAnswerN(t, h.dir, 2, answerOf("b"))
		writeAnswerN(t, h.dir, 3, answerOf("c"))

		// b and c are resolved one after the other while a holds the lane, and wait in the lane
		// together: they land in one batch once a has landed.
		second := landAsync(h.fx, h.b, "Land b")
		require.Eventually(t, func() bool { return countIn(t, filepath.Join(h.dir, "calls")) == 2 }, 30*time.Second, 10*time.Millisecond)
		third := landAsync(h.fx, h.c, "Land c")
		require.Eventually(t, func() bool { return countIn(t, filepath.Join(h.dir, "calls")) == 3 }, 30*time.Second, 10*time.Millisecond)
		// The lane is held: the two requests wait in it together. Wait until it says so, not for a
		// while.
		require.Eventually(t, func() bool { return h.fx.laneQueueDepth(t) == 2 }, 30*time.Second, 10*time.Millisecond,
			"b and c never queued in the lane")
		requireRunning(t, h.first, "the land of a")

		h.release()
		first := requireLanded(t, h.first)
		respB := requireLanded(t, second)
		respC := requireLanded(t, third)

		require.Equal(t, 3, resolverCalls(t, h.dir), "no verdict was made again")
		require.Equal(t, int32(2), respB.GetBatchSize())
		require.Equal(t, int32(2), respC.GetBatchSize())
		earlier, later, laterCommit := respB, respC, h.c
		if respB.GetBatchPosition() == 2 {
			earlier, later, laterCommit = respC, respB, h.b
		}
		require.Equal(t, int32(1), earlier.GetBatchPosition())
		require.Equal(t, int32(2), later.GetBatchPosition())
		require.Equal(t, first.GetLandedCommitId(), earlier.GetOldTip())

		landed := git.ObjectID(later.GetLandedCommitId())
		requireBranchAt(t, h.fx.cfg, h.fx.repoPath, git.DefaultBranch, landed)
		commit, err := h.fx.repo.ReadCommit(h.fx.ctx, landed.Revision())
		require.NoError(t, err)
		require.Equal(t, []string{earlier.GetLandedCommitId(), laterCommit.String()}, commit.GetParentIds())
		for _, p := range []string{"a", "b", "c"} {
			require.Equal(t, "merged-"+p+"\n", h.fx.blobAt(t, landed, p), p)
		}
	})
}

func TestUserLand_resolveRounds(t *testing.T) {
	t.Parallel()

	t.Run("only the conflicting file that changed is resolved again", func(t *testing.T) {
		t.Parallel()

		// While a and b are resolved, a writer changes a: the verdict on b holds at the writer,
		// the one on a does not, and only a is sent again.
		dir, argv := resolverScript(t, answerByCall)
		writeAnswer(t, dir, validAnswer())
		second := answerOf("a")
		second.Files[0].Content = []byte("merged-a-again\n")
		writeAnswerN(t, dir, 2, second)
		fx := newResolveFixture(t, enabledResolver(argv))
		writer := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip),
			gittest.WithTreeEntries(entries("a", "ours-again\n", "b", "ours\n", "c", "c\n")...))
		writeMoves(t, dir, fx, writer)

		resp, err := fx.client.UserLand(fx.ctx, fx.request("Land agent work"))
		require.NoError(t, err)
		require.Equal(t, 2, resolverCalls(t, dir))
		require.Equal(t, []string{"a", "b"}, requestPaths(t, dir, 1))
		require.Equal(t, []string{"a"}, requestPaths(t, dir, 2))
		require.Equal(t, "ours-again\n", string(resolverRequest(t, dir, 2).Files[0].Ours.Content))
		require.Equal(t, writer.String(), resp.GetOldTip())
		require.Equal(t, "graft-resolve/fake", resp.GetResolver())
		require.Equal(t, [][]byte{[]byte("a"), []byte("b")}, resp.GetResolvedPaths())

		landed := git.ObjectID(resp.GetLandedCommitId())
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, landed)
		commit, err := fx.repo.ReadCommit(fx.ctx, landed.Revision())
		require.NoError(t, err)
		require.Equal(t, []string{writer.String(), fx.commit.String()}, commit.GetParentIds())
		require.Equal(t, "merged-a-again\n", fx.blobAt(t, landed, "a"))
		require.Equal(t, "merged-b\n", fx.blobAt(t, landed, "b"))
	})

	t.Run("files that keep changing fail after resolve_rounds, by name", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, answerByCall)
		writeAnswer(t, dir, validAnswer())
		writeAnswerN(t, dir, 2, answerOf("a"))
		rcfg := enabledResolver(argv)
		rcfg.ResolveRounds = 2
		fx := newResolveFixture(t, rcfg)
		w1 := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip),
			gittest.WithTreeEntries(entries("a", "ours-1\n", "b", "ours\n", "c", "c\n")...))
		w2 := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(w1),
			gittest.WithTreeEntries(entries("a", "ours-2\n", "b", "ours\n", "c", "c\n")...))
		writeMoves(t, dir, fx, w1, w2)

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		require.ErrorContains(t, err, "files kept changing while the land was resolved, after 2 rounds: a")
		conflict := landErrorDetail(t, err).GetMergeConflict()
		require.Equal(t, [][]byte{[]byte("a")}, conflict.GetConflictingFiles())
		require.Equal(t, []string{w2.String(), fx.commit.String()}, conflict.GetConflictingCommitIds())
		require.Equal(t, 2, resolverCalls(t, dir))
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, w2)
	})

	t.Run("files that keep changing fail at the deadline, by name", func(t *testing.T) {
		t.Parallel()

		// A writer changes a and b while every round is resolved, more often than the land can
		// keep up with: the land fails at the resolver's deadline, however many rounds the cap
		// would allow, and says why.
		dir, argv := resolverScript(t, "sleep 0.2\n"+answerByCall)
		writeAnswer(t, dir, validAnswer())
		rcfg := enabledResolver(argv)
		rcfg.Timeout = "3s"
		rcfg.ResolveRounds = 1000
		fx := newResolveFixture(t, rcfg)
		var writers []git.ObjectID
		parent := fx.tip
		for i := 0; i < 100; i++ {
			parent = gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(parent),
				gittest.WithTreeEntries(entries("a", fmt.Sprintf("ours-%d\n", i), "b", fmt.Sprintf("ours-%d\n", i), "c", "c\n")...))
			writers = append(writers, parent)
		}
		writeMoves(t, dir, fx, writers...)

		start := time.Now()
		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		require.ErrorContains(t, err, "files kept changing while the land was resolved, until the deadline of 3s: a, b")
		require.Less(t, time.Since(start), 30*time.Second)
		require.GreaterOrEqual(t, resolverCalls(t, dir), 2)
		require.Less(t, resolverCalls(t, dir), len(writers), "the deadline ended it, not the writers")
	})

	t.Run("under churn and a short deadline every land ends, landed or with its reason", func(t *testing.T) {
		t.Parallel()

		// More lands contend for a than the resolver's deadline lets land: each one either lands
		// or fails with a merge conflict that says why, and none hangs.
		const lands = 10
		dir, argv := resolverScript(t, "sleep 0.3\n"+concurrentCallsBody)
		writeAnswer(t, dir, answerOf("a"))
		rcfg := enabledResolver(argv)
		rcfg.Timeout = "2s"
		graft := batchingGraft()
		graft.Land.Resolver = rcfg
		fx := newResolveFixtureIn(t, graft,
			entries("a", "base\n"),
			entries("a", "ours\n"),
			entries("a", "theirs-0\n", "f0", "0\n"),
		)
		commits := []git.ObjectID{fx.commit}
		for i := 1; i < lands; i++ {
			commits = append(commits, gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base),
				gittest.WithTreeEntries(entries("a", fmt.Sprintf("theirs-%d\n", i), fmt.Sprintf("f%d", i), fmt.Sprintf("%d\n", i))...)))
		}

		var dones []<-chan landResult
		for i, commit := range commits {
			dones = append(dones, landAsync(fx, commit, fmt.Sprintf("Land %d", i)))
		}
		landed, timedOut := 0, 0
		for i, done := range dones {
			select {
			case r := <-done:
				if r.err == nil {
					landed++
					continue
				}
				requireMergeConflict(t, r.err)
				require.True(t, strings.Contains(r.err.Error(), "files kept changing") || strings.Contains(r.err.Error(), "deadline"),
					"land %d fails with its reason: %v", i, r.err)
				timedOut++
			case <-time.After(60 * time.Second):
				require.FailNow(t, "a land hangs", "land %d", i)
			}
		}
		require.Equal(t, lands, landed+timedOut)
		require.Positive(t, landed)
		t.Logf("%d lands: %d landed, %d ended at the deadline, %d resolutions", lands, landed, timedOut, concurrentCalls(t, dir))
	})

	t.Run("lands that move the branch under each other all land", func(t *testing.T) {
		t.Parallel()

		// Every change conflicts with the branch on a and adds a file of its own, and they all land
		// at once through the lane: each that lands changes a under the others, which resolve it
		// again. A round fails only when another land landed, so resolve_rounds of the number of
		// lands lets every one land.
		const lands = 6
		dir, argv := resolverScript(t, concurrentCallsBody)
		writeAnswer(t, dir, answerOf("a"))
		rcfg := enabledResolver(argv)
		rcfg.ResolveRounds = lands
		graft := batchingGraft()
		graft.Land.Resolver = rcfg
		fx := newResolveFixtureIn(t, graft,
			entries("a", "base\n"),
			entries("a", "ours\n"),
			entries("a", "theirs-0\n", "f0", "0\n"),
		)
		commits := []git.ObjectID{fx.commit}
		for i := 1; i < lands; i++ {
			commits = append(commits, gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base),
				gittest.WithTreeEntries(entries("a", fmt.Sprintf("theirs-%d\n", i), fmt.Sprintf("f%d", i), fmt.Sprintf("%d\n", i))...)))
		}

		var dones []<-chan landResult
		for i, commit := range commits {
			dones = append(dones, landAsync(fx, commit, fmt.Sprintf("Land %d", i)))
		}
		for _, done := range dones {
			requireLanded(t, done)
		}

		tip, err := fx.repo.ResolveRevision(fx.ctx, git.NewReferenceNameFromBranchName(git.DefaultBranch).Revision())
		require.NoError(t, err)
		require.Equal(t, "merged-a\n", fx.blobAt(t, tip, "a"))
		for i := range commits {
			require.Equal(t, fmt.Sprintf("%d\n", i), fx.blobAt(t, tip, fmt.Sprintf("f%d", i)))
		}
		calls := concurrentCalls(t, dir)
		require.GreaterOrEqual(t, calls, lands, "every land is resolved at least once")
		t.Logf("%d lands took %d resolutions", lands, calls)
	})
}

// TestLandApplyVerdicts_neverCallsTheResolver: the lane's part of a resolution writes the verdicts
// made before it and nothing else; a file without a verdict goes back to UserLand by name.
func TestLandApplyVerdicts_neverCallsTheResolver(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())
	rcfg := enabledResolver(argv)
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)
	in := fx.request("Land agent work")

	pre := newLandPreResolution()
	_, _, err := s.landResolveAt(fx.ctx, in, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
	require.NoError(t, err)
	require.Equal(t, 1, resolverCalls(t, dir))
	require.Len(t, pre.verdicts, 2)

	// At the tip they were made at, the verdicts are written.
	res, err := s.landApplyVerdicts(fx.ctx, in, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
	require.NoError(t, err)
	require.Equal(t, "graft-resolve/fake", res.resolver)
	require.Equal(t, [][]byte{[]byte("a"), []byte("b")}, res.resolvedPaths)
	require.Equal(t, "merged-a\n", fx.blobAt(t, res.commit, "a"))
	require.Equal(t, "merged-b\n", fx.blobAt(t, res.commit, "b"))

	// At a tip that changed a, a has no verdict.
	writer := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip),
		gittest.WithTreeEntries(entries("a", "ours-again\n", "b", "ours\n", "c", "c\n")...))
	_, err = s.landApplyVerdicts(fx.ctx, in, fx.repo, writer, fx.commit, fx.committer(), pre)
	var need *landNeedsResolution
	require.ErrorAs(t, err, &need)
	require.Equal(t, []string{"a"}, need.paths)
	require.Equal(t, writer, need.tip)

	// Without a pre-resolution, every file is.
	_, err = s.landApplyVerdicts(fx.ctx, in, fx.repo, fx.tip, fx.commit, fx.committer(), nil)
	require.ErrorAs(t, err, &need)
	require.Equal(t, []string{"a", "b"}, need.paths)

	require.Equal(t, 1, resolverCalls(t, dir), "the lane never calls the resolver")
}
