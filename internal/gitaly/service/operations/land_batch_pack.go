package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
)

// packBatchObjects packs the objects of a batch attempt before its publish. It is
// packAttemptObjects, and a variable so that tests can make it fail.
var packBatchObjects = packAttemptObjects

// packAttemptObjects packs every loose object of objectDir, the object directory of the quarantine of
// a batch attempt, into one pack in that directory, and then removes the loose files and the fan-out
// directories they leave empty. It returns the number of packed objects; 0 means that the quarantine
// holds no loose object, and nothing is done.
//
// This is how a batch makes its objects durable once (doc/graft/PLAN8.md, section 3.2): the rebaser
// writes them without fsync, git pack-objects writes and fsyncs the pack and its index with the
// fsync configuration of the server, and the quarantine migration of the update then moves only the
// pack files, fsyncing them again, before the reference is updated. The loose files are removed only
// after git pack-objects succeeded; on any error they are left as they are, and the attempt, whose
// quarantine is discarded, publishes nothing. A file that is not a loose object, such as a pack that
// a helper wrote, is left for the migration, which fsyncs it.
//
// objectDir must be a quarantine object directory that only the attempt writes to: the files are
// listed once, so an object written while the pack is built would stay loose.
func packAttemptObjects(ctx context.Context, repo *localrepo.Repo, objectDir string) (int, error) {
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return 0, fmt.Errorf("detecting object hash: %w", err)
	}

	entries, err := os.ReadDir(objectDir)
	if err != nil {
		return 0, fmt.Errorf("reading the object directory: %w", err)
	}

	var objectIDs, files, fanouts []string
	for _, entry := range entries {
		if !entry.IsDir() || !isFanoutName(entry.Name()) {
			continue
		}

		fanout := filepath.Join(objectDir, entry.Name())
		objects, err := os.ReadDir(fanout)
		if err != nil {
			return 0, fmt.Errorf("reading fan-out directory: %w", err)
		}

		for _, object := range objects {
			if object.IsDir() {
				continue
			}

			// A loose object is named by the rest of its hex object ID. Anything else, such as the
			// temporary file of a writer that died, is not an object and stays for the migration.
			id, err := objectHash.FromHex(entry.Name() + object.Name())
			if err != nil {
				continue
			}

			objectIDs = append(objectIDs, id.String())
			files = append(files, filepath.Join(fanout, object.Name()))
		}
		fanouts = append(fanouts, fanout)
	}

	if len(objectIDs) == 0 {
		return 0, nil
	}

	packDir := filepath.Join(objectDir, "pack")
	if err := os.MkdirAll(packDir, mode.Directory); err != nil {
		return 0, fmt.Errorf("creating the pack directory: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "pack-objects",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "-q"}},
			Args:  []string{filepath.Join(packDir, "pack")},
		},
		gitcmd.WithStdin(strings.NewReader(strings.Join(objectIDs, "\n")+"\n")),
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return 0, fmt.Errorf("pack-objects: %w, stderr: %q", err, stderr.String())
	}

	for _, file := range files {
		if err := os.Remove(file); err != nil {
			return 0, fmt.Errorf("removing packed loose object: %w", err)
		}
	}

	// A fan-out directory left in the quarantine would make the migration create it in the
	// repository, where housekeeping may remove it while the migration still uses it. One that
	// still holds a file that is not an object stays.
	for _, fanout := range fanouts {
		if err := os.Remove(fanout); err != nil && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, fs.ErrExist) {
			return 0, fmt.Errorf("removing emptied fan-out directory: %w", err)
		}
	}

	return len(objectIDs), nil
}

// isFanoutName reports whether name is the name of a loose-object fan-out directory: two lower-case
// hex digits.
func isFanoutName(name string) bool {
	if len(name) != 2 {
		return false
	}

	for _, c := range name {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}

	return true
}
