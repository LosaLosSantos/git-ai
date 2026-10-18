package rebase

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/stretchr/testify/require"
)

// TestEngineRecoversFromBrokenMergeTreeStream merges a commit whose tree references a blob that was
// never written. git merge-tree aborts on it, which ends the stream. The rebase of a later,
// unrelated branch must still succeed with the same result as localrepo.
func TestEngineRecoversFromBrokenMergeTreeStream(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	base := f.commit(state{"a": "base\n"}, "base")
	upstream := f.commit(state{"a": "base\n", "m": "upstream\n"}, "upstream", base)
	good := f.commit(state{"a": "base\n", "g": "good\n"}, "good", base)

	// The blob is only hashed, never written, so the tree refers to a missing object.
	missingBlob := strings.TrimSpace(string(gittest.ExecOpts(t, f.cfg,
		gittest.ExecConfig{Stdin: strings.NewReader("never written\n")},
		"-C", f.repoPath, "hash-object", "--stdin")))
	tree := strings.TrimSpace(string(gittest.ExecOpts(t, f.cfg,
		gittest.ExecConfig{Stdin: strings.NewReader(fmt.Sprintf("100644 blob %s\tm\n", missingBlob))},
		"-C", f.repoPath, "mktree", "--missing")))
	bad := git.ObjectID(strings.TrimSpace(string(gittest.Exec(t, f.cfg,
		"-C", f.repoPath, "commit-tree", tree, "-p", base.String(), "-m", "bad"))))

	fb := &fakeBackends{repo: f.repo}
	engine := newEngine(f.repo, fb.backends())
	t.Cleanup(func() { require.NoError(t, engine.Close()) })

	_, err := landRebase(f.ctx, f.repo, engine, upstream, bad, nil)
	require.Error(t, err)
	require.Equal(t, 1, fb.streamStarts)
	require.Equal(t, 1, fb.streamClosed, "the broken stream is closed")

	// The next rebase starts a new stream and gives the result of localrepo.
	require.NoError(t, requireSameRebase(t, f, engine, upstream, good, &testCommitter))
	require.Equal(t, 2, fb.streamStarts)
}

// TestEngineRecoversAfterMergeError makes the first merge fail without a conflict by using a stream
// that reports an error, and checks that the stream is replaced for the next merge.
func TestEngineRecoversAfterMergeError(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, upstream, branch := linearBranches(f)

	fb := &fakeBackends{repo: f.repo}
	failFirst := &failingStreamBackends{inner: fb.backends(), fails: 1}
	engine := newEngine(f.repo, failFirst.backends())
	t.Cleanup(func() { require.NoError(t, engine.Close()) })

	_, err := landRebase(f.ctx, f.repo, engine, upstream, branch, &testCommitter)
	require.ErrorIs(t, err, errInjected)
	require.Equal(t, 1, fb.streamClosed, "the stream that failed is closed")

	require.NoError(t, requireSameRebase(t, f, engine, upstream, branch, &testCommitter))
	require.Equal(t, 2, fb.streamStarts, "the next rebase starts a new stream")
}

// TestEngineReplacesHelpersOfCanceledContext checks that helpers started with a context that was
// canceled afterwards do not fail the rebases that follow, even with a live context.
func TestEngineReplacesHelpersOfCanceledContext(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, upstream, branch := linearBranches(f)

	fb := &fakeBackends{repo: f.repo}
	engine := newEngine(f.repo, fb.backends())

	ctx, cancel := context.WithCancel(f.ctx)
	want, err := landRebase(ctx, f.repo, engine, upstream, branch, &testCommitter)
	require.NoError(t, err)
	require.Equal(t, 1, fb.streamStarts)
	require.Equal(t, 1, fb.writerStarts)

	cancel()

	got, err := landRebase(f.ctx, f.repo, engine, upstream, branch, &testCommitter)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, 2, fb.streamStarts)
	require.Equal(t, 2, fb.writerStarts)

	// The helpers of the canceled context were replaced and closed. Close closes the current ones.
	require.Equal(t, 1, fb.streamClosed)
	require.Equal(t, 1, fb.writerClosed)
	require.NoError(t, engine.Close())
	require.Equal(t, 2, fb.streamClosed)
	require.Equal(t, 2, fb.writerClosed)
}

var errInjected = fmt.Errorf("injected merge failure")

// failingStreamBackends makes the first fails merges of the streams it starts return errInjected
// without running them.
type failingStreamBackends struct {
	inner backends
	fails int
}

func (fsb *failingStreamBackends) backends() backends {
	b := fsb.inner
	newMergeStream := b.newMergeStream
	b.newMergeStream = func(ctx context.Context) (mergeStream, error) {
		stream, err := newMergeStream(ctx)
		if err != nil {
			return nil, err
		}
		return &injectingStream{mergeStream: stream, owner: fsb}, nil
	}
	return b
}

// injectingStream is a mergeStream that fails the merges while its owner has failures left.
type injectingStream struct {
	mergeStream
	owner *failingStreamBackends
}

func (s *injectingStream) Merge(ctx context.Context, base, ours, theirs git.ObjectID) (git.ObjectID, error) {
	if s.owner.fails > 0 {
		s.owner.fails--
		return "", errInjected
	}
	return s.mergeStream.Merge(ctx, base, ours, theirs)
}
