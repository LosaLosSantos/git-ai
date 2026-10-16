package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/quarantine"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/grpc/metadata"
	"github.com/feder-cr/git-ai/internal/helper"
	"github.com/feder-cr/git-ai/internal/helper/duration"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/transaction/txinfo"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrintAlert(t *testing.T) {
	testCases := []struct {
		message  string
		expected string
	}{
		{
			message: "Lorem ipsum dolor sit amet, consectetur adipiscing elit. Curabitur nec mi lectus. Fusce eu ligula in odio hendrerit posuere. Ut semper neque vitae maximus accumsan. In malesuada justo nec leo congue egestas. Vivamus interdum nec libero ac convallis. Praesent euismod et nunc vitae vulputate. Mauris tincidunt ligula urna, bibendum vestibulum sapien luctus eu. Donec sed justo in erat dictum semper. Ut porttitor augue in felis gravida scelerisque. Morbi dolor justo, accumsan et nulla vitae, luctus consectetur est. Donec aliquet erat pellentesque suscipit elementum. Cras posuere eros ipsum, a tincidunt tortor laoreet quis. Mauris varius nulla vitae placerat imperdiet. Vivamus ut ligula odio. Cras nec euismod ligula.",
			expected: `========================================================================

   Lorem ipsum dolor sit amet, consectetur adipiscing elit. Curabitur
  nec mi lectus. Fusce eu ligula in odio hendrerit posuere. Ut semper
    neque vitae maximus accumsan. In malesuada justo nec leo congue
  egestas. Vivamus interdum nec libero ac convallis. Praesent euismod
    et nunc vitae vulputate. Mauris tincidunt ligula urna, bibendum
  vestibulum sapien luctus eu. Donec sed justo in erat dictum semper.
  Ut porttitor augue in felis gravida scelerisque. Morbi dolor justo,
  accumsan et nulla vitae, luctus consectetur est. Donec aliquet erat
      pellentesque suscipit elementum. Cras posuere eros ipsum, a
   tincidunt tortor laoreet quis. Mauris varius nulla vitae placerat
      imperdiet. Vivamus ut ligula odio. Cras nec euismod ligula.

========================================================================`,
		},
		{
			message: "Lorem ipsum dolor sit amet, consectetur",
			expected: `========================================================================

                Lorem ipsum dolor sit amet, consectetur

========================================================================`,
		},
	}

	for _, tc := range testCases {
		var result bytes.Buffer

		require.NoError(t, printAlert(gitlab.PostReceiveMessage{Message: tc.message}, &result))
		assert.Equal(t, tc.expected, result.String())
	}
}

func TestCountRefUpdates(t *testing.T) {
	t.Parallel()

	zero := gittest.DefaultObjectHash.ZeroOID.String()
	oid := strings.Repeat("1", len(zero))

	for _, tc := range []struct {
		desc     string
		changes  string
		expected refUpdateStats
	}{
		{
			desc:     "empty",
			changes:  "",
			expected: refUpdateStats{},
		},
		{
			desc:     "single creation",
			changes:  fmt.Sprintf("%s %s refs/heads/new\n", zero, oid),
			expected: refUpdateStats{total: 1, created: 1},
		},
		{
			desc:     "single deletion",
			changes:  fmt.Sprintf("%s %s refs/heads/gone\n", oid, zero),
			expected: refUpdateStats{total: 1, deleted: 1},
		},
		{
			desc:     "single update",
			changes:  fmt.Sprintf("%s %s refs/heads/master\n", oid, oid),
			expected: refUpdateStats{total: 1, updated: 1},
		},
		{
			desc: "mixed updates",
			changes: fmt.Sprintf("%s %s refs/heads/new\n%s %s refs/heads/gone\n%s %s refs/heads/master\n",
				zero, oid, oid, zero, oid, oid),
			expected: refUpdateStats{total: 3, created: 1, deleted: 1, updated: 1},
		},
		{
			desc:     "malformed lines are skipped",
			changes:  fmt.Sprintf("garbage\n%s %s refs/heads/master\n", oid, oid),
			expected: refUpdateStats{total: 1, updated: 1},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.expected, countRefUpdates([]byte(tc.changes)))
		})
	}
}

