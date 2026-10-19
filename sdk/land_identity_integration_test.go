package sdk_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/sdk"
)

// lostLandResponses loses the response of the first UserLand call after the server answered it, the
// way a connection reset or a node that stops after the update does: the client gets Unavailable
// for a land that happened. afterLand runs once the first call landed, before the client sees the
// loss.
type lostLandResponses struct {
	mu        sync.Mutex
	calls     int
	landed    string
	afterLand func(landed string)
}

func (l *lostLandResponses) intercept(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if err := invoker(ctx, method, req, reply, cc, opts...); err != nil || method != "/gitaly.OperationService/UserLand" {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.calls++
	if l.calls > 1 {
		return nil
	}

	l.landed = reply.(interface{ GetLandedCommitId() string }).GetLandedCommitId()
	if l.afterLand != nil {
		l.afterLand(l.landed)
	}
	return status.Error(codes.Unavailable, "connection reset after the land")
}

// TestIntegration_LandRetryAfterALostResponse lands a change by rebase through the SDK, whose first
// answer is lost after the server landed it: the SDK retries the Unavailable, and the server answers
// from its record of the first land (doc/graft/PLAN11.md). The change is on the branch once.
func TestIntegration_LandRetryAfterALostResponse(t *testing.T) {
	t.Parallel()

	user := sdk.User{ID: "user-1", Name: "Ada", Email: "ada@example.com"}
	retry := sdk.UnavailableRetry{Base: time.Millisecond, Cap: 2 * time.Millisecond}

	for _, tc := range []struct {
		desc string
		// revert reverts the first land before the retry reaches the server.
		revert bool
	}{
		{desc: "the retry answers with the rebased copy"},
		{desc: "a revert before the retry stays", revert: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			lost := &lostLandResponses{}
			env := startIntegration(t, nil, sdk.WithGRPCOptions(grpc.WithUnaryInterceptor(lost.intercept)))
			base := env.writeCommit(t, "main", map[string]string{"base.txt": "base"})
			tip := env.writeCommit(t, "main", map[string]string{"base.txt": "base", "x": "x"}, base)
			change := env.writeCommit(t, "", map[string]string{"base.txt": "base", "y": "y"}, base)

			var reverted string
			if tc.revert {
				lost.afterLand = func(landed string) {
					reverted = gittest.WriteCommit(t, env.cfg, env.repoPath,
						gittest.WithTree(env.writeTree(t, map[string]string{"base.txt": "base", "x": "x"})),
						gittest.WithParents(git.ObjectID(landed)),
						gittest.WithMessage(fmt.Sprintf("Revert \"y\"\n\nThis reverts commit %s.\n", landed)),
					).String()
					env.git(t, "update-ref", "refs/heads/main", reverted, landed)
				}
			}

			result, err := env.repo.Land(env.ctx, sdk.LandOptions{
				Branch:           "main",
				Commit:           change.String(),
				User:             user,
				Strategy:         sdk.LandRebase,
				RetryUnavailable: retry,
			})
			require.NoError(t, err)
			require.Equal(t, 1, result.UnavailableRetries)
			require.Equal(t, "connection reset after the land", result.LastUnavailable)

			require.NotEqual(t, change.String(), lost.landed, "the first land rebased the change")
			require.True(t, result.AlreadyLanded)
			require.False(t, result.Rewritten)
			require.Equal(t, lost.landed, result.LandedCommitID)
			require.NotZero(t, result.LandedSequence)
			require.Equal(t, reverted, result.RevertedByCommitID)

			detail := fmt.Sprintf("landed as %s at changelog %d", lost.landed, result.LandedSequence)
			head := lost.landed
			if tc.revert {
				detail += fmt.Sprintf("; it was reverted later by %s", reverted)
				head = reverted
			}
			require.Equal(t, detail, result.AlreadyLandedDetail)
			require.Equal(t, head, env.mustResolve(t, "main"))
			require.Equal(t, tip.String(), env.git(t, "rev-parse", lost.landed+"^"), "the change landed once, on the tip")
		})
	}
}
