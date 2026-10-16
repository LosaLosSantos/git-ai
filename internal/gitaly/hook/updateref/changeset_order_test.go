package updateref_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// pausingHookManager pauses the committed phase of the reference-transaction hook for one hook
// input until it is released, as a scheduling delay of the process would.
type pausingHookManager struct {
	hook.Manager
	pauseOn string
	paused  chan struct{}
	release chan struct{}
}

func (m *pausingHookManager) ReferenceTransactionHook(ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
	changes, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}

	if state == hook.ReferenceTransactionCommitted && string(changes) == m.pauseOn {
		close(m.paused)
		<-m.release
	}

	return m.Manager.ReferenceTransactionHook(ctx, state, env, bytes.NewReader(changes))
}

func TestUpdaterWithHooks_changesetOrder(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	logger := testhelper.NewLogger(t)
	locator := config.NewLocator(cfg)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithMessage("base"))
	a := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithMessage("a"))
	b := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(a), gittest.WithMessage("b"))

	reference := git.ReferenceName("refs/heads/main")
	manager := &pausingHookManager{
		Manager: hook.NewManager(cfg, locator, logger, gitCmdFactory, nil, nil, gitlab.NewMockClient(
			t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive,
		), hook.NewPostReceiveRegistry()),
		pauseOn: fmt.Sprintf("%s %s %s\n", base, a, reference),
		paused:  make(chan struct{}),
		release: make(chan struct{}),
	}
	updater := updateref.NewUpdaterWithHooks(cfg, logger, locator, manager, gitCmdFactory, nil)

	// The first update moves the branch from base to a and is paused after the branch was
	// updated and unlocked, before its committed phase records it.
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- updater.UpdateReference(ctx, repo, gittest.TestUser, nil, reference, a, base)
	}()
	<-manager.paused

	// The second update builds on the first one, which is visible already.
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- updater.UpdateReference(ctx, repo, gittest.TestUser, nil, reference, b, a)
	}()

	require.Eventually(t, func() bool {
		return gittest.ResolveRevision(t, cfg, repoPath, reference.String()) == b
	}, 10*time.Second, 10*time.Millisecond)

	// The second update must not be recorded before the first one.
	select {
	case err := <-secondDone:
		require.FailNow(t, "second update finished while the first one was not recorded", "err: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	entries, err := changelog.List(repoPath, 0, 10)
	require.NoError(t, err)
	require.Empty(t, entries)

	close(manager.release)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)

	entries, err = changelog.List(repoPath, 0, 10)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	require.Equal(t, uint64(1), entries[0].Sequence)
	require.Equal(t, base.String(), entries[0].OldOID)
	require.Equal(t, a.String(), entries[0].NewOID)

	require.Equal(t, uint64(2), entries[1].Sequence)
	require.Equal(t, a.String(), entries[1].OldOID)
	require.Equal(t, b.String(), entries[1].NewOID)
}
