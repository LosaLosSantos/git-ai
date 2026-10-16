package gitops_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// newReftableFixture creates an empty repository with the reftable reference
// backend through gitops.InitBare. It is created inside the test storage so
// that it can be used as a localrepo.
func newReftableFixture(t *testing.T, format string) fixture {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	storagePath := cfg.Storages[0].Path

	parent, err := os.MkdirTemp(storagePath, "reftable-")
	require.NoError(t, err)
	path := filepath.Join(parent, "repo.git")
	require.NoError(t, gitops.InitBare(ctx, gittest.NewCommandFactory(t, cfg), path, format))

	relativePath, err := filepath.Rel(storagePath, path)
	require.NoError(t, err)
	protoRepo := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: relativePath}

	hash, err := git.ObjectHashByFormat(format)
	require.NoError(t, err)

	f := fixture{
		ctx:  ctx,
		cfg:  cfg,
		repo: localrepo.NewTestRepo(t, cfg, protoRepo),
		path: path,
		hash: hash,
	}
	require.Equal(t, "reftable", f.git(t, "rev-parse", "--show-ref-format"))

	return f
}

// newFixtureWithBackend returns a fresh repository with the reftable or the
// files reference backend.
func newFixtureWithBackend(t *testing.T, format string, reftable bool) fixture {
	t.Helper()

	if reftable {
		return newReftableFixture(t, format)
	}
	return newFixture(t, format)
}

// backendName is the reference backend name used in subtest names.
func backendName(reftable bool) string {
	if reftable {
		return "reftable"
	}
	return "files"
}

func TestApplyRefsHeadWithItsReferent(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		for _, reftable := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s %s", format, backendName(reftable)), func(t *testing.T) {
				t.Parallel()

				// Each case sets the references and HEAD of a fresh repository, so
				// ReadRefs must return exactly the map.
				for _, tc := range []struct {
					desc string
					refs func(c1, c2 git.ObjectID) map[string]corestore.Target
				}{
					{
						desc: "HEAD and the branch it points to",
						refs: func(_, c2 git.ObjectID) map[string]corestore.Target {
							return map[string]corestore.Target{
								"refs/heads/main": corestore.Target(c2.String()),
								"HEAD":            corestore.SymbolicTarget("refs/heads/main"),
							}
						},
					},
					{
						desc: "HEAD and another branch",
						refs: func(c1, c2 git.ObjectID) map[string]corestore.Target {
							return map[string]corestore.Target{
								"refs/heads/main":    corestore.Target(c2.String()),
								"refs/heads/feature": corestore.Target(c1.String()),
								"HEAD":               corestore.SymbolicTarget("refs/heads/feature"),
							}
						},
					},
					{
						desc: "detached HEAD at the tip of its branch",
						refs: func(_, c2 git.ObjectID) map[string]corestore.Target {
							return map[string]corestore.Target{
								"refs/heads/main": corestore.Target(c2.String()),
								"HEAD":            corestore.Target(c2.String()),
							}
						},
					},
				} {
					t.Run(tc.desc, func(t *testing.T) {
						f := newFixtureWithBackend(t, format, reftable)
						c1 := f.commit(t, nil, "one")
						c2 := f.commit(t, []git.ObjectID{c1}, "two")

						refs := tc.refs(c1, c2)
						require.NoError(t, gitops.ApplyRefs(f.ctx, f.repo, refs))

						current, err := gitops.ReadRefs(f.ctx, f.repo)
						require.NoError(t, err)
						require.Equal(t, refs, current)
					})
				}
			})
		}
	}
}

// TestApplyRefsHydration covers the hydration of a repository: its full pack is
// indexed into an empty repository and every reference of the source is applied.
func TestApplyRefsHydration(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		for _, sourceReftable := range []bool{false, true} {
			for _, targetReftable := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s source %s target %s", format, backendName(sourceReftable), backendName(targetReftable)), func(t *testing.T) {
					t.Parallel()

					for _, head := range []string{"branch", "detached"} {
						t.Run(head, func(t *testing.T) {
							source := newFixtureWithBackend(t, format, sourceReftable)
							c1 := source.commit(t, nil, "one")
							c2 := source.commit(t, []git.ObjectID{c1}, "two")
							gittest.WriteRef(t, source.cfg, source.path, "refs/heads/main", c2)
							gittest.WriteRef(t, source.cfg, source.path, "refs/heads/old", c1)
							source.git(t, "tag", "v1", c1.String())
							source.git(t, "symbolic-ref", "refs/heads/alias", "refs/heads/main")
							if head == "branch" {
								source.git(t, "symbolic-ref", "HEAD", "refs/heads/main")
							} else {
								source.git(t, "update-ref", "--no-deref", "HEAD", c2.String())
							}

							refs, err := gitops.ReadRefs(source.ctx, source.repo)
							require.NoError(t, err)
							require.Contains(t, refs, "HEAD")

							full, err := gitops.BuildFullPack(source.ctx, source.repo, t.TempDir())
							require.NoError(t, err)
							data := readPack(t, full)

							target := newFixtureWithBackend(t, format, targetReftable)
							require.NoError(t, gitops.IndexPack(target.ctx, target.repo, bytes.NewReader(data)))
							require.NoError(t, gitops.ApplyRefs(target.ctx, target.repo, refs))

							current, err := gitops.ReadRefs(target.ctx, target.repo)
							require.NoError(t, err)
							require.Equal(t, refs, current)

							gittest.Exec(t, target.cfg, "-C", target.path, "fsck", "--connectivity-only")
						})
					}
				})
			}
		}
	}
}

