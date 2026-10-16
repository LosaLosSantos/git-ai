package hook

import (
	"context"
	"io"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	gitalyhook "github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/limiter"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/streamcache"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedHookServiceServer
	logger              log.Logger
	manager             gitalyhook.Manager
	locator             storage.Locator
	gitCmdFactory       gitcmd.CommandFactory
	packObjectsCache    streamcache.Cache
	packObjectsLimiter  limiter.Limiter
	postReceiveRegistry *gitalyhook.PostReceiveRegistry
	// shutdownCtx is cancelled once the graceful shutdown period has elapsed (see
	// serve.go, which cancels it after b.Wait returns). It is used to terminate
	// git-pack-objects processes that intentionally ignore the per-request context,
	// so a slow or stalled transfer cannot keep the process alive past the graceful
	// shutdown period. It is never nil: GetShutdownContext returns a non-cancellable
	// context.Background() when no shutdown context was wired up.
	shutdownCtx      context.Context
	runPackObjectsFn func(
		context.Context,
		gitcmd.CommandFactory,
		io.Writer,
		*gitalypb.PackObjectsHookWithSidechannelRequest,
		*packObjectsArgs,
		io.Reader,
		string,
	) error
}

// NewServer creates a new instance of a gRPC namespace server
func NewServer(deps *service.Dependencies) gitalypb.HookServiceServer {
	srv := &server{
		logger:              deps.GetLogger(),
		manager:             deps.GetHookManager(),
		locator:             deps.GetLocator(),
		gitCmdFactory:       deps.GetGitCmdFactory(),
		packObjectsCache:    deps.GetPackObjectsCache(),
		packObjectsLimiter:  deps.GetPackObjectsLimiter(),
		postReceiveRegistry: deps.GetPostReceiveRegistry(),
		shutdownCtx:         deps.GetShutdownContext(),
		runPackObjectsFn:    runPackObjects,
	}

	return srv
}
