package gittest

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

// setup sets up a test configuration and repository. Ideally we'd use our central test helpers to
// do this, but because of an import cycle we can't.
func setup(tb testing.TB) (config.Cfg, *gitalypb.Repository, string) {
	tb.Helper()

	ctx := testhelper.Context(tb)
	cfg := testcfg.Build(tb)

	repo, repoPath := CreateRepository(tb, ctx, cfg, CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	return cfg, repo, repoPath
}