func TestApplyRefsLockContentionReftable(t *testing.T) {
	t.Parallel()

	f := newReftableFixture(t, "sha1")
	c1 := f.commit(t, nil, "one")

	// The reftable backend locks its table list with a file in its directory.
	lock := filepath.Join(f.path, "reftable", "tables.list.lock")
	require.NoError(t, os.WriteFile(lock, nil, 0o600))

	err := gitops.ApplyRefs(f.ctx, f.repo, map[string]corestore.Target{
		"refs/heads/locked": corestore.Target(c1.String()),
	})
	require.Error(t, err)
	require.True(t, gitops.IsLockError(err), "error: %v", err)

	values, err := gitops.ReadRefValues(f.ctx, f.repo, []string{"refs/heads/locked"})
	require.NoError(t, err)
	require.Empty(t, values, "the refused transaction changes nothing")

	require.NoError(t, os.Remove(lock))
	require.NoError(t, gitops.ApplyRefs(f.ctx, f.repo, map[string]corestore.Target{
		"refs/heads/locked": corestore.Target(c1.String()),
	}))
	require.Equal(t, c1.String(), f.git(t, "rev-parse", "refs/heads/locked"))
}

func TestReadRefValuesExactNamesAndChunks(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, format)
			c1 := f.commit(t, nil, "one")
			c2 := f.commit(t, []git.ObjectID{c1}, "two")
			gittest.WriteRef(t, f.cfg, f.path, "refs/heads/feature/x", c1)
			gittest.WriteRef(t, f.cfg, f.path, "refs/heads/main", c2)

			// A name that is only a directory of references is absent, and names that
			// cannot be references are skipped instead of being read as options.
			values, err := gitops.ReadRefValues(f.ctx, f.repo, []string{
				"refs/heads/feature",
				"refs/heads/feature/x",
				"-refs/heads/main",
				"refs/heads/with space",
				"main",
				"refs/heads/main",
			})
			require.NoError(t, err)
			require.Equal(t, map[string]corestore.Target{
				"refs/heads/feature/x": corestore.Target(c1.String()),
				"refs/heads/main":      corestore.Target(c2.String()),
			}, values)

			// More names than one run takes: the present one is found in the second run.
			names := make([]string, 0, 1200)
			for i := 0; i < 1200; i++ {
				names = append(names, fmt.Sprintf("refs/heads/missing-%d", i))
			}
			names[1100] = "refs/heads/main"

			values, err = gitops.ReadRefValues(f.ctx, f.repo, names)
			require.NoError(t, err)
			require.Equal(t, map[string]corestore.Target{
				"refs/heads/main": corestore.Target(c2.String()),
			}, values)
		})
	}
}

// TestBuildPackManyTips builds a pack from more tips than one command line can
// hold (about 60000 SHA-1 object IDs exceed ARG_MAX on Linux). The tips are
// blobs created with fast-import, which needs no reference.
func TestBuildPackManyTips(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "sha1")

	const count = 60000
	var input bytes.Buffer
	for i := 1; i <= count; i++ {
		content := fmt.Sprintf("tag %d\n", i)
		fmt.Fprintf(&input, "blob\nmark :%d\ndata %d\n%s\n", i, len(content), content)
	}

	marks := filepath.Join(t.TempDir(), "marks")
	fastImport := gittest.NewCommand(t, f.cfg, "-C", f.path, "fast-import", "--quiet", "--export-marks="+marks)
	fastImport.Stdin = &input
	require.NoError(t, fastImport.Run())

	raw, err := os.ReadFile(marks)
	require.NoError(t, err)

	var tips []git.ObjectID
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		_, oid, ok := strings.Cut(line, " ")
		require.True(t, ok, "marks line %q", line)
		tips = append(tips, git.ObjectID(oid))
	}
	require.Len(t, tips, count)

	tmpDir := t.TempDir()
	pack, err := gitops.BuildPack(f.ctx, f.repo, tips, tmpDir)
	require.NoError(t, err)
	require.Equal(t, count, pack.Objects)
	readPack(t, pack)
	require.Equal(t, 1, countEntries(t, tmpDir))
}
