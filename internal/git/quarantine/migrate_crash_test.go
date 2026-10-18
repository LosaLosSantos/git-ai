package quarantine

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// TestMigrateCrashChild is the child process of TestMigrateCrashLeavesAPrefixOfTheLinks. It migrates
// the quarantine it is given and kills itself right before the link that it is told.
func TestMigrateCrashChild(t *testing.T) {
	source := os.Getenv("QUARANTINE_CRASH_SOURCE")
	if source == "" {
		t.Skip("only runs as the child of TestMigrateCrashLeavesAPrefixOfTheLinks")
	}
	target := os.Getenv("QUARANTINE_CRASH_TARGET")
	crashAt, err := strconv.Atoi(os.Getenv("QUARANTINE_CRASH_AT"))
	require.NoError(t, err)

	links := 0
	beforeLink = func(string) {
		if links == crashAt {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		}
		links++
	}

	require.NoError(t, migrate(context.Background(), source, target, false))
}

// copyTree copies the directory tree at from to to.
func copyTree(t *testing.T, from, to string) {
	t.Helper()

	require.NoError(t, filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), data, 0o444)
	}))
}

// TestMigrateCrashLeavesAPrefixOfTheLinks kills a migration before each of its links in turn, in a
// child process, and checks what the repository holds afterwards: the names that the migration
// linked before the crash, in link order, so a pack index never without its pack, and git fsck
// finds every object it holds intact. A crash of the process leaves the page cache; a power cut,
// which the filesystem assumption of migrate covers, cannot be simulated here.
func TestMigrateCrashLeavesAPrefixOfTheLinks(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	// A quarantine with loose objects and a pack, written by git.
	_, scratchPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	packed := gittest.WriteCommit(t, cfg, scratchPath, gittest.WithMessage("packed"))
	gittest.ExecOpts(t, cfg, gittest.ExecConfig{Stdin: strings.NewReader(packed.String() + "\n")},
		"-C", scratchPath, "pack-objects", "-q", "--revs", filepath.Join(scratchPath, "objects", "pack", "pack"))
	gittest.Exec(t, cfg, "-C", scratchPath, "prune-packed")
	gittest.WriteCommit(t, cfg, scratchPath, gittest.WithMessage("loose"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "loose", Content: "loose\n"}))
	quarantine := filepath.Join(t.TempDir(), "quarantine")
	copyTree(t, filepath.Join(scratchPath, "objects"), quarantine)

	// The order of the links, from a migration that completes.
	var order []string
	originalBefore := beforeLink
	t.Cleanup(func() { beforeLink = originalBefore })
	beforeLink = func(targetPath string) { order = append(order, targetPath) }
	fullTarget := filepath.Join(t.TempDir(), "objects")
	require.NoError(t, os.MkdirAll(fullTarget, 0o755))
	source := filepath.Join(t.TempDir(), "source")
	copyTree(t, quarantine, source)
	require.NoError(t, migrate(ctx, source, fullTarget, false))
	beforeLink = originalBefore

	var relOrder []string
	for _, path := range order {
		rel, err := filepath.Rel(fullTarget, path)
		require.NoError(t, err)
		relOrder = append(relOrder, rel)
	}
	var idx, pack int = -1, -1
	for i, rel := range relOrder {
		switch {
		case strings.HasSuffix(rel, ".pack"):
			pack = i
		case strings.HasSuffix(rel, ".idx"):
			idx = i
		}
	}
	require.True(t, pack >= 0 && idx > pack, "the pack is linked before its index: %v", relOrder)

	for crashAt := 0; crashAt <= len(relOrder); crashAt++ {
		_, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
		target := filepath.Join(repoPath, "objects")
		source := filepath.Join(t.TempDir(), "source")
		copyTree(t, quarantine, source)

		cmd := exec.Command(os.Args[0], "-test.run", "^TestMigrateCrashChild$")
		cmd.Env = append(os.Environ(),
			"QUARANTINE_CRASH_SOURCE="+source,
			"QUARANTINE_CRASH_TARGET="+target,
			"QUARANTINE_CRASH_AT="+strconv.Itoa(crashAt),
		)
		output, err := cmd.CombinedOutput()
		if crashAt < len(relOrder) {
			require.Error(t, err, "the child is killed before link %d", crashAt)
		} else {
			require.NoError(t, err, "%s", output)
		}

		for i, rel := range relOrder {
			_, statErr := os.Stat(filepath.Join(target, rel))
			if i < crashAt {
				require.NoError(t, statErr, "crash at %d: %s linked before the crash", crashAt, rel)
			} else {
				require.ErrorIs(t, statErr, fs.ErrNotExist, "crash at %d: %s linked after the crash", crashAt, rel)
			}
		}

		gittest.Exec(t, cfg, "-C", repoPath, "fsck", "--no-dangling")
	}
}
