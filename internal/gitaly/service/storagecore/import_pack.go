package storagecore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/corestore/gitops"
	"github.com/feder-cr/open_scion/internal/structerr"
)

// buildImportPack writes the pack of changeset 1 of an import: every object
// reachable from the object IDs of refs, the references as the import read
// them. Symbolic references add nothing (their referents are references of
// their own). Without object IDs the pack is empty and no file is created.
//
// The tips are passed explicitly to `git pack-objects --revs --stdout` instead
// of packing what the references reach at the time the pack is built
// (gitops.BuildFullPack, `rev-list --all`): in shadow mode a reference can move
// between the read and the pack, also away and back, and the pack must still
// hold the objects of the values that changeset 1 records.
func buildImportPack(ctx context.Context, repo *localrepo.Repo, refs map[string]corestore.Target, tmpDir string) (_ gitops.Pack, returnedErr error) {
	hash, err := repo.ObjectHash(ctx)
	if err != nil {
		return gitops.Pack{}, fmt.Errorf("object hash: %w", err)
	}

	seen := map[string]struct{}{}
	tips := make([]string, 0, len(refs))
	for name, target := range refs {
		if target.Absent() || target.IsSymbolic() {
			continue
		}
		if err := hash.ValidateHex(string(target)); err != nil {
			return gitops.Pack{}, fmt.Errorf("reference %q: %w", name, err)
		}
		if _, dup := seen[string(target)]; dup {
			continue
		}
		seen[string(target)] = struct{}{}
		tips = append(tips, string(target))
	}
	if len(tips) == 0 {
		return gitops.Pack{}, nil
	}
	sort.Strings(tips)

	var stdin bytes.Buffer
	for _, tip := range tips {
		stdin.WriteString(tip)
		stdin.WriteByte('\n')
	}

	file, err := os.CreateTemp(tmpDir, "graft-import-pack-*")
	if err != nil {
		return gitops.Pack{}, fmt.Errorf("create pack file: %w", err)
	}
	defer func() {
		if returnedErr != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()

	hasher := sha256.New()
	var stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx, gitcmd.Command{
		Name: "pack-objects",
		Flags: []gitcmd.Option{
			gitcmd.Flag{Name: "-q"},
			gitcmd.Flag{Name: "--revs"},
			gitcmd.Flag{Name: "--stdout"},
		},
	},
		gitcmd.WithStdin(&stdin),
		gitcmd.WithStdout(io.MultiWriter(file, hasher)),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return gitops.Pack{}, structerr.New("pack-objects: %w", err).WithMetadata("stderr", stderr.String())
	}

	info, err := file.Stat()
	if err != nil {
		return gitops.Pack{}, fmt.Errorf("stat pack file: %w", err)
	}
	var header [12]byte
	if _, err := file.ReadAt(header[:], 0); err != nil {
		return gitops.Pack{}, fmt.Errorf("read pack header: %w", err)
	}
	if string(header[:4]) != "PACK" {
		return gitops.Pack{}, errors.New("pack-objects wrote no pack header")
	}
	if err := file.Close(); err != nil {
		return gitops.Pack{}, fmt.Errorf("close pack file: %w", err)
	}

	var sum [32]byte
	copy(sum[:], hasher.Sum(nil))
	return gitops.Pack{
		Path:    file.Name(),
		SHA256:  sum,
		Size:    info.Size(),
		Objects: int(binary.BigEndian.Uint32(header[8:12])),
	}, nil
}