// BenchmarkCountRefUpdates measures parsing of the reference-update list that
// git-receive-pack feeds the post-receive hook on stdin. It covers a range of
// push shapes, from a single-branch push to a mass ref update, cycling through
// creations, deletions and updates so all classification branches are hit.
func BenchmarkCountRefUpdates(b *testing.B) {
	zero := gittest.DefaultObjectHash.ZeroOID.String()
	oid := strings.Repeat("1", len(zero))

	generate := func(refs int) []byte {
		var buf bytes.Buffer
		for i := 0; i < refs; i++ {
			oldOID, newOID := oid, oid
			switch i % 3 {
			case 0:
				oldOID = zero // creation
			case 1:
				newOID = zero // deletion
			}
			fmt.Fprintf(&buf, "%s %s refs/heads/branch-%d\n", oldOID, newOID, i)
		}
		return buf.Bytes()
	}

	for _, refs := range []int{1, 10, 100, 1000} {
		changes := generate(refs)

		b.Run(fmt.Sprintf("refs=%d", refs), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(changes)))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_ = countRefUpdates(changes)
			}
		})
	}
}

func TestPostReceive_recordsReferenceUpdateCustomFields(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	// The custom fields are normally injected by the gRPC interceptor; do the
	// same here so the hook can record onto them.
	ctx = log.InitContextCustomFields(ctx)
	cfg := testcfg.Build(t)

	repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	gitCmdFactory := gittest.NewCommandFactory(t, cfg)
	txManager := transaction.NewTrackingManager()
	hookManager := NewManager(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t), gitCmdFactory, nil, txManager, gitlab.NewMockClient(
		t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive,
	), NewPostReceiveRegistry())

	payload, err := gitcmd.NewHooksPayload(
		ctx,
		cfg,
		repo,
		gittest.DefaultObjectHash,
		nil,
		&gitcmd.UserDetails{UserID: "1234", Username: "user", Protocol: "web"},
		gitcmd.PostReceiveHook,
		featureflag.FromContext(ctx),
	).Env()
	require.NoError(t, err)

	zero := gittest.DefaultObjectHash.ZeroOID.String()
	oid := strings.Repeat("1", len(zero))
	stdin := fmt.Sprintf("%s %s refs/heads/new\n%s %s refs/heads/gone\n%s %s refs/heads/master\n",
		zero, oid, oid, zero, oid, oid)

	var stdout, stderr bytes.Buffer
	require.NoError(t, hookManager.PostReceiveHook(ctx, repo, nil, []string{payload}, strings.NewReader(stdin), &stdout, &stderr))

	customFields := log.CustomFieldsFromContext(ctx)
	require.NotNil(t, customFields)

	require.Equal(t, log.Fields{
		"post_receive_pack.ref_updates.total":   3,
		"post_receive_pack.ref_updates.created": 1,
		"post_receive_pack.ref_updates.deleted": 1,
		"post_receive_pack.ref_updates.updated": 1,
	}, customFields.Fields())

	require.Equal(t, 1.0, promtest.ToFloat64(hookManager.refUpdatesCounter.WithLabelValues("created")))
	require.Equal(t, 1.0, promtest.ToFloat64(hookManager.refUpdatesCounter.WithLabelValues("deleted")))
	require.Equal(t, 1.0, promtest.ToFloat64(hookManager.refUpdatesCounter.WithLabelValues("updated")))
}

