package hook

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/eager"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// recordingEnqueuer records the commits that the eager observer enqueues.
type recordingEnqueuer struct {
	mu      sync.Mutex
	commits []git.ObjectID
}

func (e *recordingEnqueuer) Enqueue(_ storage.Repository, commits []git.ObjectID) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.commits = append(e.commits, commits...)
}

func (e *recordingEnqueuer) enqueued() []git.ObjectID {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]git.ObjectID(nil), e.commits...)
}

// TestReferenceTransactionHook_observesCommittedUpdates checks that a committed
// reference update reaches the eager observer, whether or not the changeset log
// records it, and that the other states of the transaction do not.
func TestReferenceTransactionHook_observesCommittedUpdates(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc             string
		changelogDisable bool
	}{
		{desc: "changelog enabled"},
		{desc: "changelog disabled", changelogDisable: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			cfg.Graft.Changelog.Disabled = tc.changelogDisable

			repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})

			var (
				listMu sync.Mutex
				listed [][2]string
				writes int
				mu     sync.Mutex
			)
			enqueuer := &recordingEnqueuer{}
			observer := eager.NewObserver(ctx, testhelper.SharedLogger(t),
				eager.Options{Eager: true, CommitGraph: true, Debounce: 5 * time.Millisecond},
				eager.Deps{
					Enqueuer: enqueuer,
					ListNewCommits: func(_ context.Context, _ storage.Repository, oldOID, newOID string) ([]git.ObjectID, error) {
						listMu.Lock()
						defer listMu.Unlock()

						listed = append(listed, [2]string{oldOID, newOID})
						return []git.ObjectID{git.ObjectID(newOID)}, nil
					},
					WriteCommitGraph: func(context.Context, storage.Repository) error {
						mu.Lock()
						defer mu.Unlock()

						writes++
						return nil
					},
				},
			)
			defer observer.Close()

			manager := newEagerTestManager(t, cfg, WithEagerObserver(observer))

			oldOID := gittest.DefaultObjectHash.EmptyTreeOID.String()
			newOID := strings.Repeat("b", 40)
			changes := fmt.Sprintf("%s %s refs/heads/main\n", oldOID, newOID)

			env, err := gitcmd.NewHooksPayload(ctx, cfg, repo, gittest.DefaultObjectHash, nil, nil, gitcmd.ReferenceTransactionHook, nil).Env()
			require.NoError(t, err)

			require.NoError(t, manager.ReferenceTransactionHook(ctx, ReferenceTransactionPrepared, []string{env}, strings.NewReader(changes)))
			require.NoError(t, manager.ReferenceTransactionHook(ctx, ReferenceTransactionAborted, []string{env}, strings.NewReader(changes)))

			listMu.Lock()
			require.Empty(t, listed, "only the committed state is observed")
			listMu.Unlock()

			require.NoError(t, manager.ReferenceTransactionHook(ctx, ReferenceTransactionCommitted, []string{env}, strings.NewReader(changes)))

			require.Eventually(t, func() bool { return len(enqueuer.enqueued()) == 1 }, 2*time.Second, time.Millisecond)
			require.Equal(t, []git.ObjectID{git.ObjectID(newOID)}, enqueuer.enqueued())

			listMu.Lock()
			require.Equal(t, [][2]string{{oldOID, newOID}}, listed)
			listMu.Unlock()

			require.Eventually(t, func() bool {
				mu.Lock()
				defer mu.Unlock()

				return writes == 1
			}, 2*time.Second, time.Millisecond)
		})
	}
}

// newEagerTestManager returns a hook manager for tests of the eager triggers.
func newEagerTestManager(t *testing.T, cfg config.Cfg, opts ...ManagerOption) *GitLabHookManager {
	t.Helper()

	return NewManager(
		cfg,
		config.NewLocator(cfg),
		testhelper.SharedLogger(t),
		gittest.NewCommandFactory(t, cfg),
		nil,
		transaction.NewManager(cfg, testhelper.SharedLogger(t), backchannel.NewRegistry()),
		gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive),
		NewPostReceiveRegistry(),
		opts...,
	)
}
