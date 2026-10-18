package operations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/quarantine"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// looseObjects returns the loose objects of objectDir, as fan-out directory plus file name.
func looseObjects(t *testing.T, objectDir string) []string {
	t.Helper()

	var objects []string
	entries, err := os.ReadDir(objectDir)
	require.NoError(t, err)
	for _, entry := range entries {
		if !entry.IsDir() || !isFanoutName(entry.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(objectDir, entry.Name()))
		require.NoError(t, err)
		for _, file := range files {
			objects = append(objects, entry.Name()+file.Name())
		}
	}
	slices.Sort(objects)
	return objects
}

// packIndexes returns the pack index files of objectDir.
func packIndexes(t *testing.T, objectDir string) []string {
	t.Helper()

	indexes, err := filepath.Glob(filepath.Join(objectDir, "pack", "*.idx"))
	require.NoError(t, err)
	return indexes
}

// packedObjects returns the sorted object IDs of the pack index idx.
func packedObjects(t *testing.T, cfg config.Cfg, repoPath, idx string) []string {
	t.Helper()

	var objects []string
	for _, line := range strings.Split(strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "verify-pack", "-v", idx))), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && (fields[1] == "commit" || fields[1] == "tree" || fields[1] == "blob" || fields[1] == "tag") {
			objects = append(objects, fields[0])
		}
	}
	slices.Sort(objects)
	return objects
}

// newQuarantine returns a quarantine of the repository, a local repository on it and its object
// directory.
func newQuarantine(t *testing.T, ctx context.Context, cfg config.Cfg, repoProto *gitalypb.Repository, repoPath string) (*quarantine.Dir, *localrepo.Repo, string) {
	t.Helper()

	locator := config.NewLocator(cfg)
	dir, cleanup, err := quarantine.New(ctx, repoProto, testhelper.SharedLogger(t), locator)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	objectDir := dir.QuarantinedRepo().GetGitObjectDirectory()
	if !filepath.IsAbs(objectDir) {
		objectDir = filepath.Join(repoPath, objectDir)
	}

	return dir, localrepo.NewTestRepo(t, cfg, dir.QuarantinedRepo()), objectDir
}

func TestPackAttemptObjects(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})

	t.Run("packs every loose object and leaves no fan-out directory", func(t *testing.T) {
		t.Parallel()

		dir, repo, objectDir := newQuarantine(t, ctx, cfg, repoProto, repoPath)

		var written []string
		for _, content := range []string{"one\n", "two\n", "three\n"} {
			id, err := repo.WriteBlob(ctx, strings.NewReader(content), localrepo.WriteBlobConfig{})
			require.NoError(t, err)
			written = append(written, id.String())
		}
		slices.Sort(written)
		require.Equal(t, written, looseObjects(t, objectDir))

		packed, err := packAttemptObjects(ctx, repo, objectDir)
		require.NoError(t, err)
		require.Equal(t, len(written), packed)

		require.Empty(t, looseObjects(t, objectDir), "no loose object is left")
		entries, err := os.ReadDir(objectDir)
		require.NoError(t, err)
		for _, entry := range entries {
			require.False(t, isFanoutName(entry.Name()), "fan-out directory %s is left", entry.Name())
		}

		indexes := packIndexes(t, objectDir)
		require.Len(t, indexes, 1)
		require.Equal(t, written, packedObjects(t, cfg, repoPath, indexes[0]))

		for _, id := range written {
			_, err := repo.ReadObject(ctx, git.ObjectID(id))
			require.NoError(t, err, "object %s is readable from the pack", id)
		}

		// The migration moves the pack, and the repository then has the objects.
		require.NoError(t, dir.Migrate(ctx))
		for _, id := range written {
			gittest.Exec(t, cfg, "-C", repoPath, "cat-file", "-e", id)
		}
	})

	t.Run("nothing to pack", func(t *testing.T) {
		t.Parallel()

		_, repo, objectDir := newQuarantine(t, ctx, cfg, repoProto, repoPath)

		packed, err := packAttemptObjects(ctx, repo, objectDir)
		require.NoError(t, err)
		require.Zero(t, packed)
		require.Empty(t, packIndexes(t, objectDir))
	})

	t.Run("a file that is not an object stays", func(t *testing.T) {
		t.Parallel()

		_, repo, objectDir := newQuarantine(t, ctx, cfg, repoProto, repoPath)

		id, err := repo.WriteBlob(ctx, strings.NewReader("object\n"), localrepo.WriteBlobConfig{})
		require.NoError(t, err)
		stray := filepath.Join(objectDir, id.String()[:2], "tmp_obj_stray")
		require.NoError(t, os.WriteFile(stray, []byte("partial"), 0o600))

		packed, err := packAttemptObjects(ctx, repo, objectDir)
		require.NoError(t, err)
		require.Equal(t, 1, packed)
		require.FileExists(t, stray)
		require.NoFileExists(t, filepath.Join(objectDir, id.String()[:2], id.String()[2:]))
	})

	t.Run("a failed pack leaves the loose objects", func(t *testing.T) {
		t.Parallel()

		_, repo, objectDir := newQuarantine(t, ctx, cfg, repoProto, repoPath)

		id, err := repo.WriteBlob(ctx, strings.NewReader("kept\n"), localrepo.WriteBlobConfig{})
		require.NoError(t, err)

		// pack-objects cannot write into a read-only pack directory.
		packDir := filepath.Join(objectDir, "pack")
		require.NoError(t, os.MkdirAll(packDir, 0o700))
		require.NoError(t, os.Chmod(packDir, 0o500))
		t.Cleanup(func() { require.NoError(t, os.Chmod(packDir, 0o700)) })

		_, err = packAttemptObjects(ctx, repo, objectDir)
		require.ErrorContains(t, err, "pack-objects")
		require.Equal(t, []string{id.String()}, looseObjects(t, objectDir))
	})
}