func TestPostReceive_customHook(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	gitCmdFactory := gittest.NewCommandFactory(t, cfg)
	locator := config.NewLocator(cfg)

	txManager := transaction.NewTrackingManager()
	hookManager := NewManager(cfg, locator, testhelper.SharedLogger(t), gitCmdFactory, nil, txManager, gitlab.NewMockClient(
		t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive,
	), NewPostReceiveRegistry())

	receiveHooksPayload := &gitcmd.UserDetails{
		UserID:   "1234",
		Username: "user",
		Protocol: "web",
	}

	payload, err := gitcmd.NewHooksPayload(
		ctx,
		cfg,
		repo,
		gittest.DefaultObjectHash,
		nil,
		receiveHooksPayload,
		gitcmd.PostReceiveHook,
		featureflag.FromContext(ctx),
	).Env()
	require.NoError(t, err)

	primaryPayload, err := gitcmd.NewHooksPayload(
		ctx,
		cfg,
		repo,
		gittest.DefaultObjectHash,
		&txinfo.Transaction{
			ID: 1234, Node: "primary", Primary: true,
		},
		receiveHooksPayload,
		gitcmd.PostReceiveHook,
		featureflag.FromContext(ctx),
	).Env()
	require.NoError(t, err)

	secondaryPayload, err := gitcmd.NewHooksPayload(
		ctx,
		cfg,
		repo,
		gittest.DefaultObjectHash,
		&txinfo.Transaction{
			ID: 1234, Node: "secondary", Primary: false,
		},
		receiveHooksPayload,
		gitcmd.PostReceiveHook,
		featureflag.FromContext(ctx),
	).Env()
	require.NoError(t, err)

	testCases := []struct {
		desc           string
		env            []string
		pushOptions    []string
		hook           string
		stdin          string
		expectedErr    string
		expectedStdout string
		expectedStderr string
		expectedVotes  []transaction.PhasedVote
	}{
		{
			desc:           "hook receives environment variables",
			env:            []string{payload},
			stdin:          "changes\n",
			hook:           "#!/bin/sh\nenv | grep -v -e '^SHLVL=' -e '^_=' | sort\n",
			expectedStdout: strings.Join(getExpectedEnv(t, ctx, locator, gitCmdFactory, repo), "\n") + "\n",
			expectedVotes:  []transaction.PhasedVote{},
		},
		{
			desc:  "push options are passed through",
			env:   []string{payload},
			stdin: "changes\n",
			pushOptions: []string{
				"mr.merge_when_pipeline_succeeds",
				"mr.create",
			},
			hook: "#!/bin/sh\nenv | grep -e '^GIT_PUSH_OPTION' | sort\n",
			expectedStdout: strings.Join([]string{
				"GIT_PUSH_OPTION_0=mr.merge_when_pipeline_succeeds",
				"GIT_PUSH_OPTION_1=mr.create",
				"GIT_PUSH_OPTION_COUNT=2",
			}, "\n") + "\n",
			expectedVotes: []transaction.PhasedVote{},
		},
		{
			desc:           "hook can write to stderr and stdout",
			env:            []string{payload},
			stdin:          "changes\n",
			hook:           "#!/bin/sh\necho foo >&1 && echo bar >&2\n",
			expectedStdout: "foo\n",
			expectedStderr: "bar\n",
			expectedVotes:  []transaction.PhasedVote{},
		},
		{
			desc:           "hook receives standard input",
			env:            []string{payload},
			hook:           "#!/bin/sh\ncat\n",
			stdin:          "foo\n",
			expectedStdout: "foo\n",
			expectedVotes:  []transaction.PhasedVote{},
		},
		{
			desc:           "hook succeeds without consuming stdin",
			env:            []string{payload},
			hook:           "#!/bin/sh\necho foo\n",
			stdin:          "ignore me\n",
			expectedStdout: "foo\n",
			expectedVotes:  []transaction.PhasedVote{},
		},
		{
			desc:          "invalid hook results in error",
			env:           []string{payload},
			stdin:         "changes\n",
			hook:          "",
			expectedErr:   "exec format error",
			expectedVotes: []transaction.PhasedVote{},
		},
		{
			desc:          "failing hook results in error",
			env:           []string{payload},
			stdin:         "changes\n",
			hook:          "#!/bin/sh\nexit 123",
			expectedErr:   "exit status 123",
			expectedVotes: []transaction.PhasedVote{},
		},
		{
			desc:           "hook is executed on primary",
			env:            []string{primaryPayload},
			stdin:          "changes\n",
			hook:           "#!/bin/sh\necho foo\n",
			expectedStdout: "foo\n",
			expectedVotes:  []transaction.PhasedVote{},
		},
		{
			desc:          "hook is not executed on secondary",
			env:           []string{secondaryPayload},
			stdin:         "changes\n",
			hook:          "#!/bin/sh\necho foo\n",
			expectedVotes: []transaction.PhasedVote{},
		},
		{
			desc:          "missing changes cause error",
			env:           []string{payload},
			expectedErr:   "hook got no reference updates",
			expectedVotes: []transaction.PhasedVote{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			txManager.Reset()

			gittest.WriteCustomHook(t, repoPath, "post-receive", []byte(tc.hook))

			var stdout, stderr bytes.Buffer
			err = hookManager.PostReceiveHook(ctx, repo, tc.pushOptions, tc.env, strings.NewReader(tc.stdin), &stdout, &stderr)

			if tc.expectedErr != "" {
				require.Contains(t, err.Error(), tc.expectedErr)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, tc.expectedStdout, stdout.String())
			require.Equal(t, tc.expectedStderr, stderr.String())
			require.Equal(t, tc.expectedVotes, txManager.Votes())
		})
	}
}

