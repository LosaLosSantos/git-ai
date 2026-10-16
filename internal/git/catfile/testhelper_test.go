package catfile

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/helper"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

type repoExecutor struct {
	storage.Repository
	gitCmdFactory gitcmd.CommandFactory
}

func newRepoExecutor(t *testing.T, cfg config.Cfg, repo storage.Repository) gitcmd.RepositoryExecutor {
	return &repoExecutor{
		Repository:    repo,
		gitCmdFactory: gittest.NewCommandFactory(t, cfg),
	}
}

func (e *repoExecutor) Exec(ctx context.Context, cmd gitcmd.Command, opts ...gitcmd.CmdOpt) (*command.Command, error) {
	return e.gitCmdFactory.New(ctx, e.Repository, cmd, opts...)
}

func (e *repoExecutor) ExecAndWait(ctx context.Context, cmd gitcmd.Command, opts ...gitcmd.CmdOpt) error {
	command, err := e.Exec(ctx, cmd, opts...)
	if err != nil {
		return err
	}
	return command.Wait()
}

func (e *repoExecutor) GitVersion(ctx context.Context) (string, error) {
	return e.gitCmdFactory.GitVersion(ctx)
}

func (e *repoExecutor) ObjectHash(ctx context.Context) (git.ObjectHash, error) {
	return gittest.DefaultObjectHash, nil
}

func (e *repoExecutor) ReferenceBackend(ctx context.Context) (git.ReferenceBackend, error) {
	return gittest.DefaultReferenceBackend, nil
}

func setupObjectReader(t *testing.T, ctx context.Context) (config.Cfg, ObjectContentReader, *gitalypb.Repository, string) {
	t.Helper()

	cfg := testcfg.Build(t)
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repoExecutor := newRepoExecutor(t, cfg, repo)

	cache := newCache(1*time.Hour, 1000, helper.NewTimerTicker(defaultEvictionInterval))
	t.Cleanup(cache.Stop)

	objectReader, cancel, err := cache.ObjectReader(ctx, repoExecutor)
	require.NoError(t, err)
	t.Cleanup(cancel)

	return cfg, objectReader, repo, repoPath
}

type staticObject struct {
	reader     io.Reader
	objectType string
	objectSize int64
	objectID   git.ObjectID
}

func newStaticObject(contents string, objectType string, oid git.ObjectID) *staticObject {
	return &staticObject{
		reader:     strings.NewReader(contents),
		objectType: objectType,
		objectSize: int64(len(contents)),
		objectID:   oid,
	}
}

func (o *staticObject) ObjectID() git.ObjectID {
	return o.objectID
}

func (o *staticObject) ObjectSize() int64 {
	return o.objectSize
}

func (o *staticObject) ObjectType() string {
	return o.objectType
}

func (o *staticObject) Read(p []byte) (int, error) {
	return o.reader.Read(p)
}

func (o *staticObject) WriteTo(w io.Writer) (int64, error) {
	return io.Copy(w, o.reader)
}
