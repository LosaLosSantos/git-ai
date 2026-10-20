package bench

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// CloneDir returns the directory of the clone of a corpus repository under root.
func CloneDir(root, name string) string {
	return filepath.Join(root, strings.ReplaceAll(name, "/", "__")+".git")
}

// EnsureClone makes a bare clone of repo under root unless one exists, and makes sure that it has
// the pinned commit. The clone is complete, not partial: replaying merges reads the blobs of
// every changed file, which a partial clone would fetch one at a time.
func EnsureClone(ctx context.Context, root string, repo Repo) (string, error) {
	dir := CloneDir(root, repo.Name)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return "", err
		}
		// The clone is made under a temporary name and renamed once complete, so an interrupted
		// clone is never taken for a complete one; a left-over temporary clone is started again.
		tmp := dir + ".partial"
		if err := os.RemoveAll(tmp); err != nil {
			return "", err
		}
		g := gitRepo{dir: root}
		if _, _, err := g.run(ctx, nil, nil, "clone", "--bare", "--quiet", repo.URL, tmp); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, dir); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}

	g := gitRepo{dir: dir}
	if _, code, err := g.run(ctx, nil, []int{1, 128}, "cat-file", "-e", repo.Commit+"^{commit}"); err != nil {
		return "", err
	} else if code != 0 {
		if _, _, err := g.run(ctx, nil, nil, "fetch", "--quiet", "origin", repo.Commit); err != nil {
			return "", fmt.Errorf("fetching the pinned commit of %s: %w", repo.Name, err)
		}
	}
	return dir, nil
}
