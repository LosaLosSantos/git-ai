package hook

import (
	"context"
	"fmt"
	"os"
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
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// changesetOrderSetup is a repository with a branch history base -> a -> b -> c and a hook manager
// that records its reference updates in the changeset log.
type changesetOrderSetup struct {
	repoPath string
	// first moves the branch from base to a, second from a to b, third from b to c. other creates
	// another branch at base.
	first, second, third, other string
	run                         func(state ReferenceTransactionState, changes string) error
	// runWith runs the hook with the context ctx.
	runWith func(ctx context.Context, state ReferenceTransactionState, changes string) error
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
	c := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(b), gittest.WithMessage("c"))

	manager := NewManager(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t), gittest.NewCommandFactory(t, cfg), nil, &transaction.MockManager{}, gitlab.NewMockClient(
		t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive,
	), NewPostReceiveRegistry())

	payload, err := gitcmd.NewHooksPayload(ctx, cfg, repo, gittest.DefaultObjectHash, nil, &gitcmd.UserDetails{
		UserID:   "user-1",
		Username: "user",
		Protocol: "web",
	}, gitcmd.ReferenceTransactionHook, nil).Env()
	require.NoError(t, err)

	runWith := func(ctx context.Context, state ReferenceTransactionState, changes string) error {
		return manager.ReferenceTransactionHook(ctx, state, []string{payload}, strings.NewReader(changes))
	}

	return changesetOrderSetup{
		repoPath: repoPath,
		first:    fmt.Sprintf("%s %s refs/heads/main\n", base, a),
		second:   fmt.Sprintf("%s %s refs/heads/main\n", a, b),
		third:    fmt.Sprintf("%s %s refs/heads/main\n", b, c),
		other:    fmt.Sprintf("%s %s refs/heads/other\n", gittest.DefaultObjectHash.ZeroOID, base),
		run: func(state ReferenceTransactionState, changes string) error {
			return runWith(ctx, state, changes)
		},
		runWith: runWith,
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

	t.Run("a committed phase whose caller went away still records in order", func(t *testing.T) {
		t.Parallel()

		setup := setupChangesetOrder(t, ctx)

		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.first))
		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.second))

		// The caller of the second committed phase went away while the first transaction is
		// still pending. Its update is committed in git, so it is recorded all the same, after
		// the first one.
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		secondDone := make(chan error, 1)
		go func() { secondDone <- setup.runWith(canceledCtx, ReferenceTransactionCommitted, setup.second) }()

		select {
		case err := <-secondDone:
			require.FailNow(t, "second transaction recorded before the first", "err: %v", err)
		case <-time.After(200 * time.Millisecond):
		}

		require.NoError(t, setup.run(ReferenceTransactionCommitted, setup.first))
		require.NoError(t, <-secondDone)
		require.Equal(t, []string{setup.first, setup.second}, recordedChanges(t, setup.repoPath))
	})

	t.Run("transactions on other references are recorded in order too", func(t *testing.T) {
		t.Parallel()

		setup := setupChangesetOrder(t, ctx)

		// A later transaction may have read what an earlier one wrote to another reference, so it
		// is recorded after it.
		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.other))
		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.first))

		firstDone := make(chan error, 1)
		go func() { firstDone <- setup.run(ReferenceTransactionCommitted, setup.first) }()

		select {
		case err := <-firstDone:
			require.FailNow(t, "transaction recorded before an earlier one on another reference", "err: %v", err)
		case <-time.After(200 * time.Millisecond):
		}

		require.NoError(t, setup.run(ReferenceTransactionCommitted, setup.other))
		require.NoError(t, <-firstDone)
		require.Equal(t, []string{setup.other, setup.first}, recordedChanges(t, setup.repoPath))
	})

	t.Run("ready transactions are written as one group", func(t *testing.T) {
		t.Parallel()

		setup := setupChangesetOrder(t, ctx)

		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.first))
		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.second))
		require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.third))

		// The committed phases of the later transactions run first and wait for the first one.
		laterDone := make(chan error, 2)
		for _, changes := range []string{setup.third, setup.second} {
			go func() { laterDone <- setup.run(ReferenceTransactionCommitted, changes) }()
		}

		// Both later tickets are ready before the first transaction commits.
		require.Eventually(t, func() bool {
			changesetTickets.Lock()
			defer changesetTickets.Unlock()
			q := changesetTickets.queues[setup.repoPath]
			return q != nil && len(q.tickets) == 3 && q.tickets[1].record != nil && q.tickets[2].record != nil
		}, 10*time.Second, 5*time.Millisecond)
		require.Empty(t, recordedChanges(t, setup.repoPath))

		// The first committed phase writes all three with one write: their entries share the
		// time of that write. It returns only once its entry is durable, so it is in the log.
		require.NoError(t, setup.run(ReferenceTransactionCommitted, setup.first))
		require.Equal(t, []string{setup.first, setup.second, setup.third}, recordedChanges(t, setup.repoPath))
		require.NoError(t, <-laterDone)
		require.NoError(t, <-laterDone)

		entries, err := changelog.List(setup.repoPath, 0, 10)
		require.NoError(t, err)
		require.Len(t, entries, 3)
		require.Equal(t, entries[0].RecordedAt, entries[1].RecordedAt)
		require.Equal(t, entries[0].RecordedAt, entries[2].RecordedAt)

		changesetTickets.Lock()
		_, ok := changesetTickets.queues[setup.repoPath]
		changesetTickets.Unlock()
		require.False(t, ok, "the queue is dropped once it is empty")
	})
}

// TestReferenceTransactionHook_changesetRecordsLost is not parallel because it reads the counter
// of lost records, which is global.
func TestReferenceTransactionHook_changesetRecordsLost(t *testing.T) {
	ctx := testhelper.Context(t)
	setup := setupChangesetOrder(t, ctx)

	// The log cannot be written: a directory takes the place of its file.
	require.NoError(t, os.MkdirAll(changelog.Path(setup.repoPath), 0o755))

	before := testutil.ToFloat64(changelogRecordsLostTotal)
	require.NoError(t, setup.run(ReferenceTransactionPrepared, setup.first))
	require.NoError(t, setup.run(ReferenceTransactionCommitted, setup.first), "recording never fails the update")
	require.InDelta(t, 1, testutil.ToFloat64(changelogRecordsLostTotal)-before, 0)
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
	queue := changesetTickets.queues[setup.repoPath].tickets
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