type postreceiveAPIMock struct {
	postreceive func(context.Context, string, string, string, []byte, ...string) (bool, []gitlab.PostReceiveMessage, error)
}

func (m *postreceiveAPIMock) Allowed(ctx context.Context, params gitlab.AllowedParams) (bool, string, error) {
	return true, "", nil
}

func (m *postreceiveAPIMock) PreReceive(ctx context.Context, glRepository string) (bool, error) {
	return true, nil
}

func (m *postreceiveAPIMock) Check(ctx context.Context) (*gitlab.CheckInfo, error) {
	return nil, errors.New("unexpected call")
}

func (m *postreceiveAPIMock) PostReceive(ctx context.Context, glRepository, glID, changes string, clientCtx []byte, pushOptions ...string) (bool, []gitlab.PostReceiveMessage, error) {
	return m.postreceive(ctx, glRepository, glID, changes, clientCtx, pushOptions...)
}

func (m *postreceiveAPIMock) ObjectPoolMembers(ctx context.Context, diskPaths []string, storage string, upstreamOnly bool) (map[string][]gitlab.ObjectPoolMember, error) {
	return nil, errors.New("unexpected call")
}

func TestPostReceive_gitlab(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	ctx = metadata.AppendToIncomingContext(ctx, metadata.ClientContextMetadataKey, "foobar")

	payload, err := gitcmd.NewHooksPayload(
		ctx,
		cfg,
		repo,
		gittest.DefaultObjectHash,
		nil,
		&gitcmd.UserDetails{
			UserID:   "1234",
			Username: "user",
			Protocol: "web",
		}, gitcmd.PostReceiveHook, nil).Env()
	require.NoError(t, err)

	standardEnv := []string{payload}

	testCases := []struct {
		desc           string
		env            []string
		pushOptions    []string
		changes        string
		postreceive    func(*testing.T, context.Context, string, string, string, []byte, ...string) (bool, []gitlab.PostReceiveMessage, error)
		expectHookCall bool
		expectedErr    error
		expectedStdout string
		expectedStderr string
	}{
		{
			desc:    "allowed change",
			env:     standardEnv,
			changes: "changes\n",
			postreceive: func(t *testing.T, ctx context.Context, glRepo, glID, changes string, clientCtx []byte, pushOptions ...string) (bool, []gitlab.PostReceiveMessage, error) {
				require.Equal(t, repo.GetGlRepository(), glRepo)
				require.Equal(t, "1234", glID)
				require.Equal(t, "changes\n", changes)
				require.Empty(t, pushOptions)
				return true, nil, nil
			},
			expectedStdout: "hook called\n",
		},
		{
			desc: "push options are passed through",
			env:  standardEnv,
			pushOptions: []string{
				"mr.merge_when_pipeline_succeeds",
				"mr.create",
			},
			changes: "changes\n",
			postreceive: func(t *testing.T, ctx context.Context, glRepo, glID, changes string, clientCtx []byte, pushOptions ...string) (bool, []gitlab.PostReceiveMessage, error) {
				require.Equal(t, []string{
					"mr.merge_when_pipeline_succeeds",
					"mr.create",
				}, pushOptions)
				return true, nil, nil
			},
			expectedStdout: "hook called\n",
		},
		{
			desc:    "access denied without message",
			env:     standardEnv,
			changes: "changes\n",
			postreceive: func(t *testing.T, ctx context.Context, glRepo, glID, changes string, clientCtx []byte, pushOptions ...string) (bool, []gitlab.PostReceiveMessage, error) {
				return false, nil, nil
			},
			expectedErr: errors.New(""),
		},
		{
			desc:    "access denied with message",
			env:     standardEnv,
			changes: "changes\n",
			postreceive: func(t *testing.T, ctx context.Context, glRepo, glID, changes string, clientCtx []byte, pushOptions ...string) (bool, []gitlab.PostReceiveMessage, error) {
				return false, []gitlab.PostReceiveMessage{
					{
						Message: "access denied",
						Type:    "alert",
					},
				}, nil
			},
			expectedStdout: "\n========================================================================\n\n                             access denied\n\n========================================================================\n\n",
			expectedErr:    errors.New(""),
		},
		{
			desc:    "access check returns error",
			env:     standardEnv,
			changes: "changes\n",
			postreceive: func(t *testing.T, ctx context.Context, glRepo, glID, changes string, clientCtx []byte, pushOptions ...string) (bool, []gitlab.PostReceiveMessage, error) {
				return false, nil, errors.New("failure")
			},
			expectedErr: fmt.Errorf("GitLab: %w", fmt.Errorf("failure")),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			gitlabAPI := postreceiveAPIMock{
				postreceive: func(ctx context.Context, glRepo, glID, changes string, clientCtx []byte, pushOptions ...string) (bool, []gitlab.PostReceiveMessage, error) {
					require.Equal(t, []byte("foobar"), clientCtx)
					return tc.postreceive(t, ctx, glRepo, glID, changes, clientCtx, pushOptions...)
				},
			}

			hookManager := NewManager(
				cfg,
				config.NewLocator(cfg),
				testhelper.SharedLogger(t),
				gittest.NewCommandFactory(t, cfg),
				nil,
				transaction.NewManager(cfg, testhelper.SharedLogger(t), backchannel.NewRegistry()),
				&gitlabAPI,
				NewPostReceiveRegistry(),
			)

			gittest.WriteCustomHook(t, repoPath, "post-receive", []byte("#!/bin/sh\necho hook called\n"))

			var stdout, stderr bytes.Buffer
			err = hookManager.PostReceiveHook(ctx, repo, tc.pushOptions, tc.env, strings.NewReader(tc.changes), &stdout, &stderr)

			if tc.expectedErr != nil {
				require.Equal(t, tc.expectedErr, err)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, tc.expectedStdout, stdout.String())
			require.Equal(t, tc.expectedStderr, stderr.String())
		})
	}
}

