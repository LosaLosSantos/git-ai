package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// gitRepo runs git in one clone of the corpus.
type gitRepo struct {
	dir string
	// bin is the git program, "git" by default.
	bin string
}

// run runs git with args and returns its stdout. A non-zero exit is an error that carries stderr,
// unless the exit code is in okCodes, in which case it is returned as code.
func (g gitRepo) run(ctx context.Context, stdin []byte, okCodes []int, args ...string) ([]byte, int, error) {
	bin := g.bin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"-C", g.dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		for _, ok := range okCodes {
			if exit.ExitCode() == ok {
				return stdout.Bytes(), ok, nil
			}
		}
	}
	return nil, 0, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
}

// merges returns the merge commits reachable from rev with exactly two parents, newest first,
// at most max of them (0 means no limit), each with its parents.
func (g gitRepo) merges(ctx context.Context, rev string, max int) ([][3]string, error) {
	args := []string{"rev-list", "--merges", "--parents"}
	if max > 0 {
		args = append(args, fmt.Sprintf("--max-count=%d", max))
	}
	out, _, err := g.run(ctx, nil, nil, append(args, rev)...)
	if err != nil {
		return nil, err
	}

	var merges [][3]string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			// Octopus merges are not replayed: the resolver merges two sides.
			continue
		}
		merges = append(merges, [3]string{fields[0], fields[1], fields[2]})
	}
	return merges, nil
}

// mergeBases returns the merge bases of a and b.
func (g gitRepo) mergeBases(ctx context.Context, a, b string) ([]string, error) {
	out, code, err := g.run(ctx, nil, []int{1}, "merge-base", "--all", a, b)
	if err != nil {
		return nil, err
	}
	if code == 1 {
		return nil, nil
	}
	return strings.Fields(string(out)), nil
}

// conflicts replays the merge of ours and theirs and returns the paths that conflict, as git
// merge-tree --write-tree reports them.
func (g gitRepo) conflicts(ctx context.Context, ours, theirs string) ([]string, error) {
	out, code, err := g.run(ctx, nil, []int{1}, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", ours, theirs)
	if err != nil {
		return nil, err
	}
	if code == 0 {
		return nil, nil
	}

	// The output is the tree OID, then the conflicted paths, each terminated by a NUL; an empty
	// entry ends the list.
	parts := strings.Split(string(out), "\x00")
	var paths []string
	seen := map[string]bool{}
	for _, p := range parts[1:] {
		if p == "" {
			break
		}
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// entry is a blob of a tree: its object ID and mode.
type entry struct {
	OID  string `json:"oid"`
	Mode string `json:"mode"`
}

// entryAt returns the blob at path in the tree of rev, or nil when the path is absent or not a
// blob.
func (g gitRepo) entryAt(ctx context.Context, rev, path string) (*entry, error) {
	out, _, err := g.run(ctx, nil, nil, "ls-tree", "-z", rev, "--", path)
	if err != nil {
		return nil, err
	}
	line := strings.TrimSuffix(string(out), "\x00")
	if line == "" {
		return nil, nil
	}
	// "<mode> SP <type> SP <oid> TAB <path>"
	meta, name, ok := strings.Cut(line, "\t")
	if !ok || name != path {
		return nil, nil
	}
	fields := strings.Fields(meta)
	if len(fields) != 3 || fields[1] != "blob" {
		return nil, nil
	}
	return &entry{OID: fields[2], Mode: fields[0]}, nil
}

// blob returns the content of a blob.
func (g gitRepo) blob(ctx context.Context, oid string) ([]byte, error) {
	out, _, err := g.run(ctx, nil, nil, "cat-file", "blob", oid)
	return out, err
}

// message returns the full commit message of a commit.
func (g gitRepo) message(ctx context.Context, oid string) (string, error) {
	out, _, err := g.run(ctx, nil, nil, "log", "-1", "--format=%B", oid)
	return strings.TrimRight(string(out), "\n"), err
}
