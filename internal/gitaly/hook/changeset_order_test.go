package hook

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// changesetOrderSetup is a repository with a branch history base -> a -> b and a hook manager that
// records its reference updates in the changeset log.
type changesetOrderSetup struct {
	repoPath string
	// first moves the branch from base to a, second from a to b.
	first, second string
	run           func(state ReferenceTransactionState, changes string) error
}

func setupChangesetOrder(t *testing.T, ctx context.Context) changesetOrderSetup {
	t.Helper()

	cfg := testcfg.Build(t)
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("base"))
	a := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithMessage("a"))
	b := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(a), gittest.WithMessage("b"))

	manager := NewManager(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t), gittest.NewCommandFactory(t, cfg), nil, &transaction.MockManager{}, gitlab.NewMockClient(
		t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive,
	), NewPostReceiveRegistry())

	payload, err := gitcmd.NewHooksPayload(ctx, cfg, repo, gittest.DefaultObjectHash, nil, &gitcmd.UserDetails{
		UserID:   "user-1",
		Username: "user",
		Protocol: "web",
	}, gitcmd.ReferenceTransactionHook, nil).Env()
	require.NoError(t, err)

	return changesetOrderSetup{
		repoPath: repoPath,
		first:    fmt.Sprintf("%s %s refs/heads/main\n", base, a),
		second:   fmt.Sprintf("%s %s refs/heads/main\n", a, b),
		run: func(state ReferenceTransactionState, changes string) error {
			return manager.ReferenceTransactionHook(ctx, state, []string{payload}, strings.NewReader(changes))
		},
	}
}

// recordedChanges returns the hook input that corresponds to each entry of the changeset log.
func recordedChanges(t *testing.T, repoPath string) []string {
	t.Helper()

	entries, err := changelog.List(repoPath, 0, 100)
	require.NoError(t, err)

	var changes []string
	for i, entry := range entries {
		require.Equal(t, uint64(i+1), entry.Sequence)
		changes = append(changes, fmt.Sprintf("%s %s %s\n", entry.OldOID, entry.NewOID, entry.Reference))
	}

	return changes
}

func TestReferenceTransactionHook_changesetOrder(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	t.Run("committed phases record in the order of the prepared phases", func(t *testing.T) {
		t.Parallel()

		setup := setupChangesetOrder(t, ctx)

		// The first transaction updated the branch and released its locks, so the second one
		// could update the branch. The committed phase of the second transaction runs before
		// the one of the first.
		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.first))
		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.second))

		secondDone := make(chan error, 1)
		go func() { secondDone <- setup.run(ReferenceTransactionCommitted, setup.second) }()

		select {
		case err := <-secondDone:
			require.FailNow(t, "second transaction recorded before the first", "err: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		require.Empty(t, recordedChanges(t, setup.repoPath))

		require.NoError(t, setup.run(ReferenceTransactionCommitted, setup.first))
		require.NoError(t, <-secondDone)

		require.Equal(t, []string{setup.first, setup.second}, recordedChanges(t, setup.repoPath))
	})

	t.Run("aborted transaction is not waited for", func(t *testing.T) {
		t.Parallel()

		setup := setupChangesetOrder(t, ctx)

		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.first))
		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.second))
		require.NoError(t, setup.run(ReferenceTransactionAborted, setup.first))
		require.NoError(t, setup.run(ReferenceTransactionCommitted, setup.second))

		require.Equal(t, []string{setup.second}, recordedChanges(t, setup.repoPath))
	})

	t.Run("transaction without prepared phase records immediately", func(t *testing.T) {
		t.Parallel()

		setup := setupChangesetOrder(t, ctx)

		require.NoError(t, setup.run(ReferenceTransactionCommitted, setup.first))
		require.Equal(t, []string{setup.first}, recordedChanges(t, setup.repoPath))
	})

	t.Run("canceled context stops waiting", func(t *testing.T) {
		t.Parallel()

		setup := setupChangesetOrder(t, ctx)

		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.first))
		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.second))

		// The ticket of the first transaction is still pending, but the caller of the second
		// committed phase went away. The update is recorded regardless.
		repoPath := setup.repoPath
		ticket, earlier := findChangesetTicket(repoPath, []byte(setup.second))
		require.NotNil(t, ticket)
		require.Len(t, earlier, 1)

		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		require.False(t, waitForChangesetTickets(canceledCtx, earlier))

		require.NoError(t, setup.run(ReferenceTransactionAborted, setup.first))
		require.NoError(t, setup.run(ReferenceTransactionCommitted, setup.second))
		require.Equal(t, []string{setup.second}, recordedChanges(t, setup.repoPath))
	})
}

// TestReferenceTransactionHook_changesetOrderExpiredTicket is not parallel because it lowers the
// ticket timeout of the package.
func TestReferenceTransactionHook_changesetOrderExpiredTicket(t *testing.T) {
	ctx := testhelper.Context(t)

	defaultTimeout := changesetTicketTimeout
	changesetTicketTimeout = 100 * time.Millisecond
	t.Cleanup(func() { changesetTicketTimeout = defaultTimeout })

	setup := setupChangesetOrder(t, ctx)

	// The first transaction never reports its outcome, as if its process died. The second one
	// records once the ticket of the first one expired.
	require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.first))
	require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.second))

	start := time.Now()
	require.NoError(t, setup.run(ReferenceTransactionCommitted, setup.second))
	require.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond)

	require.Equal(t, []string{setup.second}, recordedChanges(t, setup.repoPath))

	// The expired ticket is dropped when the next transaction is prepared.
	require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.first))
	changesetTickets.Lock()
	queue := changesetTickets.queues[setup.repoPath]
	changesetTickets.Unlock()
	require.Len(t, queue, 1)
	require.NoError(t, setup.run(ReferenceTransactionAborted, setup.first))
}

func TestReferenceTransactionHook_changesetOrderDisabled(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Changelog.Disabled = true

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	commitID := gittest.WriteCommit(t, cfg, repoPath)

	manager := NewManager(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t), gittest.NewCommandFactory(t, cfg), nil, &transaction.MockManager{}, gitlab.NewMockClient(
		t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive,
	), NewPostReceiveRegistry())

	payload, err := gitcmd.NewHooksPayload(ctx, cfg, repo, gittest.DefaultObjectHash, nil, nil, gitcmd.ReferenceTransactionHook, nil).Env()
	require.NoError(t, err)

	changes := fmt.Sprintf("%s %s %s\n", gittest.DefaultObjectHash.ZeroOID, commitID, git.ReferenceName("refs/heads/main"))
	require.NoError(t, manager.ReferenceTransactionHook(ctx, ReferenceTransactionPrepared, []string{payload}, strings.NewReader(changes)))

	// Without the changeset log, transactions are not ordered.
	changesetTickets.Lock()
	_, ok := changesetTickets.queues[repoPath]
	changesetTickets.Unlock()
	require.False(t, ok)
}