func TestPostReceive_quarantine(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	quarantine, cleanup, err := quarantine.New(ctx, repoProto, testhelper.SharedLogger(t), config.NewLocator(cfg))
	require.NoError(t, err)
	t.Cleanup(cleanup)

	quarantinedRepo := localrepo.NewTestRepo(t, cfg, quarantine.QuarantinedRepo())
	blobID, err := quarantinedRepo.WriteBlob(ctx, strings.NewReader("allyourbasearebelongtous"), localrepo.WriteBlobConfig{})
	require.NoError(t, err)

	hookManager := NewManager(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t), gittest.NewCommandFactory(t, cfg), nil, nil, gitlab.NewMockClient(
		t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive,
	), NewPostReceiveRegistry())

	gittest.WriteCustomHook(t, repoPath, "post-receive", []byte(fmt.Sprintf(
		`#!/bin/sh
		git cat-file -p %q || true
	`, blobID.String())))

	for repo, isQuarantined := range map[*gitalypb.Repository]bool{
		quarantine.QuarantinedRepo(): true,
		repoProto:                    false,
	} {
		t.Run(fmt.Sprintf("quarantined: %v", isQuarantined), func(t *testing.T) {
			env, err := gitcmd.NewHooksPayload(
				ctx,
				cfg,
				repo,
				gittest.DefaultObjectHash,
				nil,
				&gitcmd.UserDetails{
					UserID:   "1234",
					Username: "user",
					Protocol: "web",
				},
				gitcmd.PreReceiveHook,
				featureflag.FromContext(ctx),
			).Env()
			require.NoError(t, err)

			stdin := strings.NewReader(fmt.Sprintf("%s %s refs/heads/master",
				gittest.DefaultObjectHash.ZeroOID, gittest.DefaultObjectHash.ZeroOID))

			var stdout, stderr bytes.Buffer
			require.NoError(t, hookManager.PostReceiveHook(ctx, repo, nil,
				[]string{env}, stdin, &stdout, &stderr))

			if isQuarantined {
				require.Equal(t, "allyourbasearebelongtous", stdout.String())
				require.Empty(t, stderr.String())
			} else {
				require.Empty(t, stdout.String())
				require.Contains(t, stderr.String(), "Not a valid object name")
			}
		})
	}
}

