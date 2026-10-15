package gitcmd_test

import (
	"context"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/graft/identity"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func newGraftTestPayload(t *testing.T, ctx context.Context, userDetails *gitcmd.UserDetails) gitcmd.HooksPayload {
	t.Helper()

	return gitcmd.NewHooksPayload(
		ctx,
		config.Cfg{},
		&gitalypb.Repository{StorageName: "default", RelativePath: "@hashed/ab/cd/repo.git"},
		git.ObjectHashSHA1,
		nil,
		userDetails,
		gitcmd.ReceivePackHooks,
		nil,
	)
}

func changelogMetaFromPayload(t *testing.T, payload gitcmd.HooksPayload) gitcmd.ChangelogMeta {
	t.Helper()

	env, err := payload.Env()
	require.NoError(t, err)

	meta, err := gitcmd.ChangelogMetaFromEnv([]string{env})
	require.NoError(t, err)

	return meta
}

func TestNewHooksPayload_changelogMeta(t *testing.T) {
	t.Parallel()

	t.Run("operation, actor and rewritten come from the context", func(t *testing.T) {
		ctx := changelog.WithRewritten(changelog.WithOperation(context.Background(), "land"))
		ctx = identity.NewContext(ctx, identity.Identity{Name: "agent-a", Source: identity.SourceAgent})

		meta := changelogMetaFromPayload(t, newGraftTestPayload(t, ctx, &gitcmd.UserDetails{UserID: "user-1"}))

		require.Equal(t, gitcmd.ChangelogMeta{Operation: "land", Actor: "agent-a", Agent: true, Rewritten: true}, meta,
			"the authenticated identity wins over the user ID")
	})

	t.Run("a shared-token identity is not marked as an agent", func(t *testing.T) {
		ctx := identity.NewContext(context.Background(), identity.Identity{Name: "shared", Source: identity.SourceSharedToken})

		meta := changelogMetaFromPayload(t, newGraftTestPayload(t, ctx, &gitcmd.UserDetails{UserID: "user-1"}))

		require.Equal(t, gitcmd.ChangelogMeta{Actor: "shared"}, meta)
	})

	t.Run("only explicit values are carried without an identity or gRPC method", func(t *testing.T) {
		// The recording hook falls back to the user ID and to "unknown".
		meta := changelogMetaFromPayload(t, newGraftTestPayload(t, context.Background(), &gitcmd.UserDetails{UserID: "user-1"}))

		require.Equal(t, gitcmd.ChangelogMeta{}, meta)
	})

	t.Run("metadata does not change the decoded upstream payload", func(t *testing.T) {
		ctx := changelog.WithRewritten(changelog.WithOperation(context.Background(), "ff_branch"))
		withMeta := newGraftTestPayload(t, ctx, &gitcmd.UserDetails{UserID: "user-2"})
		withoutMeta := newGraftTestPayload(t, context.Background(), &gitcmd.UserDetails{UserID: "user-2"})

		decode := func(payload gitcmd.HooksPayload) gitcmd.HooksPayload {
			env, err := payload.Env()
			require.NoError(t, err)

			decoded, err := gitcmd.HooksPayloadFromEnv([]string{env})
			require.NoError(t, err)
			return decoded
		}

		// Decoding must not restore the changelog metadata, so the decoded
		// payloads of both are equal and compare as upstream payloads do.
		require.Equal(t, decode(withoutMeta), decode(withMeta), "decoding must not restore the changelog metadata")

		require.Equal(t, "ff_branch", changelogMetaFromPayload(t, withMeta).Operation)
	})
}
