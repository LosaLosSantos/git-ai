package verify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

// testScript is the test command of the fixtures. It records the commit under
// test in $RUNS, waits for the $GATE file when one is set, and fails while a
// file named broken is present in the tree.
const testScript = `#!/bin/sh
echo "$GRAFT_VERIFY_COMMIT" >> "$RUNS"
if [ -n "$GATE" ]; then
  while [ ! -e "$GATE" ]; do sleep 0.01; done
fi
if [ -e broken ]; then
  echo "broken file present" >&2
  exit 1
fi
exit 0
`

// liveBackend is an in-memory repository whose changeset watch keeps running
// after the backlog, like the server's watch does.
type liveBackend struct {
	*workspace.MemoryBackend
}

// ResolveRevision maps refs/heads/<name> to the branch name of the memory
// repository, as git resolves it.
func (b *liveBackend) ResolveRevision(ctx context.Context, rev string) (string, error) {
	return b.MemoryBackend.ResolveRevision(ctx, strings.TrimPrefix(rev, "refs/heads/"))
}

// WatchChangesets delivers the backlog after the cursor and then new changesets
// until ctx is done.
func (b *liveBackend) WatchChangesets(ctx context.Context, after uint64) (<-chan workspace.Changeset, <-chan error) {
	out := make(chan workspace.Changeset)
	errs := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errs)

		cursor := after
		for {
			batch, err := b.MemoryBackend.Changesets(ctx, cursor, 100)
			if err != nil {
				errs <- err
				return
			}
			for _, c := range batch {
				select {
				case out <- c:
					cursor = c.Sequence
				case <-ctx.Done():
					return
				}
			}
			if len(batch) == 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Millisecond):
				}
			}
		}
	}()

	return out, errs
}

// fakeGreens is an in-memory green ref store with compare-and-swap moves.
type fakeGreens struct {
	mu       sync.Mutex
	refs     map[string]string
	moves    int
	failMove error
}

func newFakeGreens() *fakeGreens {
	return &fakeGreens{refs: map[string]string{}}
}

// Current implements greenRefs.
func (g *fakeGreens) Current(_ context.Context, ref string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refs[ref], nil
}

// Move implements greenRefs.
func (g *fakeGreens) Move(_ context.Context, ref, old, next string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.failMove != nil {
		return g.failMove
	}
	if g.refs[ref] != old {
		return fmt.Errorf("reference %s is %q, not %q", ref, g.refs[ref], old)
	}
	g.refs[ref] = next
	g.moves++
	return nil
}

// get returns the value of ref.
func (g *fakeGreens) get(ref string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refs[ref]
}

// fixture is a verifier over an in-memory repository, with a test command
// that records its runs.
type fixture struct {
	t       *testing.T
	backend *liveBackend
	greens  *fakeGreens
	cfg     Config
	runs    string
	gate    string
}

// newFixture creates a fixture. mutate may change the configuration before
// it is finalized.
func newFixture(t *testing.T, mutate func(*Config)) *fixture {
	t.Helper()

	dir := t.TempDir()
	script := filepath.Join(dir, "test.sh")
	require.NoError(t, os.WriteFile(script, []byte(testScript), 0o700))

	fx := &fixture{
		t:       t,
		backend: &liveBackend{MemoryBackend: workspace.NewMemoryBackend()},
		greens:  newFakeGreens(),
		runs:    filepath.Join(dir, "runs.log"),
		gate:    filepath.Join(dir, "gate"),
	}
	// The gate is open unless a test closes it by removing the file.
	require.NoError(t, os.WriteFile(fx.gate, nil, 0o600))

	cfg := Config{
		Address:     "unix:/unused",
		Repository:  "project.git",
		Branch:      "main",
		WorkDir:     filepath.Join(dir, "work"),
		StateFile:   filepath.Join(dir, "state.json"),
		BatchWindow: Duration(20 * time.Millisecond),
		Test: TestConfig{
			Command: []string{"sh", script},
			Env:     []string{"RUNS=" + fx.runs, "GATE=" + fx.gate},
			Timeout: Duration(time.Minute),
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	require.NoError(t, cfg.Finalize())
	fx.cfg = cfg
	return fx
}

// verifier returns a verifier over the fixture, loaded from its state file.
func (fx *fixture) verifier() *Verifier {
	fx.t.Helper()

	v, err := newVerifier(context.Background(), fx.cfg, fx.backend, fx.greens, discardLogger())
	require.NoError(fx.t, err)
	return v
}

// commit lands a commit that writes path on main and returns its ID.
func (fx *fixture) commit(path, content string) string {
	return fx.backend.Put("main", path, []byte(content))
}

// runsLog returns the commits that the test command ran on, in order.
func (fx *fixture) runsLog() []string {
	data, err := os.ReadFile(fx.runs)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(fx.t, err)

	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// treeFile returns the path of a file in the work tree.
func (fx *fixture) treeFile(path string) string {
	return filepath.Join(fx.cfg.WorkDir, treeDirName, path)
}

// green returns the green ref of the fixture.
func (fx *fixture) green() string {
	return fx.greens.get(fx.cfg.GreenRef)
}

// discardLogger returns a logger that drops everything.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
