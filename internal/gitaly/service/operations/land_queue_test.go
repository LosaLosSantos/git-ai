package operations

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The tests of the queue of the lands that lose a round (land_tickets.go): N lands that conflict on
// one file, a resolver that unions the two sides (a rule: the file keeps every line), and the lands
// that wait for their turn without calling the resolver.

// unionResolver writes a resolver command that resolves every file of the request with the union of the
// lines of the two sides, as a rule, and counts its runs in dir/calls.log. The runs are concurrent, so
// each reads its own request file.
func unionResolver(t *testing.T) (string, []string) {
	t.Helper()

	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
DIR='%s'
echo x >> "$DIR/calls.log"
cat > "$DIR/req-$$.json"
python3 - "$DIR/req-$$.json" <<'PY'
import base64, json, sys
req = json.load(open(sys.argv[1]))
files = []
for f in req["files"]:
    ours = base64.b64decode(f["ours"]["content"]).decode().splitlines()
    theirs = base64.b64decode(f["theirs"]["content"]).decode().splitlines()
    merged = ours + [l for l in theirs if l not in ours]
    files.append({"path": f["path"], "action": "write", "mode": "100644", "origin": "rule",
                  "content": base64.b64encode(("\n".join(merged) + "\n").encode()).decode()})
json.dump({"version": 1, "resolver": "graft-resolve/union", "files": files}, sys.stdout)
PY
`, dir)
	path := filepath.Join(dir, "resolver.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return dir, []string{path}
}

// unionCalls is the number of runs of the resolver of dir.
func unionCalls(t *testing.T, dir string) int {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	return len(raw) / 2
}

// queueFixture is a repository where main (tip) changed a and b, and n commits from the base each
// change a in their own way (conflict on a only; b is left as the base has it, so that it merges
// clean), plus one commit that changes b only.
type queueFixture struct {
	resolveFixture
	dir      string
	onA      []git.ObjectID
	onlyB    git.ObjectID
	scope    string
	keyOfA   string
	keyOfB   string
	resolver config.GraftResolver
}

func newQueueFixture(t *testing.T, n int, tune func(*config.GraftResolver)) queueFixture {
	t.Helper()

	dir, argv := unionResolver(t)
	rcfg := enabledResolver(argv)
	if tune != nil {
		tune(&rcfg)
	}
	fx := newResolveFixtureWith(t, rcfg,
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "base\n"}, {Mode: "100644", Path: "b", Content: "base\n"}},
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "ours\n"}, {Mode: "100644", Path: "b", Content: "ours\n"}},
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "theirs-0\n"}, {Mode: "100644", Path: "b", Content: "base\n"}},
	)
	q := queueFixture{resolveFixture: fx, dir: dir, resolver: rcfg}
	q.onA = append(q.onA, fx.commit)
	for i := 1; i < n; i++ {
		q.onA = append(q.onA, gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base),
			gittest.WithTreeEntries(
				gittest.TreeEntry{Mode: "100644", Path: "a", Content: fmt.Sprintf("theirs-%d\n", i)},
				gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"})))
	}
	q.onlyB = gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base\n"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "theirs-b\n"}))
	q.scope = landTicketScope(fx.repoProto.GetStorageName(), fx.repoProto.GetRelativePath(), []byte(git.DefaultBranch))
	q.keyOfA = landTicketKeys(q.scope, []string{"a"}, false)[0]
	q.keyOfB = landTicketKeys(q.scope, []string{"b"}, false)[0]
	return q
}

func (q queueFixture) land(commit git.ObjectID) (*gitalypb.UserLandResponse, error) {
	req := q.request("Land " + commit.String())
	req.CommitId = commit.String()
	return q.client.UserLand(q.ctx, req)
}

// branchBlob reads a file at the tip of the branch.
func (q queueFixture) branchBlob(t *testing.T, path string) string {
	t.Helper()

	return strings.TrimSpace(string(gittest.Exec(t, q.cfg, "-C", q.repoPath, "show", git.DefaultBranch+":"+path)))
}

// TestUserLand_theLandsOnAHotFileAllLand: more lands than resolve_rounds conflict on one file. Without an
// order each one loses a round to every land that lands before it, and the last ones run out of rounds;
// with the queue each loses at most one, and the resolver is called for a land after its first round only
// when it is its turn.
func TestUserLand_theLandsOnAHotFileAllLand(t *testing.T) {
	t.Parallel()

	const lands = 8
	q := newQueueFixture(t, lands, func(c *config.GraftResolver) { c.ResolveRounds = 3 })

	var wg sync.WaitGroup
	errs := make([]error, lands)
	for i, c := range q.onA {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = q.land(c)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "land %d of %d (resolve_rounds is %d)", i+1, lands, 3)
	}

	// Every land is in the file: the union of all the sides.
	got := strings.Split(q.branchBlob(t, "a"), "\n")
	require.Len(t, got, lands+1, "ours and the %d changes: %v", lands, got)
	// The resolver was called for the first round of every land, and once more for each land that lost
	// it: about 2N, against N*(N+1)/2 without the queue.
	require.LessOrEqual(t, unionCalls(t, q.dir), 2*lands, "resolver calls")
	require.Empty(t, landTickets.keysOf(q.scope), "no ticket is kept")
}

// TestUserLand_aLandWaitsForItsTurnWithoutTheResolver: another land holds the ticket of the file. A land on
// the same file resolves once, then waits: the resolver is not called while it waits, and it lands when
// the holder is done, with the verdict it already had. A land on another file is not delayed.
func TestUserLand_aLandWaitsForItsTurnWithoutTheResolver(t *testing.T) {
	t.Parallel()

	q := newQueueFixture(t, 1, nil)
	holder, err := landTickets.acquire(q.ctx, []string{q.keyOfA})
	require.NoError(t, err)
	defer holder.release()

	// A land on b only passes by, whoever holds a.
	_, err = q.land(q.onlyB)
	require.NoError(t, err, "a land on another file is not delayed")
	require.Equal(t, 1, unionCalls(t, q.dir))

	done := make(chan error, 1)
	go func() {
		_, err := q.land(q.onA[0])
		done <- err
	}()
	require.Eventually(t, func() bool { return landTickets.waiting(q.keyOfA) == 1 }, 30*time.Second, 5*time.Millisecond, "the land never queued")
	callsWhenQueued := unionCalls(t, q.dir)
	require.Equal(t, 2, callsWhenQueued, "the first round of the land resolved once")
	select {
	case err := <-done:
		t.Fatalf("the land went past the holder of its file: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	require.Equal(t, callsWhenQueued, unionCalls(t, q.dir), "a waiting land does not call the resolver")

	holder.release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the land did not land when the holder was done")
	}
	require.Equal(t, callsWhenQueued, unionCalls(t, q.dir), "it landed with the verdict it had: the tip did not change the file")
	require.Equal(t, []string{"ours", "theirs-0"}, strings.Split(q.branchBlob(t, "a"), "\n")[:2])
}

// TestUserLand_aLandThatWaitsPastItsDeadlineGetsThePackage: the holder of the file does not finish in
// time. The land that waits ends at its own deadline with the package that says why, and does not land
// past the holder.
func TestUserLand_aLandThatWaitsPastItsDeadlineGetsThePackage(t *testing.T) {
	t.Parallel()

	q := newQueueFixture(t, 1, func(c *config.GraftResolver) { c.Timeout = "2s" })
	holder, err := landTickets.acquire(q.ctx, []string{q.keyOfA})
	require.NoError(t, err)
	defer holder.release()

	start := time.Now()
	_, err = q.land(q.onA[0])
	require.Less(t, time.Since(start), 30*time.Second)
	requireMergeConflict(t, err)
	pkg := landConflictDetail(err)
	require.NotNil(t, pkg, "the package, not a bare deadline")
	f := packageFile(t, pkg, "a")
	require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, f.GetOutcome())
	require.Contains(t, f.GetReason(), "waiting for the other lands on the same files")
	requireBranchAt(t, q.cfg, q.repoPath, git.DefaultBranch, q.tip)
	require.Equal(t, 0, landTickets.waiting(q.keyOfA), "the land left the queue")

	// The holder is still the holder.
	require.True(t, landTickets.blocked([]string{q.keyOfA}, nil))
}