// runLandBatchStepperServer starts an operations service and returns its Server, so that a test can
// drive the batch stepper directly.
func runLandBatchStepperServer(t *testing.T, cfg config.Cfg) (*Server, config.Cfg) {
	t.Helper()

	testcfg.BuildGitalyHooks(t, cfg)

	var srv *Server
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, func(g *grpc.Server, deps *service.Dependencies) {
		s := NewServer(deps)
		if srv == nil {
			srv = s
		}
		gitalypb.RegisterOperationServiceServer(g, s)
		gitalypb.RegisterHookServiceServer(g, hook.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(g, repository.NewServer(deps))
	})
	require.NotNil(t, srv)

	return srv, cfg
}

// stepperFixture is a repository whose main branch is at base, with changes to land onto it.
type stepperFixture struct {
	cfg       config.Cfg
	repoProto *gitalypb.Repository
	repoPath  string
	base      git.ObjectID
	// forward fast-forwards base, diverged1 and diverged2 change other files from base.
	forward, diverged1, diverged2 git.ObjectID
}

func newStepperFixture(t *testing.T, ctx context.Context, cfg config.Cfg) stepperFixture {
	t.Helper()

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	rootEntry := gittest.TreeEntry{Mode: "100644", Path: "root", Content: "root"}
	root := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(rootEntry))
	baseEntries := []gittest.TreeEntry{rootEntry, {Mode: "100644", Path: "base", Content: "base"}}
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithParents(root),
		gittest.WithTreeEntries(baseEntries...))

	// The diverged changes are based on root, the parent of the tip, so they are rebased.
	change := func(parent git.ObjectID, entries []gittest.TreeEntry, path string) git.ObjectID {
		return gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(parent), gittest.WithMessage(path),
			gittest.WithTreeEntries(append(slices.Clone(entries), gittest.TreeEntry{Mode: "100644", Path: path, Content: path})...))
	}

	return stepperFixture{
		cfg: cfg, repoProto: repoProto, repoPath: repoPath, base: base,
		forward:   change(base, baseEntries, "forward"),
		diverged1: change(root, []gittest.TreeEntry{rootEntry}, "one"),
		diverged2: change(root, []gittest.TreeEntry{rootEntry}, "two"),
	}
}