func TestPostReceive_praefectSignalCoordination(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	gitlabAPI := postreceiveAPIMock{
		postreceive: func(ctx context.Context, glRepo, glID, changes string, clientCtx []byte, pushOptions ...string) (bool, []gitlab.PostReceiveMessage, error) {
			return true, nil, nil
		},
	}

	// changesLine is the post-receive stdin: a single reference update.
	changesLine := fmt.Sprintf("%s %s refs/heads/master\n",
		gittest.DefaultObjectHash.ZeroOID, gittest.DefaultObjectHash.EmptyTreeOID)

	// makePayload builds a primary-node post-receive payload carrying a Praefect
	// transaction (Transaction != nil && BackchannelID != 0), which together with
	// the feature flag in signalCtx satisfies the waitForPraefectSignal gate.
	makePayload := func(t *testing.T, signalCtx context.Context, txID uint64) string {
		t.Helper()
		payload, err := gitcmd.NewHooksPayload(
			signalCtx,
			cfg,
			repo,
			gittest.DefaultObjectHash,
			&txinfo.Transaction{
				ID:            txID,
				Node:          "primary",
				Primary:       true,
				BackchannelID: 1, // Non-zero to indicate running through Praefect.
			},
			&gitcmd.UserDetails{
				UserID:   "1234",
				Username: "user",
				Protocol: "web",
			},
			gitcmd.PostReceiveHook,
			featureflag.FromContext(signalCtx),
		).Env()
		require.NoError(t, err)
		return payload
	}

	newHookManager := func(t *testing.T, registry *PostReceiveRegistry, gitlabAPI gitlab.Client) *GitLabHookManager {
		return NewManager(
			cfg,
			config.NewLocator(cfg),
			testhelper.SharedLogger(t),
			gittest.NewCommandFactory(t, cfg),
			nil,
			transaction.NewTrackingManager(),
			gitlabAPI,
			registry,
		)
	}

	t.Run("hook blocks until the success signal, then calls Rails", func(t *testing.T) {
		signalCtx := featureflag.ContextWithFeatureFlag(ctx, featureflag.PraefectPostReceiveSignal, true)
		registry := NewPostReceiveRegistry()

		railsCalled := make(chan struct{})
		railsAPI := postreceiveAPIMock{
			postreceive: func(context.Context, string, string, string, []byte, ...string) (bool, []gitlab.PostReceiveMessage, error) {
				close(railsCalled)
				return true, nil, nil
			},
		}

		const txID uint64 = 12345
		hookManager := newHookManager(t, registry, &railsAPI)
		payload := makePayload(t, signalCtx, txID)

		hookErr := make(chan error, 1)
		go func() {
			var stdout, stderr bytes.Buffer
			hookErr <- hookManager.PostReceiveHook(signalCtx, repo, nil,
				[]string{payload}, strings.NewReader(changesLine), &stdout, &stderr)
		}()

		// The hook must block in waitForPraefectSignal — which runs before the
		// Rails call — until Praefect signals.
		select {
		case <-railsCalled:
			t.Fatal("hook called Rails before the Praefect signal")
		case err := <-hookErr:
			t.Fatalf("hook returned before the Praefect signal: %v", err)
		case <-time.After(100 * time.Millisecond):
		}

		registry.Signal(txID, nil)

		select {
		case <-railsCalled:
		case <-time.After(5 * time.Second):
			t.Fatal("hook did not call Rails after the success signal")
		}
		require.NoError(t, <-hookErr)
	})

	t.Run("hook fails the push when the signal carries failure", func(t *testing.T) {
		signalCtx := featureflag.ContextWithFeatureFlag(ctx, featureflag.PraefectPostReceiveSignal, true)
		registry := NewPostReceiveRegistry()

		var railsCalled atomic.Bool
		railsAPI := postreceiveAPIMock{
			postreceive: func(context.Context, string, string, string, []byte, ...string) (bool, []gitlab.PostReceiveMessage, error) {
				railsCalled.Store(true)
				return true, nil, nil
			},
		}

		const txID uint64 = 23456
		hookManager := newHookManager(t, registry, &railsAPI)
		payload := makePayload(t, signalCtx, txID)

		hookErr := make(chan error, 1)
		go func() {
			var stdout, stderr bytes.Buffer
			hookErr <- hookManager.PostReceiveHook(signalCtx, repo, nil,
				[]string{payload}, strings.NewReader(changesLine), &stdout, &stderr)
		}()

		registry.Signal(txID, errors.New("Praefect post-transaction work failed"))

		select {
		case err := <-hookErr:
			require.Error(t, err)
			require.Contains(t, err.Error(), "waiting for Praefect signal")
		case <-time.After(5 * time.Second):
			t.Fatal("hook did not return after the failure signal")
		}
		require.False(t, railsCalled.Load(), "hook must not call Rails when the signal carries failure")
	})

	t.Run("hook fails the push when it times out waiting for the signal", func(t *testing.T) {
		signalCtx := featureflag.ContextWithFeatureFlag(ctx, featureflag.PraefectPostReceiveSignal, true)
		registry := NewPostReceiveRegistry()

		var railsCalled atomic.Bool
		railsAPI := postreceiveAPIMock{
			postreceive: func(context.Context, string, string, string, []byte, ...string) (bool, []gitlab.PostReceiveMessage, error) {
				railsCalled.Store(true)
				return true, nil, nil
			},
		}

		const txID uint64 = 34567
		hookManager := newHookManager(t, registry, &railsAPI)
		// Replace the timeout ticker with a manual one so the test fires the timeout
		// deterministically instead of waiting postReceiveWaitTimeout (60s).
		ticker := helper.NewManualTicker()
		hookManager.praefectSignalTicker = func(d time.Duration) helper.Ticker {
			return ticker
		}

		payload := makePayload(t, signalCtx, txID)

		hookErr := make(chan error, 1)
		go func() {
			var stdout, stderr bytes.Buffer
			hookErr <- hookManager.PostReceiveHook(signalCtx, repo, nil,
				[]string{payload}, strings.NewReader(changesLine), &stdout, &stderr)
		}()

		// Fire the timeout without ever signaling. The manual ticker's channel is
		// buffered, so this is safe even if it lands before the hook's select.
		ticker.Tick()

		select {
		case err := <-hookErr:
			require.Error(t, err)
			require.Contains(t, err.Error(), "waiting for Praefect signal")
		case <-time.After(5 * time.Second):
			t.Fatal("hook did not return after the timeout")
		}
		require.False(t, railsCalled.Load(), "hook must not call Rails when it times out")
	})

	t.Run("hook does not wait without Praefect transaction", func(t *testing.T) {
		postReceiveRegistry := NewPostReceiveRegistry()

		hookManager := NewManager(
			cfg,
			config.NewLocator(cfg),
			testhelper.SharedLogger(t),
			gittest.NewCommandFactory(t, cfg),
			nil,
			transaction.NewTrackingManager(),
			&gitlabAPI,
			postReceiveRegistry,
		)

		payload, err := gitcmd.NewHooksPayload(
			ctx,
			cfg,
			repo,
			gittest.DefaultObjectHash,
			nil,
			&gitcmd.UserDetails{
				UserID:   "1234",
				Username: "user",
				Protocol: "web",
			},
			gitcmd.PostReceiveHook,
			nil,
		).Env()
		require.NoError(t, err)

		start := time.Now()

		stdin := strings.NewReader(fmt.Sprintf("%s %s refs/heads/master\n",
			gittest.DefaultObjectHash.ZeroOID, gittest.DefaultObjectHash.EmptyTreeOID))

		var stdout, stderr bytes.Buffer
		err = hookManager.PostReceiveHook(ctx, repo, nil, []string{payload}, stdin, &stdout, &stderr)
		elapsed := time.Since(start)

		require.NoError(t, err)
		require.Less(t, elapsed, time.Second, "hook should complete quickly without Praefect transaction")
	})
}

// TestNewManager_postReceiveWaitTimeout verifies that the post-receive wait timeout
// is taken from Hooks.PostReceiveWaitTimeout and falls back to the 60s default when
// it is unset.
func TestNewManager_postReceiveWaitTimeout(t *testing.T) {
	t.Parallel()

	newManager := func(t *testing.T, cfg config.Cfg) *GitLabHookManager {
		return NewManager(
			cfg,
			config.NewLocator(cfg),
			testhelper.SharedLogger(t),
			gittest.NewCommandFactory(t, cfg),
			nil,
			transaction.NewTrackingManager(),
			gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive),
			NewPostReceiveRegistry(),
		)
	}

	t.Run("defaults to 60s", func(t *testing.T) {
		require.Equal(t, 60*time.Second, newManager(t, testcfg.Build(t)).postReceiveWaitTimeout)
	})

	t.Run("honors configured value", func(t *testing.T) {
		cfg := testcfg.Build(t)
		cfg.Hooks.PostReceiveWaitTimeout = duration.Duration(5 * time.Second)
		require.Equal(t, 5*time.Second, newManager(t, cfg).postReceiveWaitTimeout)
	})
}
