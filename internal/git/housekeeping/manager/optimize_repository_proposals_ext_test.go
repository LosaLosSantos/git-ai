package manager_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	housekeepingmgr "github.com/feder-cr/git-ai/internal/git/housekeeping/manager"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	gitalycfgprom "github.com/feder-cr/git-ai/internal/gitaly/config/prometheus"
	"github.com/feder-cr/git-ai/internal/graft/proposals"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// TestOptimizeRepository_prunesGraftProposals: the housekeeping pass expires the proposals of the
// land on every branch, those of a branch nothing lands on any more included, and keeps the
// fresh ones; without the option it leaves them.
func TestOptimizeRepository_prunesGraftProposals(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	now := time.Now()
	zero := gittest.DefaultObjectHash.ZeroOID
	old := git.ReferenceName(fmt.Sprintf("%s%040x", proposals.BranchDir("idle"), 1))
	fresh := git.ReferenceName(fmt.Sprintf("%s%040x", proposals.BranchDir("main"), 2))
	require.NoError(t, proposals.Update(ctx, repo, []proposals.Op{
		{Name: old, NewOID: gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("old"), gittest.WithCommitterDate(now.Add(-30*24*time.Hour))), OldOID: zero},
		{Name: fresh, NewOID: gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("fresh"), gittest.WithCommitterDate(now.Add(-time.Hour))), OldOID: zero},
	}))
	refs := func() []string {
		return strings.Fields(text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref", "--format=%(refname)", proposals.RefPrefix)))
	}

	require.NoError(t, housekeepingmgr.New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil).OptimizeRepository(ctx, repo))
	require.ElementsMatch(t, []string{old.String(), fresh.String()}, refs(), "without the option nothing is pruned")

	manager := housekeepingmgr.New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil, housekeepingmgr.WithGraftProposalTTL(7*24*time.Hour))
	require.NoError(t, manager.OptimizeRepository(ctx, repo))
	require.Equal(t, []string{fresh.String()}, refs(), "the idle branch's old proposal expired")
}
