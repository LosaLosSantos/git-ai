package storagecore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// verifyChangesetBatch is how many changesets checkStoredObjects lists at once.
const verifyChangesetBatch = 500

// checkStoredObjects checks that the object storage can rebuild the database
// state of the repository: it indexes the pack of every changeset up to
// headSeq into a new empty repository in the storage's temporary directory (a
// dry hydration, nothing is installed) and checks that every object reachable
// from the object IDs of dbRefs is there (`git rev-list --objects --quiet`).
//
// The local connectivity check cannot see this: a git write that shadow mode
// did not record leaves objects that are in the local repository but in no
// pack, and once a later write recorded a reference on top of them the
// references of git and of the database agree while a hydration on another
// node fails. It returns whether the check passed and, when it did not, what
// is missing; an error only when the check could not run.
func (s *server) checkStoredObjects(ctx context.Context, core *corestore.Core, repo *gitalypb.Repository, record corestore.Repository, dbRefs map[string]corestore.Target, headSeq uint64) (bool, string, error) {
	storageName := repo.GetStorageName()
	storagePath, err := s.deps.GetLocator().GetStorageByName(ctx, storageName)
	if err != nil {
		return false, "", err
	}
	tmpDir, err := s.deps.GetLocator().TempDir(storageName)
	if err != nil {
		return false, "", structerr.NewInternal("temporary directory: %w", err)
	}
	if err := os.MkdirAll(tmpDir, mode.Directory); err != nil {
		return false, "", structerr.NewInternal("create temporary directory: %w", err)
	}
	build, err := os.MkdirTemp(tmpDir, "graft-verify-*")
	if err != nil {
		return false, "", structerr.NewInternal("create verification directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(build) }()

	buildPath := filepath.Join(build, "repo.git")
	if err := gitops.InitBare(ctx, s.deps.GetGitCmdFactory(), buildPath, record.ObjectFormat); err != nil {
		return false, "", structerr.NewInternal("init verification repository: %w", err)
	}
	rel, err := filepath.Rel(storagePath, buildPath)
	if err != nil {
		return false, "", structerr.NewInternal("relative verification path: %w", err)
	}
	stored := s.localRepo(&gitalypb.Repository{StorageName: storageName, RelativePath: rel})

	for after := uint64(0); after < headSeq; {
		upTo := min(after+verifyChangesetBatch, headSeq)
		changesets, err := core.Meta.ListChangesets(ctx, record.ID, after, upTo)
		if err != nil {
			return false, "", structerr.NewUnavailable("storage core: list changesets: %w", err)
		}
		if len(changesets) == 0 {
			return false, fmt.Sprintf("the database has no changesets after %d up to %d", after, upTo), nil
		}

		for _, cs := range changesets {
			after = cs.Seq
			if cs.PackKey == "" {
				continue
			}
			if id, _, ok := corestore.ParsePackKey(cs.PackKey); !ok || id != record.ID {
				return false, fmt.Sprintf("changeset %d references %q, which is not a pack of the repository", cs.Seq, cs.PackKey), nil
			}

			problem, err := indexStoredPack(ctx, core, stored, cs)
			if err != nil || problem != "" {
				return false, problem, err
			}
		}
	}

	var stdin bytes.Buffer
	for _, target := range dbRefs {
		if target.Absent() || target.IsSymbolic() {
			continue
		}
		stdin.WriteString(string(target))
		stdin.WriteByte('\n')
	}
	if stdin.Len() == 0 {
		return true, "", nil
	}

	var stderr bytes.Buffer
	err = stored.ExecAndWait(ctx, gitcmd.Command{
		Name: "rev-list",
		Flags: []gitcmd.Option{
			gitcmd.Flag{Name: "--objects"},
			gitcmd.Flag{Name: "--quiet"},
			gitcmd.Flag{Name: "--stdin"},
		},
	}, gitcmd.WithStdin(&stdin), gitcmd.WithStderr(&stderr))
	if err == nil {
		return true, "", nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, "", structerr.NewUnavailable("stored objects check: %w", ctxErr)
	}
	if _, exited := command.ExitStatus(err); !exited {
		return false, "", structerr.NewInternal("stored objects check: %w", err)
	}

	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	return false, "the packs of the database do not hold every object of its references: " + msg, nil
}

// indexStoredPack downloads the pack of cs (whose key was checked) and indexes
// it into repo. It returns a description of a missing or corrupt pack, or an
// error when the check could not run.
func indexStoredPack(ctx context.Context, core *corestore.Core, repo *localrepo.Repo, cs corestore.Changeset) (string, error) {
	rc, err := core.Blobs.Get(ctx, cs.PackKey)
	if errors.Is(err, corestore.ErrBlobNotFound) {
		return fmt.Sprintf("the pack %q of changeset %d is missing from the object storage", cs.PackKey, cs.Seq), nil
	}
	if err != nil {
		return "", structerr.NewUnavailable("storage core: download pack: %w", err)
	}
	defer func() { _ = rc.Close() }()

	if err := gitops.IndexPack(ctx, repo, rc); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", structerr.NewUnavailable("stored objects check: %w", ctxErr)
		}
		return fmt.Sprintf("the pack %q of changeset %d cannot be indexed: %v", cs.PackKey, cs.Seq, err), nil
	}
	return "", nil
}