func (f stepperFixture) request(t *testing.T, ctx context.Context, commit git.ObjectID) *landBatchRequest {
	t.Helper()

	in := landRequest(f.repoProto, git.DefaultBranch, commit)
	committer, err := git.SignatureFromRequest(in)
	require.NoError(t, err)

	return &landBatchRequest{ctx: ctx, in: in, committer: committer, actor: landBatchActor(ctx, in)}
}

// commandCount returns how many commands of ctx have completed.
func commandCount(ctx context.Context) int {
	count, _ := log.CustomFieldsFromContext(ctx).Fields()["command.count"].(int)
	return count
}

// TestLandBatchStepperLandsWithOneProcessPerMember drives one batch attempt: every request is decided
// from one range listing, the long-lived processes of the attempt serve the rest, and the publish
// makes the objects of the attempt durable as one pack before the reference moves.
func TestLandBatchStepperLandsWithOneProcessPerMember(t *testing.T) {
	t.Parallel()

	ctx := log.InitContextCustomFields(featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false))
	cfg := testcfg.Build(t)
	srv, cfg := runLandBatchStepperServer(t, cfg)
	f := newStepperFixture(t, ctx, cfg)

	reference := git.NewReferenceNameFromBranchName(git.DefaultBranch)

	// A first attempt lands every kind of request once, so that what a server does once per git
	// binary (detecting its version) is done before the processes are counted.
	warmUp, cleanupWarmUp, err := srv.newLandBatchStepper(ctx, f.repoProto)
	require.NoError(t, err)
	warmTip := f.base
	for _, commit := range []git.ObjectID{f.diverged1, f.diverged2, f.base, f.forward} {
		step, err := warmUp.Land(ctx, warmTip, f.request(t, ctx, commit))
		require.NoError(t, err)
		if commit != f.forward {
			warmTip = step.tip
		}
	}
	cleanupWarmUp()

	st, cleanup, err := srv.newLandBatchStepper(ctx, f.repoProto)
	require.NoError(t, err)
	defer cleanup()

	tip, err := st.Tip(ctx, reference)
	require.NoError(t, err)
	require.Equal(t, f.base, tip)

	// The first rebase starts the long-lived processes of the attempt.
	first, err := st.Land(ctx, tip, f.request(t, ctx, f.diverged1))
	require.NoError(t, err)
	require.True(t, first.rewritten)

	landWithProcesses := func(tip, commit git.ObjectID) (landStep, int) {
		t.Helper()

		before := commandCount(ctx)
		step, err := st.Land(ctx, tip, f.request(t, ctx, commit))
		require.NoError(t, err)
		return step, commandCount(ctx) - before
	}

	// A rebase onto the running tip, a request already landed, and a fast-forward of the starting
	// tip each run one short git command: the range listing (git rev-list). The command factory
	// starts a second process for it: for a command outside its attr.tree list it runs git
	// rev-parse --show-object-format first (gitcmd AttrTreeConfig).
	const rangeListingProcesses = 2

	second, processes := landWithProcesses(first.tip, f.diverged2)
	require.True(t, second.rewritten)
	require.Equal(t, rangeListingProcesses, processes, "rebase")

	landed, processes := landWithProcesses(second.tip, f.base)
	require.True(t, landed.alreadyLanded)
	require.Equal(t, rangeListingProcesses, processes, "already landed")

	forward, processes := landWithProcesses(tip, f.forward)
	require.Equal(t, f.forward, forward.tip)
	require.Equal(t, rangeListingProcesses, processes, "fast-forward")

	// The publish packs the objects of the attempt and the migration moves the pack.
	objectsBefore := packIndexes(t, filepath.Join(f.repoPath, "objects"))
	kind, err := st.Update(ctx, reference, []changelog.Member{memberOf(f.request(t, ctx, f.diverged1), first), memberOf(f.request(t, ctx, f.diverged2), second)}, true, tip, second.tip)
	require.NoError(t, err)
	require.Equal(t, landUpdated, kind)
	requireBranchAt(t, cfg, f.repoPath, git.DefaultBranch, second.tip)

	newPacks := packIndexes(t, filepath.Join(f.repoPath, "objects"))
	require.Len(t, newPacks, len(objectsBefore)+1, "the attempt publishes one pack")
	for _, commit := range []git.ObjectID{first.tip, second.tip} {
		require.NoFileExists(t, filepath.Join(f.repoPath, "objects", commit.String()[:2], commit.String()[2:]),
			"the rebased commit %s is not a loose object of the repository", commit)
	}
	gittest.Exec(t, cfg, "-C", f.repoPath, "fsck", "--connectivity-only", "--no-dangling")
}

