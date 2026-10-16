// Package storagecore implements the StorageCoreService: import, state and
// verification of repositories in the Graft storage core. Owned by WP57
// (doc/graft/PLAN6.md, "Import, state and verify").
package storagecore

import (
	"bytes"
	"context"
	"sort"
	"strings"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedStorageCoreServiceServer
	deps *service.Dependencies
}

// NewServer creates the StorageCoreService server.
func NewServer(deps *service.Dependencies) gitalypb.StorageCoreServiceServer {
	return &server{deps: deps}
}

// enabledCore returns the storage core, or FailedPrecondition when it is off.
func (s *server) enabledCore() (*corestore.Core, error) {
	core := s.deps.GetGraftStorage()
	if !core.Enabled() {
		return nil, structerr.NewFailedPrecondition("the storage core is off")
	}
	return core, nil
}

// localRepo returns the local repository of a request.
func (s *server) localRepo(repo *gitalypb.Repository) *localrepo.Repo {
	return s.deps.GetRepositoryFactory().Build(repo)
}

// sortedNames returns the union of the reference names of the maps, in
// ascending order, each name once.
func sortedNames(maps ...map[string]corestore.Target) []string {
	seen := map[string]struct{}{}
	for _, m := range maps {
		for name := range m {
			seen[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// maxConnectivityError bounds the git output returned for a failed
// connectivity check: a badly broken repository can make fsck print a line per
// object.
const maxConnectivityError = 4096

// checkConnectivity runs `git fsck --connectivity-only --no-dangling` on the
// local repository. It returns whether the check passed and, when git reported
// a problem, what it printed (at most maxConnectivityError bytes). An error is
// returned only when the check could not run to completion (for example the
// request was canceled).
func checkConnectivity(ctx context.Context, repo *localrepo.Repo) (bool, string, error) {
	var stdout, stderr bytes.Buffer
	err := repo.ExecAndWait(ctx, gitcmd.Command{
		Name: "fsck",
		Flags: []gitcmd.Option{
			gitcmd.Flag{Name: "--connectivity-only"},
			gitcmd.Flag{Name: "--no-dangling"},
		},
	}, gitcmd.WithStdout(&stdout), gitcmd.WithStderr(&stderr))
	if err == nil {
		return true, "", nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, "", structerr.NewUnavailable("connectivity check: %w", ctxErr)
	}
	if _, exited := command.ExitStatus(err); !exited {
		return false, "", structerr.NewInternal("connectivity check: %w", err)
	}

	msg := strings.TrimSpace(strings.TrimSpace(stderr.String()) + "\n" + strings.TrimSpace(stdout.String()))
	if msg == "" {
		msg = err.Error()
	}
	if len(msg) > maxConnectivityError {
		msg = strings.ToValidUTF8(msg[:maxConnectivityError], "") + "..."
	}
	return false, msg, nil
}