// TestLandBatchStepperPackFailureFailsTheUpdate is not parallel: it replaces packBatchObjects.
func TestLandBatchStepperPackFailureFailsTheUpdate(t *testing.T) {
	ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	srv, cfg := runLandBatchStepperServer(t, cfg)
	f := newStepperFixture(t, ctx, cfg)

	errPack := errors.New("injected pack failure")
	original := packBatchObjects
	packBatchObjects = func(context.Context, *localrepo.Repo, string) (int, error) { return 0, errPack }
	t.Cleanup(func() { packBatchObjects = original })

	st, cleanup, err := srv.newLandBatchStepper(ctx, f.repoProto)
	require.NoError(t, err)
	defer cleanup()

	reference := git.NewReferenceNameFromBranchName(git.DefaultBranch)
	req := f.request(t, ctx, f.diverged1)
	step, err := st.Land(ctx, f.base, req)
	require.NoError(t, err)

	kind, err := st.UpdateSolo(ctx, req, memberOf(req, step), reference, true, f.base, step.tip)
	require.ErrorIs(t, err, errPack)
	testhelper.RequireGrpcCode(t, err, codes.Internal)
	require.Equal(t, landUpdateFailed, kind)
	requireBranchAt(t, cfg, f.repoPath, git.DefaultBranch, f.base)
}

// TestLandBatchStepperFaultsAroundTheMigration stops the publish of an attempt right after the pack
// (a pre-receive hook, which runs before the migration) and right after the migration (an update
// hook, which runs before update-ref). Neither moves the reference, and the repository stays
// connected; the second leaves the migrated pack in the repository, unreferenced.
func TestLandBatchStepperFaultsAroundTheMigration(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		hook         string
		wantMigrated bool
	}{
		{hook: "pre-receive", wantMigrated: false},
		{hook: "update", wantMigrated: true},
	} {
		t.Run(tc.hook, func(t *testing.T) {
			t.Parallel()

			ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
			cfg := testcfg.Build(t)
			srv, cfg := runLandBatchStepperServer(t, cfg)
			f := newStepperFixture(t, ctx, cfg)
			gittest.WriteCustomHook(t, f.repoPath, tc.hook, []byte("#!/bin/sh\necho fault injected >&2\nexit 1\n"))

			st, cleanup, err := srv.newLandBatchStepper(ctx, f.repoProto)
			require.NoError(t, err)
			defer cleanup()

			reference := git.NewReferenceNameFromBranchName(git.DefaultBranch)
			req := f.request(t, ctx, f.diverged1)
			step, err := st.Land(ctx, f.base, req)
			require.NoError(t, err)

			packsBefore := len(packIndexes(t, filepath.Join(f.repoPath, "objects")))
			kind, err := st.Update(ctx, reference, []changelog.Member{memberOf(req, step)}, true, f.base, step.tip)
			require.Error(t, err)
			require.Equal(t, landUpdateRejected, kind)
			requireBranchAt(t, cfg, f.repoPath, git.DefaultBranch, f.base)

			packsAfter := len(packIndexes(t, filepath.Join(f.repoPath, "objects")))
			if tc.wantMigrated {
				require.Equal(t, packsBefore+1, packsAfter, "the migrated pack stays, unreferenced")
				gittest.Exec(t, cfg, "-C", f.repoPath, "cat-file", "-e", step.tip.String())
			} else {
				require.Equal(t, packsBefore, packsAfter, "nothing was migrated")
			}
			gittest.Exec(t, cfg, "-C", f.repoPath, "fsck", "--connectivity-only")
		})
	}
}
